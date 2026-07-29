package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EmbedDim is the embedding width. It MUST match the FLOAT[...] width declared
// in memorySchemaSQL — a mismatch is a schema migration, not a re-embed, which
// is why memory_meta records the dimension and startup validates it.
// TestSchemaDimensionMatchesConstant asserts the two agree.
const EmbedDim = 384

// Memory schema. Appended to schemaSQL so it is created idempotently on every
// Open, alongside the existing session tables.
//
// Note on memory_sources.session_id: the design sketch keyed conversation
// sources by a composite source_ref ("<session>#<seq>-<seq>"), but matching that
// by prefix during a cascade delete is fragile. An explicit session_id column
// makes the cascade a single indexed predicate and covers artifact-derived
// sources too. Directory sources leave it empty, which is exactly why directory
// memory survives session deletion.
const memorySchemaSQL = `
CREATE TABLE IF NOT EXISTS memory_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_sources (
	id           TEXT PRIMARY KEY,
	source_type  TEXT NOT NULL,          -- conversation|artifact|directory
	source_ref   TEXT NOT NULL,
	session_id   TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL DEFAULT '',
	path         TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '',
	mtime        TIMESTAMP,
	ingested_at  TIMESTAMP NOT NULL,
	token_count  INTEGER NOT NULL DEFAULT 0,
	entity_pass  TEXT NOT NULL DEFAULT 'pending'  -- pending|done|failed
);
CREATE TABLE IF NOT EXISTS memory_chunks (
	id           TEXT PRIMARY KEY,
	source_id    TEXT NOT NULL,
	ord          INTEGER NOT NULL,
	text         TEXT NOT NULL,
	heading_path TEXT NOT NULL DEFAULT '',
	token_count  INTEGER NOT NULL DEFAULT 0,
	char_len     INTEGER NOT NULL DEFAULT 0,
	embedding    FLOAT[384],
	embed_model  TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_terms (
	term TEXT PRIMARY KEY,
	df   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS memory_chunk_terms (
	chunk_id TEXT NOT NULL,
	term     TEXT NOT NULL,
	tf       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chunk_id, term)
);
CREATE TABLE IF NOT EXISTS memory_entities (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,   -- person|org|date|path|code|number|tag
	value_norm TEXT NOT NULL,
	UNIQUE (kind, value_norm)
);
CREATE TABLE IF NOT EXISTS memory_chunk_entities (
	chunk_id  TEXT NOT NULL,
	entity_id TEXT NOT NULL,
	count     INTEGER NOT NULL DEFAULT 1,
	extractor TEXT NOT NULL DEFAULT 'heuristic',  -- heuristic|tag|llm
	PRIMARY KEY (chunk_id, entity_id)
);
CREATE TABLE IF NOT EXISTS memory_edges (
	src_chunk_id TEXT NOT NULL,
	dst_chunk_id TEXT NOT NULL,
	kind         TEXT NOT NULL,  -- next|prev|similar|entity|link|inferred_link
	weight       DOUBLE NOT NULL DEFAULT 0,
	PRIMARY KEY (src_chunk_id, dst_chunk_id, kind)
);
CREATE TABLE IF NOT EXISTS memory_links (
	from_ref TEXT NOT NULL,
	to_ref   TEXT NOT NULL,
	heading  TEXT NOT NULL DEFAULT '',
	raw      TEXT NOT NULL DEFAULT '',
	embed    BOOLEAN NOT NULL DEFAULT false,
	PRIMARY KEY (from_ref, to_ref, heading, raw)
);
CREATE INDEX IF NOT EXISTS idx_memory_links_to ON memory_links (to_ref);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_source ON memory_chunks (source_id);
CREATE INDEX IF NOT EXISTS idx_memory_sources_session ON memory_sources (session_id);
CREATE INDEX IF NOT EXISTS idx_memory_sources_ref ON memory_sources (source_type, source_ref);
CREATE INDEX IF NOT EXISTS idx_memory_chunk_terms_term ON memory_chunk_terms (term);
CREATE INDEX IF NOT EXISTS idx_memory_chunk_entities_entity ON memory_chunk_entities (entity_id);
CREATE INDEX IF NOT EXISTS idx_memory_edges_dst ON memory_edges (dst_chunk_id);
`

// memoryMigrationSQL brings an existing database up to the current schema.
//
// `CREATE TABLE IF NOT EXISTS` cannot add a column to a table that already
// exists, so every column added after the first release needs an explicit ALTER.
// `ADD COLUMN IF NOT EXISTS` is idempotent in DuckDB 1.4.1 — verified directly
// rather than assumed — so this runs unconditionally on every Open and needs no
// version comparison.
//
// Added in schema version 2 (Phase 7):
//   - memory_sources.file_size, so an unchanged file can be skipped from its
//     (mtime, size) alone, without being read and hashed.
//   - memory_links.kind, so a model-proposed cross-reference is recorded alongside an
//     authored one and survives re-ingest the same way (added in Phase 8; the column
//     defaults to 'link' so existing rows keep their meaning).
//   - memory_chunks.thread_context / cot_context, which are included in the
//     *embedded* text but never in the text handed back. This is what implements
//     §3.3's indexed-but-not-returned rule for CoT: the backfill embeds from the
//     stored row, so the prefix has to live somewhere the backfill can see, and it
//     must not be a field any read path returns.
const memoryMigrationSQL = `
ALTER TABLE memory_sources ADD COLUMN IF NOT EXISTS file_size BIGINT DEFAULT 0;
ALTER TABLE memory_chunks  ADD COLUMN IF NOT EXISTS thread_context TEXT DEFAULT '';
ALTER TABLE memory_chunks  ADD COLUMN IF NOT EXISTS cot_context TEXT DEFAULT '';
ALTER TABLE memory_links   ADD COLUMN IF NOT EXISTS kind TEXT DEFAULT 'link';
`

// Memory metadata keys.
const (
	MetaSchemaVersion = "schema_version"
	MetaEmbedModel    = "embed_model"
	MetaEmbedDims     = "embed_dims"
	MetaModelRevision = "model_revision"
	MetaModelDigest   = "model_digest"
	MetaStatsDirty    = "stats_dirty"
	MetaStatsN        = "bm25_n"
	MetaStatsAvgDL    = "bm25_avgdl"
)

// MemorySchemaVersion gates future migrations of the memory tables.
const MemorySchemaVersion = "2"

// Source types.
const (
	SourceConversation = "conversation"
	SourceArtifact     = "artifact"
	SourceDirectory    = "directory"
)

// Entity-pass states.
const (
	EntityPassPending = "pending"
	EntityPassDone    = "done"
	EntityPassFailed  = "failed"
)

// MemorySource is one ingested unit of provenance: a note, an artifact, or one
// conversation turn pair.
type MemorySource struct {
	ID          string `json:"id"`
	SourceType  string `json:"sourceType"`
	SourceRef   string `json:"sourceRef"`
	SessionID   string `json:"sessionId"`
	Title       string `json:"title"`
	Path        string `json:"path"`
	ContentHash string `json:"contentHash"`
	MTime       string `json:"mtime"`
	// FileSize is the source file's byte length, paired with MTime so an unchanged
	// file can be skipped without being read. Zero for non-file sources.
	FileSize   int64  `json:"fileSize"`
	IngestedAt string `json:"ingestedAt"`
	TokenCount int    `json:"tokenCount"`
	EntityPass string `json:"entityPass"`
}

// MemoryChunk is one retrievable span. Embedding is nil until the embedder has
// run, which is what lets ingestion complete without a provisioned model.
type MemoryChunk struct {
	ID          string    `json:"id"`
	SourceID    string    `json:"sourceId"`
	Ord         int       `json:"ord"`
	Text        string    `json:"text"`
	HeadingPath string    `json:"headingPath"`
	TokenCount  int       `json:"tokenCount"`
	CharLen     int       `json:"charLen"`
	Embedding   []float32 `json:"-"`
	EmbedModel  string    `json:"embedModel"`
	CreatedAt   string    `json:"createdAt"`
	// ThreadContext and CotContext are prepended to the *embedded* text and never
	// returned to the model. ThreadContext situates a conversation chunk (session
	// title plus the previous turn's gist); CotContext carries the turn's reasoning
	// note, which is indexed so terse turns are findable but withheld from output
	// because it can be wrong and because ARCHITECTURE.md deliberately keeps the
	// model's own past reasoning out of replayed history (§3.3).
	ThreadContext string `json:"-"`
	CotContext    string `json:"-"`
	// Terms and Entities are write-side only: populated by the ingester and
	// persisted alongside the chunk, not read back by GetChunk.
	Terms    map[string]int `json:"-"`
	Entities []ChunkEntity  `json:"-"`
}

// ChunkEntity associates a chunk with an entity, recording which extractor
// produced the association so the LLM tier can be reweighted or rolled back.
type ChunkEntity struct {
	Kind      string
	ValueNorm string
	Count     int
	Extractor string
}

// MemoryEdge connects two chunks. Kind is one of next|prev|similar|entity|
// link|inferred_link.
type MemoryEdge struct {
	SrcChunkID string  `json:"src"`
	DstChunkID string  `json:"dst"`
	Kind       string  `json:"kind"`
	Weight     float64 `json:"weight"`
}

// MemoryStats is a corpus summary for the UI and for BM25 scoring.
type MemoryStats struct {
	Sources         int     `json:"sources"`
	Chunks          int     `json:"chunks"`
	EmbeddedChunks  int     `json:"embeddedChunks"`
	Terms           int     `json:"terms"`
	Entities        int     `json:"entities"`
	Edges           int     `json:"edges"`
	PendingEntities int     `json:"pendingEntities"`
	AvgDL           float64 `json:"avgDocLength"`
	EmbedModel      string  `json:"embedModel"`
	EmbedDims       int     `json:"embedDims"`
}

// ---------- metadata ----------

// SetMeta upserts one memory_meta key.
func (s *Store) SetMeta(key, value string) error {
	defer s.lockWrites()()
	_, err := s.db.Exec(
		`INSERT INTO memory_meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	return nil
}

// GetMeta returns a memory_meta value, or "" if absent.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM memory_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta %s: %w", key, err)
	}
	return v, nil
}

// InitMemoryMeta records the schema version and the embedding model identity.
// Returns changed=true when the model or dimension differs from what is already
// stored, which means existing vectors were produced by a different model and
// cannot be compared with new ones — never silently mix vector spaces.
func (s *Store) InitMemoryMeta(embedModel string, dims int) (changed bool, err error) {
	if err := s.SetMeta(MetaSchemaVersion, MemorySchemaVersion); err != nil {
		return false, err
	}
	prevModel, err := s.GetMeta(MetaEmbedModel)
	if err != nil {
		return false, err
	}
	prevDims, err := s.GetMeta(MetaEmbedDims)
	if err != nil {
		return false, err
	}
	wantDims := fmt.Sprintf("%d", dims)
	changed = (prevModel != "" && prevModel != embedModel) || (prevDims != "" && prevDims != wantDims)

	if err := s.SetMeta(MetaEmbedModel, embedModel); err != nil {
		return changed, err
	}
	if err := s.SetMeta(MetaEmbedDims, wantDims); err != nil {
		return changed, err
	}
	return changed, nil
}

// ---------- sources ----------

// FindSource looks up a source by its natural key. Used by incremental
// re-ingest to decide whether anything actually changed.
func (s *Store) FindSource(sourceType, sourceRef string) (MemorySource, bool, error) {
	var src MemorySource
	var mtime sql.NullString
	err := s.db.QueryRow(`
		SELECT id, source_type, source_ref, session_id, title, path, content_hash,
		       mtime, file_size, ingested_at, token_count, entity_pass
		FROM memory_sources WHERE source_type = ? AND source_ref = ?`,
		sourceType, sourceRef,
	).Scan(&src.ID, &src.SourceType, &src.SourceRef, &src.SessionID, &src.Title,
		&src.Path, &src.ContentHash, &mtime, &src.FileSize, &src.IngestedAt,
		&src.TokenCount, &src.EntityPass)
	if err == sql.ErrNoRows {
		return MemorySource{}, false, nil
	}
	if err != nil {
		return MemorySource{}, false, fmt.Errorf("find source: %w", err)
	}
	src.MTime = mtime.String
	return src, true, nil
}

// ListSources returns every source, newest ingest first. Used by the edge passes
// that need to walk the whole corpus and by the UI's corpus view.
func (s *Store) ListSources() ([]MemorySource, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, source_ref, session_id, title, path, content_hash,
		       mtime, file_size, ingested_at, token_count, entity_pass
		FROM memory_sources ORDER BY ingested_at DESC, source_ref`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	out := make([]MemorySource, 0)
	for rows.Next() {
		var src MemorySource
		var mtime sql.NullString
		if err := rows.Scan(&src.ID, &src.SourceType, &src.SourceRef, &src.SessionID,
			&src.Title, &src.Path, &src.ContentHash, &mtime, &src.FileSize,
			&src.IngestedAt, &src.TokenCount, &src.EntityPass); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.MTime = mtime.String
		out = append(out, src)
	}
	return out, rows.Err()
}

// ReplaceSource writes a source and its chunks, replacing any previous content
// for the same (source_type, source_ref). Terms, entities and their links are
// persisted with the chunks; edges are not (they are cross-source and built in a
// later pass).
//
// This runs as a single transaction. That is safe as of DuckDB 1.4.1, where the
// ART index no longer rejects deleting and reinserting the same primary key
// within one transaction — verified by TestARTDeleteReinsertSameTx.
func (s *Store) ReplaceSource(src MemorySource, chunks []MemoryChunk) (string, error) {
	defer s.lockWrites()()
	if src.SourceType == "" || src.SourceRef == "" {
		return "", fmt.Errorf("source_type and source_ref are required")
	}

	existing, found, err := s.FindSource(src.SourceType, src.SourceRef)
	if err != nil {
		return "", err
	}
	sourceID := src.ID
	if sourceID == "" {
		if found {
			sourceID = existing.ID
		} else {
			sourceID = uuid.New().String()
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if found {
		if err := deleteChunksForSourceTx(tx, existing.ID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`DELETE FROM memory_sources WHERE id = ?`, existing.ID); err != nil {
			return "", fmt.Errorf("delete source row: %w", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if src.IngestedAt == "" {
		src.IngestedAt = now
	}
	if src.EntityPass == "" {
		src.EntityPass = EntityPassPending
	}
	total := 0
	for _, c := range chunks {
		total += c.TokenCount
	}
	if src.TokenCount == 0 {
		src.TokenCount = total
	}

	var mtime any
	if src.MTime != "" {
		mtime = src.MTime
	}
	if _, err := tx.Exec(`
		INSERT INTO memory_sources
			(id, source_type, source_ref, session_id, title, path, content_hash,
			 mtime, file_size, ingested_at, token_count, entity_pass)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, src.SourceType, src.SourceRef, src.SessionID, src.Title, src.Path,
		src.ContentHash, mtime, src.FileSize, src.IngestedAt, src.TokenCount, src.EntityPass,
	); err != nil {
		return "", fmt.Errorf("insert source: %w", err)
	}

	for i := range chunks {
		c := &chunks[i]
		if c.ID == "" {
			c.ID = uuid.New().String()
		}
		c.SourceID = sourceID
		if c.Ord == 0 {
			c.Ord = i + 1
		}
		if c.CharLen == 0 {
			c.CharLen = len(c.Text)
		}
		if c.CreatedAt == "" {
			c.CreatedAt = now
		}
	}
	if err := insertChunksTx(tx, chunks); err != nil {
		return "", err
	}
	// Terms and entity links are written in batches rather than per row. DuckDB
	// is columnar and its per-statement overhead dominates single-row inserts:
	// a 219-chunk corpus produces ~20k term rows, which cost ~4.8 ms each as
	// individual statements — 98 seconds of a 98-second ingest. Batched, the same
	// work is a few dozen statements.
	if err := insertChunkTermsTx(tx, chunks); err != nil {
		return "", err
	}
	if err := insertChunkEntitiesTx(tx, chunks, false); err != nil {
		return "", err
	}

	if err := markStatsDirtyTx(tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit source: %w", err)
	}
	return sourceID, nil
}

// insertChunksTx writes chunk rows, batching the common case.
//
// Ingestion produces chunks with no vector (Phase 3's embedder fills them in via
// SetChunkEmbeddings), so the NULL-embedding path is the hot one and is batched.
// A chunk that arrives with a vector needs the ?::FLOAT[384] cast and is written
// individually — rare enough that the per-statement cost does not matter.
func insertChunksTx(tx *sql.Tx, chunks []MemoryChunk) error {
	var plain []MemoryChunk
	for _, c := range chunks {
		if len(c.Embedding) == 0 {
			plain = append(plain, c)
			continue
		}
		if len(c.Embedding) != EmbedDim {
			return fmt.Errorf("chunk %s: embedding has %d dims, want %d", c.ID, len(c.Embedding), EmbedDim)
		}
		if _, err := tx.Exec(`
			INSERT INTO memory_chunks
				(id, source_id, ord, text, heading_path, token_count, char_len,
				 thread_context, cot_context, embedding, embed_model, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?::FLOAT[384], ?, ?)`,
			c.ID, c.SourceID, c.Ord, c.Text, c.HeadingPath, c.TokenCount, c.CharLen,
			c.ThreadContext, c.CotContext, vecToAny(c.Embedding), c.EmbedModel, c.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert chunk %s: %w", c.ID, err)
		}
	}

	return execBatches(tx,
		`memory_chunks (id, source_id, ord, text, heading_path, token_count, char_len,
		                thread_context, cot_context, embed_model, created_at)`,
		11, len(plain),
		func(i int) []any {
			c := plain[i]
			return []any{c.ID, c.SourceID, c.Ord, c.Text, c.HeadingPath,
				c.TokenCount, c.CharLen, c.ThreadContext, c.CotContext,
				c.EmbedModel, c.CreatedAt}
		})
}

// batchRows is how many value tuples go into one INSERT. Large enough to amortize
// DuckDB's per-statement cost, small enough to keep the statement and its
// parameter list manageable.
const batchRows = 400

// insertChunkTermsTx writes every chunk's term frequencies in batched multi-row
// INSERTs. No conflict handling is needed: ReplaceSource deletes the source's
// previous chunks first and assigns fresh chunk ids, so (chunk_id, term) cannot
// collide with an existing row, and a chunk's Terms map cannot collide with
// itself.
func insertChunkTermsTx(tx *sql.Tx, chunks []MemoryChunk) error {
	type row struct {
		chunkID string
		term    string
		tf      int
	}
	rows := make([]row, 0, len(chunks)*64)
	for _, c := range chunks {
		for term, tf := range c.Terms {
			if term == "" || tf <= 0 {
				continue
			}
			rows = append(rows, row{c.ID, term, tf})
		}
	}
	return execBatches(tx, "memory_chunk_terms (chunk_id, term, tf)", 3, len(rows),
		func(i int) []any { return []any{rows[i].chunkID, rows[i].term, rows[i].tf} })
}

// insertChunkEntitiesTx resolves every distinct entity across the source in one
// lookup plus one batched insert, then batches the chunk links. Resolving each
// entity individually would reintroduce the per-statement cost this avoids.
//
// additive controls conflict handling on the link rows. The ingestion path deletes a
// source's chunks first and assigns fresh ids, so (chunk_id, entity_id) cannot
// collide and a plain INSERT is correct. The enrichment path writes into chunks that
// already have heuristic associations, where a collision is expected — the LLM naming
// an entity the heuristics also found — and must not fail. When it collides, the
// existing row wins: a heuristic or tag association is at least as trustworthy as an
// LLM one, and overwriting `extractor` would misreport where the association came
// from and break the rollback path.
func insertChunkEntitiesTx(tx *sql.Tx, chunks []MemoryChunk, additive bool) error {
	// Collect distinct (kind, value_norm) pairs.
	type key struct{ kind, value string }
	distinct := make(map[key]bool)
	for _, c := range chunks {
		for _, e := range c.Entities {
			if e.ValueNorm == "" {
				continue
			}
			distinct[key{e.Kind, e.ValueNorm}] = true
		}
	}
	if len(distinct) == 0 {
		return nil
	}

	values := make([]string, 0, len(distinct))
	for k := range distinct {
		values = append(values, k.value)
	}

	// One lookup for everything that already exists. Filtering by value_norm and
	// matching kind in Go avoids composite-IN portability concerns.
	existing := make(map[key]string, len(distinct))
	for start := 0; start < len(values); start += batchRows {
		end := min(start+batchRows, len(values))
		batch := values[start:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, v := range batch {
			ph[i] = "?"
			args[i] = v
		}
		rows, err := tx.Query(
			`SELECT id, kind, value_norm FROM memory_entities WHERE value_norm IN (`+
				strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return fmt.Errorf("lookup entities: %w", err)
		}
		for rows.Next() {
			var id, kind, vn string
			if err := rows.Scan(&id, &kind, &vn); err != nil {
				rows.Close()
				return err
			}
			existing[key{kind, vn}] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	// Insert the ones that are new, assigning ids up front.
	type newEnt struct {
		id, kind, value string
	}
	var fresh []newEnt
	for k := range distinct {
		if _, ok := existing[k]; ok {
			continue
		}
		id := uuid.New().String()
		existing[k] = id
		fresh = append(fresh, newEnt{id, k.kind, k.value})
	}
	if err := execBatches(tx, "memory_entities (id, kind, value_norm)", 3, len(fresh),
		func(i int) []any { return []any{fresh[i].id, fresh[i].kind, fresh[i].value} }); err != nil {
		return err
	}

	// Link rows. A chunk may list the same entity twice only if the extractor
	// produced duplicates, which ExtractEntities already collapses; dedupe here
	// anyway so a caller cannot trip the primary key.
	type link struct {
		chunkID, entityID, extractor string
		count                        int
	}
	seen := make(map[[2]string]bool)
	var links []link
	for _, c := range chunks {
		for _, e := range c.Entities {
			if e.ValueNorm == "" {
				continue
			}
			id := existing[key{e.Kind, e.ValueNorm}]
			pair := [2]string{c.ID, id}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			count := e.Count
			if count <= 0 {
				count = 1
			}
			extractor := e.Extractor
			if extractor == "" {
				extractor = "heuristic"
			}
			links = append(links, link{c.ID, id, extractor, count})
		}
	}
	target := "memory_chunk_entities (chunk_id, entity_id, count, extractor)"
	argsAt := func(i int) []any {
		return []any{links[i].chunkID, links[i].entityID, links[i].count, links[i].extractor}
	}
	if additive {
		return execBatchesOnConflict(tx, target, 4, len(links), argsAt,
			"ON CONFLICT (chunk_id, entity_id) DO NOTHING")
	}
	return execBatches(tx, target, 4, len(links), argsAt)
}

// execBatches issues multi-row INSERTs over n logical rows, cols values each,
// obtaining each row's arguments from argsAt.
func execBatches(tx *sql.Tx, target string, cols, n int, argsAt func(i int) []any) error {
	return execBatchesOnConflict(tx, target, cols, n, argsAt, "")
}

// execBatchesOnConflict is execBatches with a trailing conflict clause.
func execBatchesOnConflict(tx *sql.Tx, target string, cols, n int, argsAt func(i int) []any, onConflict string) error {
	if n == 0 {
		return nil
	}
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	if onConflict != "" {
		onConflict = " " + onConflict
	}
	for start := 0; start < n; start += batchRows {
		end := min(start+batchRows, n)
		count := end - start
		tuples := make([]string, count)
		args := make([]any, 0, count*cols)
		for i := 0; i < count; i++ {
			tuples[i] = tuple
			args = append(args, argsAt(start+i)...)
		}
		q := `INSERT INTO ` + target + ` VALUES ` + strings.Join(tuples, ",") + onConflict
		if _, err := tx.Exec(q, args...); err != nil {
			return fmt.Errorf("batch insert into %s: %w", target, err)
		}
	}
	return nil
}

// deleteChunksForSourceTx removes a source's chunks and everything hanging off
// them. Edges are deleted where the chunk appears on EITHER side: a surviving
// chunk may point at one of these, and leaving that behind would make the graph
// walk dereference a vanished chunk.
func deleteChunksForSourceTx(tx *sql.Tx, sourceID string) error {
	const chunkSel = `SELECT id FROM memory_chunks WHERE source_id = ?`
	stmts := []struct {
		what string
		sql  string
	}{
		{"chunk terms", `DELETE FROM memory_chunk_terms WHERE chunk_id IN (` + chunkSel + `)`},
		{"chunk entities", `DELETE FROM memory_chunk_entities WHERE chunk_id IN (` + chunkSel + `)`},
		{"outbound edges", `DELETE FROM memory_edges WHERE src_chunk_id IN (` + chunkSel + `)`},
		{"inbound edges", `DELETE FROM memory_edges WHERE dst_chunk_id IN (` + chunkSel + `)`},
		{"chunks", `DELETE FROM memory_chunks WHERE source_id = ?`},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(st.sql, sourceID); err != nil {
			return fmt.Errorf("delete %s: %w", st.what, err)
		}
	}
	return nil
}

// DeleteSource removes one source and all memory derived from it.
func (s *Store) DeleteSource(sourceID string) error {
	defer s.lockWrites()()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteChunksForSourceTx(tx, sourceID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_sources WHERE id = ?`, sourceID); err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	if err := pruneOrphansTx(tx); err != nil {
		return err
	}
	if err := markStatsDirtyTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RefreshSourceStamps updates a source's mtime and size without touching its
// chunks, for a file whose stamps moved but whose content hash did not.
//
// Re-chunking in that case would be actively harmful, not merely wasteful: it
// assigns fresh chunk ids, which deletes every edge touching the old ones — so a
// `touch` on a note would silently erode the graph.
func (s *Store) RefreshSourceStamps(sourceID, mtime string, size int64) error {
	defer s.lockWrites()()
	var mt any
	if mtime != "" {
		mt = mtime
	}
	_, err := s.db.Exec(
		`UPDATE memory_sources SET mtime = ?, file_size = ? WHERE id = ?`, mt, size, sourceID)
	if err != nil {
		return fmt.Errorf("refresh source stamps: %w", err)
	}
	return nil
}

// SetSourceEntityPass records progress of the background LLM entity pass so an
// interrupted run resumes instead of restarting.
func (s *Store) SetSourceEntityPass(sourceID, state string) error {
	defer s.lockWrites()()
	_, err := s.db.Exec(`UPDATE memory_sources SET entity_pass = ? WHERE id = ?`, state, sourceID)
	if err != nil {
		return fmt.Errorf("set entity_pass: %w", err)
	}
	return nil
}

// ---------- chunks ----------

// GetChunksBySource returns a source's chunks in order, without embeddings
// (callers that need vectors do similarity in SQL rather than pulling them out).
func (s *Store) GetChunksBySource(sourceID string) ([]MemoryChunk, error) {
	rows, err := s.db.Query(`
		SELECT id, source_id, ord, text, heading_path, token_count, char_len,
		       embed_model, created_at, embedding IS NOT NULL
		FROM memory_chunks WHERE source_id = ? ORDER BY ord`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query chunks: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryChunk, 0)
	for rows.Next() {
		var c MemoryChunk
		var hasEmbedding bool
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Ord, &c.Text, &c.HeadingPath,
			&c.TokenCount, &c.CharLen, &c.EmbedModel, &c.CreatedAt, &hasEmbedding); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		if hasEmbedding {
			c.Embedding = []float32{} // non-nil marker: embedded, not loaded
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChunksMissingEmbedding returns chunk ids and the text to embed, for the
// backfill pass that runs when the model was unavailable during ingestion.
func (s *Store) ChunksMissingEmbedding(limit int) ([]MemoryChunk, error) {
	rows, err := s.db.Query(`
		SELECT id, source_id, ord, text, heading_path, token_count,
		       thread_context, cot_context
		FROM memory_chunks WHERE embedding IS NULL ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unembedded chunks: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryChunk, 0)
	for rows.Next() {
		var c MemoryChunk
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Ord, &c.Text, &c.HeadingPath,
			&c.TokenCount, &c.ThreadContext, &c.CotContext); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CandidateChunksByTerms returns chunks sharing any of the given terms, for
// ingest-side near-duplicate detection. Chunks belonging to excludeSourceRef are
// omitted so re-ingesting a file does not see its own previous chunks as
// duplicates of themselves.
//
// Preselecting by rare terms is what keeps dedup cheap: comparing every new chunk
// against the whole corpus would make ingestion quadratic in corpus size.
func (s *Store) CandidateChunksByTerms(terms []string, limit int, excludeSourceRef string) ([]MemoryChunk, error) {
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	ph := make([]string, len(terms))
	args := make([]any, 0, len(terms)+2)
	for i, t := range terms {
		ph[i] = "?"
		args = append(args, t)
	}
	args = append(args, excludeSourceRef, limit)

	rows, err := s.db.Query(`
		SELECT c.id, c.source_id, c.text
		FROM memory_chunks c
		JOIN memory_sources s ON s.id = c.source_id
		WHERE c.id IN (
			SELECT chunk_id FROM memory_chunk_terms WHERE term IN (`+strings.Join(ph, ",")+`)
		)
		AND s.source_ref <> ?
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query dedup candidates: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryChunk, 0, limit)
	for rows.Next() {
		var c MemoryChunk
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Text); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetChunkEmbeddings writes vectors for already-persisted chunks.
func (s *Store) SetChunkEmbeddings(embedModel string, vecs map[string][]float32) error {
	defer s.lockWrites()()
	if len(vecs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for id, v := range vecs {
		if len(v) != EmbedDim {
			return fmt.Errorf("chunk %s: embedding has %d dims, want %d", id, len(v), EmbedDim)
		}
		if _, err := tx.Exec(
			`UPDATE memory_chunks SET embedding = ?::FLOAT[384], embed_model = ? WHERE id = ?`,
			vecToAny(v), embedModel, id,
		); err != nil {
			return fmt.Errorf("set embedding for %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// VectorHit is one nearest-neighbour result, carrying enough provenance for the
// caller to attribute it without a second query.
type VectorHit struct {
	ChunkID     string  `json:"chunkId"`
	SourceID    string  `json:"sourceId"`
	SourceRef   string  `json:"sourceRef"`
	SourceType  string  `json:"sourceType"`
	Title       string  `json:"title"`
	HeadingPath string  `json:"headingPath"`
	Text        string  `json:"text"`
	Similarity  float64 `json:"similarity"`
}

// NearestChunks ranks chunks by cosine similarity to a query vector, in SQL.
//
// array_cosine_distance is 1 - cosine, so ORDER BY it ascending is nearest-first
// and similarity is 1 - distance. Both verified in Phase 0.5, along with the
// binding shape: a vector binds as []any of float32 with an explicit
// ?::FLOAT[384] cast — a plain []float32 is rejected.
func (s *Store) NearestChunks(query []float32, limit int) ([]VectorHit, error) {
	if len(query) != EmbedDim {
		return nil, fmt.Errorf("query vector has %d dims, want %d", len(query), EmbedDim)
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(`
		SELECT c.id, c.source_id, s.source_ref, s.source_type, s.title,
		       c.heading_path, c.text,
		       1 - array_cosine_distance(c.embedding, ?::FLOAT[384]) AS sim
		FROM memory_chunks c
		JOIN memory_sources s ON s.id = c.source_id
		WHERE c.embedding IS NOT NULL
		ORDER BY array_cosine_distance(c.embedding, ?::FLOAT[384])
		LIMIT ?`, vecToAny(query), vecToAny(query), limit)
	if err != nil {
		return nil, fmt.Errorf("nearest chunks: %w", err)
	}
	defer rows.Close()

	out := make([]VectorHit, 0, limit)
	for rows.Next() {
		var h VectorHit
		if err := rows.Scan(&h.ChunkID, &h.SourceID, &h.SourceRef, &h.SourceType,
			&h.Title, &h.HeadingPath, &h.Text, &h.Similarity); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PostingRow is one (chunk, term) posting with the data BM25 needs.
type PostingRow struct {
	ChunkID    string
	Term       string
	TF         int
	TokenCount int
}

// Postings fetches the postings for a set of query terms.
//
// BM25 is scored in Go rather than in SQL because the `fts` extension is not in the
// prebuilt DuckDB libraries (§3.0) and autoloading it would add a network
// dependency. This returns the raw postings; the arithmetic lives in the memory
// package where it is unit-testable against hand-computed values.
//
// maxRows caps the result: a common term can match most of the corpus, and pulling
// every posting for "the" would dominate query latency for no benefit. Callers
// should pass the rarest terms first (see TermDF).
func (s *Store) Postings(terms []string, maxRows int) ([]PostingRow, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	if maxRows <= 0 {
		maxRows = 20000
	}
	ph := make([]string, len(terms))
	args := make([]any, 0, len(terms)+1)
	for i, t := range terms {
		ph[i] = "?"
		args = append(args, t)
	}
	args = append(args, maxRows)

	rows, err := s.db.Query(`
		SELECT ct.chunk_id, ct.term, ct.tf, c.token_count
		FROM memory_chunk_terms ct
		JOIN memory_chunks c ON c.id = ct.chunk_id
		WHERE ct.term IN (`+strings.Join(ph, ",")+`)
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query postings: %w", err)
	}
	defer rows.Close()

	out := make([]PostingRow, 0, 256)
	for rows.Next() {
		var p PostingRow
		if err := rows.Scan(&p.ChunkID, &p.Term, &p.TF, &p.TokenCount); err != nil {
			return nil, fmt.Errorf("scan posting: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EntityMatch is a chunk that shares an entity with the query.
type EntityMatch struct {
	ChunkID   string
	Kind      string
	ValueNorm string
	Count     int
}

// ExistingEntityValues returns the subset of candidates that exist as entity values.
//
// This is what turns query-side entity extraction from a *guess* into a *lookup*.
// Running the corpus-side heuristics on a query does not work: they recognize proper
// nouns by capitalization, and a typed question is lowercase, so they find nothing —
// measured at 47 of 50 eval queries yielding zero entities. Since the entity signal is
// symmetric, that made the whole signal inert and made corpus-side enrichment
// unreachable no matter how good it was.
//
// The corpus's entity vocabulary is already known, so the query side should ask "which
// of the things I know about appear in this query" instead of trying to recognize
// entities from scratch. Callers pass word n-grams, which keeps this an indexed lookup
// rather than a scan with a LIKE per row.
func (s *Store) ExistingEntityValues(candidates []string) ([]string, error) {
	out := make([]string, 0)
	if len(candidates) == 0 {
		return out, nil
	}
	seen := map[string]bool{}
	for start := 0; start < len(candidates); start += batchRows {
		end := min(start+batchRows, len(candidates))
		batch := candidates[start:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, c := range batch {
			ph[i] = "?"
			args[i] = c
		}
		rows, err := s.db.Query(
			`SELECT DISTINCT value_norm FROM memory_entities
			 WHERE value_norm IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("match entity values: %w", err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// ChunksByEntities finds chunks sharing any of the given normalized entity values.
func (s *Store) ChunksByEntities(values []string, limit int) ([]EntityMatch, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	ph := make([]string, len(values))
	args := make([]any, 0, len(values)+1)
	for i, v := range values {
		ph[i] = "?"
		args = append(args, v)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT ce.chunk_id, e.kind, e.value_norm, ce.count
		FROM memory_chunk_entities ce
		JOIN memory_entities e ON e.id = ce.entity_id
		WHERE e.value_norm IN (`+strings.Join(ph, ",")+`)
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query entity matches: %w", err)
	}
	defer rows.Close()

	out := make([]EntityMatch, 0, 64)
	for rows.Next() {
		var m EntityMatch
		if err := rows.Scan(&m.ChunkID, &m.Kind, &m.ValueNorm, &m.Count); err != nil {
			return nil, fmt.Errorf("scan entity match: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ScoredChunk carries everything the fusion layer and the tool output need, so a
// ranked result set costs one query rather than one per hit.
type ScoredChunk struct {
	ChunkID     string
	SourceID    string
	SourceRef   string
	SourceType  string
	SessionID   string
	Title       string
	Path        string
	HeadingPath string
	Text        string
	TokenCount  int
	IngestedAt  string
	// EntityValues holds the chunk's entity values, for the overlap signal.
	EntityValues []string
}

// ChunksByIDs loads full metadata for a candidate set.
func (s *Store) ChunksByIDs(ids []string) (map[string]*ScoredChunk, error) {
	out := make(map[string]*ScoredChunk, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// Batch to keep the parameter list sane on a large candidate set.
	const batch = 400
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(`
			SELECT c.id, c.source_id, s.source_ref, s.source_type, s.session_id,
			       s.title, s.path, c.heading_path, c.text, c.token_count, s.ingested_at
			FROM memory_chunks c
			JOIN memory_sources s ON s.id = c.source_id
			WHERE c.id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query chunks by id: %w", err)
		}
		for rows.Next() {
			var c ScoredChunk
			if err := rows.Scan(&c.ChunkID, &c.SourceID, &c.SourceRef, &c.SourceType,
				&c.SessionID, &c.Title, &c.Path, &c.HeadingPath, &c.Text,
				&c.TokenCount, &c.IngestedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan chunk: %w", err)
			}
			cp := c
			out[c.ChunkID] = &cp
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Attach entity values in one further pass.
	idList := make([]string, 0, len(out))
	for id := range out {
		idList = append(idList, id)
	}
	for start := 0; start < len(idList); start += batch {
		end := min(start+batch, len(idList))
		chunk := idList[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(`
			SELECT ce.chunk_id, e.value_norm
			FROM memory_chunk_entities ce
			JOIN memory_entities e ON e.id = ce.entity_id
			WHERE ce.chunk_id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query chunk entities: %w", err)
		}
		for rows.Next() {
			var id, val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return nil, err
			}
			if c, ok := out[id]; ok {
				c.EntityValues = append(c.EntityValues, val)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// NearestChunkIDs returns the ids and similarities of the nearest chunks, for use
// as one arm of candidate generation. Lighter than NearestChunks: no text.
func (s *Store) NearestChunkIDs(query []float32, limit int) (map[string]float64, error) {
	if len(query) != EmbedDim {
		return nil, fmt.Errorf("query vector has %d dims, want %d", len(query), EmbedDim)
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`
		SELECT id, 1 - array_cosine_distance(embedding, ?::FLOAT[384]) AS sim
		FROM memory_chunks
		WHERE embedding IS NOT NULL
		ORDER BY array_cosine_distance(embedding, ?::FLOAT[384])
		LIMIT ?`, vecToAny(query), vecToAny(query), limit)
	if err != nil {
		return nil, fmt.Errorf("nearest chunk ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string]float64, limit)
	for rows.Next() {
		var id string
		var sim float64
		if err := rows.Scan(&id, &sim); err != nil {
			return nil, err
		}
		out[id] = sim
	}
	return out, rows.Err()
}

// ClearEmbeddings nulls every vector, for use when the embedding model changes.
// Chunks and their term/entity data are kept — only the vector space is invalid.
func (s *Store) ClearEmbeddings() error {
	defer s.lockWrites()()
	_, err := s.db.Exec(`UPDATE memory_chunks SET embedding = NULL, embed_model = ''`)
	if err != nil {
		return fmt.Errorf("clear embeddings: %w", err)
	}
	return nil
}

// ---------- entity enrichment ----------

// SourcesPendingEnrichment returns sources whose LLM entity pass has not run,
// highest-value first.
//
// Priority is the point, not a nicety: a vault whose enrichment is 40% done should
// have the *useful* 40% done, so ordering is by outbound link count, then token
// count, then recency. Most-linked first because a note the user cross-references is
// one they think matters; size next because a long note has more to extract; recency
// last as the tie-break.
func (s *Store) SourcesPendingEnrichment(limit int) ([]MemorySource, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT s.id, s.source_type, s.source_ref, s.session_id, s.title, s.path,
		       s.content_hash, s.mtime, s.file_size, s.ingested_at, s.token_count,
		       s.entity_pass
		FROM memory_sources s
		LEFT JOIN (
			-- COUNT(DISTINCT to_ref), not COUNT(*): a note that references the same
			-- target twice — say by name and by alias — is not better connected than one
			-- that references it once, and counting rows let the duplicate outrank a note
			-- with more actual links.
			SELECT from_ref, COUNT(DISTINCT to_ref) AS n FROM memory_links GROUP BY from_ref
		) l ON l.from_ref = s.source_ref
		WHERE s.entity_pass = ?
		ORDER BY COALESCE(l.n, 0) DESC, s.token_count DESC, s.ingested_at DESC, s.source_ref
		LIMIT ?`, EntityPassPending, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending enrichment: %w", err)
	}
	defer rows.Close()

	out := make([]MemorySource, 0, limit)
	for rows.Next() {
		var src MemorySource
		var mtime sql.NullString
		if err := rows.Scan(&src.ID, &src.SourceType, &src.SourceRef, &src.SessionID,
			&src.Title, &src.Path, &src.ContentHash, &mtime, &src.FileSize,
			&src.IngestedAt, &src.TokenCount, &src.EntityPass); err != nil {
			return nil, fmt.Errorf("scan pending source: %w", err)
		}
		src.MTime = mtime.String
		out = append(out, src)
	}
	return out, rows.Err()
}

// CountSourcesByEntityPass summarizes enrichment progress for reporting.
func (s *Store) CountSourcesByEntityPass() (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT entity_pass, COUNT(*) FROM memory_sources GROUP BY entity_pass`)
	if err != nil {
		return nil, fmt.Errorf("count entity_pass: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// AddChunkEntities adds entity associations to chunks that already exist, without
// touching the chunks themselves or the associations another extractor produced.
//
// Distinct from the write inside ReplaceSource, which owns a source's whole entity
// set. The LLM tier runs as a second pass over stored chunks, so it must *add* —
// replacing would discard the heuristic and tag tiers that search has been using
// since ingestion.
func (s *Store) AddChunkEntities(byChunk map[string][]ChunkEntity) error {
	defer s.lockWrites()()
	if len(byChunk) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Reuse the batched resolve-and-link path by shaping the input as chunks.
	chunks := make([]MemoryChunk, 0, len(byChunk))
	for id, ents := range byChunk {
		chunks = append(chunks, MemoryChunk{ID: id, Entities: ents})
	}
	// Deterministic order so a repeated run produces identical entity ids.
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].ID < chunks[b].ID })

	if err := insertChunkEntitiesTx(tx, chunks, true); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteChunkEntitiesByExtractor removes every association a given extractor
// produced, then prunes entities left with no chunks.
//
// This is what makes the `extractor` provenance column worth having: the whole LLM
// tier can be rolled back or re-run without disturbing the heuristic and tag tiers.
func (s *Store) DeleteChunkEntitiesByExtractor(extractor string) (int64, error) {
	defer s.lockWrites()()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`DELETE FROM memory_chunk_entities WHERE extractor = ?`, extractor)
	if err != nil {
		return 0, fmt.Errorf("delete %s entities: %w", extractor, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`
		DELETE FROM memory_entities
		WHERE id NOT IN (SELECT entity_id FROM memory_chunk_entities)`); err != nil {
		return 0, fmt.Errorf("prune orphaned entities: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit rollback of %s tier: %w", extractor, err)
	}
	return n, nil
}

// ResetEntityPass sets every source back to pending, so the enrichment pass can be
// re-run from scratch (after a model change, or a rollback of the LLM tier).
func (s *Store) ResetEntityPass() error {
	defer s.lockWrites()()
	_, err := s.db.Exec(`UPDATE memory_sources SET entity_pass = ?`, EntityPassPending)
	if err != nil {
		return fmt.Errorf("reset entity_pass: %w", err)
	}
	return nil
}

// SourceRefsByType returns every source_ref of one type, for resolving the targets
// an LLM proposes as cross-references.
func (s *Store) SourceRefsByType(sourceType string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT source_ref FROM memory_sources WHERE source_type = ? ORDER BY source_ref`, sourceType)
	if err != nil {
		return nil, fmt.Errorf("query source refs: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ---------- links ----------

// StoredLink is one resolved link, persisted so link edges can be rebuilt without
// re-reading the vault.
//
// Why this table exists: incremental re-ingest replaces a note's chunks, which
// deletes every edge touching them — *including inbound ones*. A note that links to
// the changed note is not itself re-ingested, so its link edge would be lost
// permanently and the graph would quietly erode with every edit. Rebuilding from
// this table restores the edges of every note on either side of the change, and it
// costs one small row per link rather than a full vault re-read.
type StoredLink struct {
	FromRef string
	ToRef   string
	Heading string
	// Raw is the link exactly as written for an authored link, or the evidence quote
	// for an inferred one. Either way it is the text used to find which chunk of the
	// source note makes the reference.
	Raw   string
	Embed bool
	// Kind is "link" for an authored wikilink or Markdown link, "inferred_link" for a
	// cross-reference the LLM pass proposed. Empty is treated as "link".
	//
	// Inferred links are stored here, rather than only written as edges, for the same
	// reason authored ones are: re-ingesting a note deletes the edges pointing at it,
	// and the notes on the other end are not re-enriched, so an edge-only inferred
	// link would erode exactly as authored links did before Phase 7.
	Kind string
}

// ReplaceLinksFrom replaces the links of one kind leaving fromRef. Unresolved links
// are not stored: they cannot produce an edge, and IngestReport already counts them.
//
// Scoped by kind because two independent passes write here. Ingestion owns the
// authored links of a note; the LLM pass owns its inferred ones. An unscoped replace
// would mean whichever ran last destroyed the other's work — re-ingesting a note would
// silently discard its inferred cross-references, and enriching it would discard its
// wikilinks.
func (s *Store) ReplaceLinksFrom(fromRef, kind string, links []StoredLink) error {
	defer s.lockWrites()()
	if kind == "" {
		kind = "link"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM memory_links WHERE from_ref = ? AND kind = ?`, fromRef, kind); err != nil {
		return fmt.Errorf("delete %s links from %s: %w", kind, fromRef, err)
	}
	seen := make(map[StoredLink]bool, len(links))
	rows := make([]StoredLink, 0, len(links))
	for _, l := range links {
		if l.ToRef == "" {
			continue
		}
		l.FromRef = fromRef
		l.Kind = kind
		if seen[l] {
			continue // the same link written twice in one note is one assertion
		}
		seen[l] = true
		rows = append(rows, l)
	}
	if err := execBatches(tx, "memory_links (from_ref, to_ref, heading, raw, embed, kind)", 6, len(rows),
		func(i int) []any {
			return []any{rows[i].FromRef, rows[i].ToRef, rows[i].Heading, rows[i].Raw,
				rows[i].Embed, rows[i].Kind}
		}); err != nil {
		return err
	}
	return tx.Commit()
}

// LinksTouching returns every stored link with one end in refs — both the links
// leaving those sources and the backlinks pointing at them. That set is exactly
// what an incremental re-ingest has to rebuild.
func (s *Store) LinksTouching(refs []string) ([]StoredLink, error) {
	out := make([]StoredLink, 0)
	if len(refs) == 0 {
		return out, nil
	}
	seen := map[StoredLink]bool{}
	for start := 0; start < len(refs); start += batchRows {
		end := min(start+batchRows, len(refs))
		batch := refs[start:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)*2)
		for i, r := range batch {
			ph[i] = "?"
			args = append(args, r)
		}
		for _, r := range batch {
			args = append(args, r)
		}
		list := strings.Join(ph, ",")
		rows, err := s.db.Query(`
			SELECT from_ref, to_ref, heading, raw, embed, kind FROM memory_links
			WHERE from_ref IN (`+list+`) OR to_ref IN (`+list+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query links touching: %w", err)
		}
		for rows.Next() {
			var l StoredLink
			if err := rows.Scan(&l.FromRef, &l.ToRef, &l.Heading, &l.Raw, &l.Embed, &l.Kind); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan link: %w", err)
			}
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// Deterministic order, so an edge rebuild is reproducible.
	sort.Slice(out, func(a, b int) bool {
		if out[a].FromRef != out[b].FromRef {
			return out[a].FromRef < out[b].FromRef
		}
		if out[a].ToRef != out[b].ToRef {
			return out[a].ToRef < out[b].ToRef
		}
		return out[a].Raw < out[b].Raw
	})
	return out, nil
}

// DeleteLinksFrom removes a source's links, for a note that no longer exists.
func (s *Store) DeleteLinksFrom(fromRef string) error {
	defer s.lockWrites()()
	_, err := s.db.Exec(`DELETE FROM memory_links WHERE from_ref = ?`, fromRef)
	if err != nil {
		return fmt.Errorf("delete links from %s: %w", fromRef, err)
	}
	return nil
}

// ChunkIDsForSources returns the chunk ids belonging to the given sources, for
// scoping an incremental similarity pass to what actually changed.
func (s *Store) ChunkIDsForSources(sourceIDs []string) ([]string, error) {
	out := make([]string, 0)
	if len(sourceIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(sourceIDs); start += batchRows {
		end := min(start+batchRows, len(sourceIDs))
		batch := sourceIDs[start:end]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			ph[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(
			`SELECT id FROM memory_chunks WHERE source_id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
		if err != nil {
			return nil, fmt.Errorf("query chunk ids for sources: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------- edges ----------

// InsertEdges upserts graph edges.
func (s *Store) InsertEdges(edges []MemoryEdge) error {
	defer s.lockWrites()()
	if len(edges) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range edges {
		if e.SrcChunkID == "" || e.DstChunkID == "" || e.Kind == "" {
			return fmt.Errorf("edge requires src, dst and kind: %+v", e)
		}
		if _, err := tx.Exec(`
			INSERT INTO memory_edges (src_chunk_id, dst_chunk_id, kind, weight)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (src_chunk_id, dst_chunk_id, kind) DO UPDATE SET weight = excluded.weight`,
			e.SrcChunkID, e.DstChunkID, e.Kind, e.Weight,
		); err != nil {
			return fmt.Errorf("insert edge: %w", err)
		}
	}
	return tx.Commit()
}

// InsertEdgesBatch is InsertEdges batched into multi-row statements, for the bulk
// edge-build path.
//
// Batched for the same reason the term tables are (see insertChunkTermsTx):
// DuckDB's per-statement cost dominates single-row inserts, and a vault-scale
// corpus produces far more edges than chunks. Conflicts are possible here —
// unlike terms, the same pair can be proposed twice by different passes — so this
// keeps the ON CONFLICT clause and issues one statement per batch.
func (s *Store) InsertEdgesBatch(edges []MemoryEdge) error {
	defer s.lockWrites()()
	if len(edges) == 0 {
		return nil
	}
	for _, e := range edges {
		if e.SrcChunkID == "" || e.DstChunkID == "" || e.Kind == "" {
			return fmt.Errorf("edge requires src, dst and kind: %+v", e)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const tuple = "(?,?,?,?)"
	for start := 0; start < len(edges); start += batchRows {
		end := min(start+batchRows, len(edges))
		batch := edges[start:end]
		tuples := make([]string, len(batch))
		args := make([]any, 0, len(batch)*4)
		for i, e := range batch {
			tuples[i] = tuple
			args = append(args, e.SrcChunkID, e.DstChunkID, e.Kind, e.Weight)
		}
		if _, err := tx.Exec(`
			INSERT INTO memory_edges (src_chunk_id, dst_chunk_id, kind, weight)
			VALUES `+strings.Join(tuples, ",")+`
			ON CONFLICT (src_chunk_id, dst_chunk_id, kind) DO UPDATE SET weight = excluded.weight`,
			args...); err != nil {
			return fmt.Errorf("batch insert edges: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteEdgesByKind removes every edge of the given kinds. Used to rebuild a
// derived edge kind from scratch without disturbing the others — `similar` and
// `entity` are recomputed wholesale, while `link` and `inferred_link` record
// assertions that should not be discarded by a similarity rebuild.
func (s *Store) DeleteEdgesByKind(kinds ...string) (int64, error) {
	defer s.lockWrites()()
	if len(kinds) == 0 {
		return 0, nil
	}
	ph := make([]string, len(kinds))
	args := make([]any, len(kinds))
	for i, k := range kinds {
		ph[i] = "?"
		args[i] = k
	}
	res, err := s.db.Exec(`DELETE FROM memory_edges WHERE kind IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("delete edges by kind: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // driver may not report it
	}
	return n, nil
}

// BuildSequentialEdges writes next/prev edges between consecutive chunks of the
// same source. sourceID may be empty to cover the whole corpus.
//
// Done in SQL rather than in Go because it is a pure self-join on (source_id,
// ord): pulling every chunk id into Go to pair them up would be the same work
// with a round trip per source.
func (s *Store) BuildSequentialEdges(sourceID string, weight float64) (int, error) {
	defer s.lockWrites()()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	scope := ""
	baseArgs := []any{}
	if sourceID != "" {
		scope = ` AND a.source_id = ?`
		baseArgs = append(baseArgs, sourceID)
	}

	total := 0
	// 'next' points forward, 'prev' points back. Both are stored so the walk can
	// read a chunk's neighbours with one outbound query rather than a union.
	for _, spec := range []struct {
		kind string
		join string
	}{
		{"next", `b.ord = a.ord + 1`},
		{"prev", `b.ord = a.ord - 1`},
	} {
		args := append([]any{spec.kind, weight}, baseArgs...)
		res, err := tx.Exec(`
			INSERT INTO memory_edges (src_chunk_id, dst_chunk_id, kind, weight)
			SELECT a.id, b.id, ?, ?
			FROM memory_chunks a
			JOIN memory_chunks b ON b.source_id = a.source_id AND `+spec.join+`
			WHERE TRUE`+scope+`
			ON CONFLICT (src_chunk_id, dst_chunk_id, kind) DO UPDATE SET weight = excluded.weight`,
			args...)
		if err != nil {
			return 0, fmt.Errorf("build %s edges: %w", spec.kind, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit sequential edges: %w", err)
	}
	return total, nil
}

// ChunkRef is the minimum needed to aim an edge at a chunk.
type ChunkRef struct {
	ChunkID     string
	SourceID    string
	Ord         int
	HeadingPath string
	Text        string
}

// ChunkRefsBySourceRefs returns each named source's chunks in order, keyed by
// source_ref. Link edges need both ends resolved from a source_ref, and doing it
// in one query keeps edge building from issuing two lookups per link.
func (s *Store) ChunkRefsBySourceRefs(sourceType string, refs []string) (map[string][]ChunkRef, error) {
	out := make(map[string][]ChunkRef, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	for start := 0; start < len(refs); start += batchRows {
		end := min(start+batchRows, len(refs))
		batch := refs[start:end]
		ph := make([]string, len(batch))
		args := []any{sourceType}
		for i, r := range batch {
			ph[i] = "?"
			args = append(args, r)
		}
		rows, err := s.db.Query(`
			SELECT s.source_ref, c.id, c.source_id, c.ord, c.heading_path, c.text
			FROM memory_chunks c
			JOIN memory_sources s ON s.id = c.source_id
			WHERE s.source_type = ? AND s.source_ref IN (`+strings.Join(ph, ",")+`)
			ORDER BY s.source_ref, c.ord`, args...)
		if err != nil {
			return nil, fmt.Errorf("query chunk refs: %w", err)
		}
		for rows.Next() {
			var ref string
			var c ChunkRef
			if err := rows.Scan(&ref, &c.ChunkID, &c.SourceID, &c.Ord, &c.HeadingPath, &c.Text); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan chunk ref: %w", err)
			}
			out[ref] = append(out[ref], c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EmbeddedChunkIDs returns every chunk id that has a vector, in a stable order.
// The similarity pass needs the full set for a from-scratch build.
func (s *Store) EmbeddedChunkIDs() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM memory_chunks WHERE embedding IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query embedded chunk ids: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SimilarNeighbors returns, for each given chunk, its topK most similar chunks in
// *other* sources with similarity at or above minSim.
//
// Same-source pairs are excluded deliberately: consecutive chunks of one note are
// both trivially similar and already joined by next/prev edges, so including them
// would spend the per-chunk edge budget re-asserting adjacency instead of finding
// the cross-document connections the walk exists for.
//
// The ranking is done in SQL — one windowed self-join over FLOAT[384] in DuckDB's
// vectorized engine, rather than |ids| round trips. This is the expensive pass:
// it is O(|ids| x corpus), which is why callers scope ids to what actually
// changed instead of rebuilding globally.
func (s *Store) SimilarNeighbors(ids []string, topK int, minSim float64) ([]MemoryEdge, error) {
	if len(ids) == 0 || topK <= 0 {
		return nil, nil
	}
	out := make([]MemoryEdge, 0, len(ids)*topK)
	// A smaller batch than batchRows: each id fans out across the whole corpus, so
	// the join's working set grows with the batch.
	const simBatch = 64
	for start := 0; start < len(ids); start += simBatch {
		end := min(start+simBatch, len(ids))
		batch := ids[start:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)+2)
		for i, id := range batch {
			ph[i] = "?"
			args = append(args, id)
		}
		args = append(args, topK, minSim)

		rows, err := s.db.Query(`
			SELECT src, dst, sim FROM (
				SELECT a.id AS src, b.id AS dst,
				       1 - array_cosine_distance(a.embedding, b.embedding) AS sim,
				       row_number() OVER (
				           PARTITION BY a.id
				           ORDER BY array_cosine_distance(a.embedding, b.embedding), b.id
				       ) AS rn
				FROM memory_chunks a
				JOIN memory_chunks b
				  ON b.source_id <> a.source_id AND b.embedding IS NOT NULL
				WHERE a.id IN (`+strings.Join(ph, ",")+`) AND a.embedding IS NOT NULL
			)
			WHERE rn <= ? AND sim >= ?
			ORDER BY src, sim DESC, dst`, args...)
		if err != nil {
			return nil, fmt.Errorf("query similar neighbors: %w", err)
		}
		for rows.Next() {
			var e MemoryEdge
			if err := rows.Scan(&e.SrcChunkID, &e.DstChunkID, &e.Weight); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan similar neighbor: %w", err)
			}
			e.Kind = "similar"
			out = append(out, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EntityGroup is one entity and the chunks carrying it, with the chunk count that
// gives the entity its inverse-frequency weight.
type EntityGroup struct {
	EntityID  string
	Kind      string
	ValueNorm string
	// Chunks holds (chunk_id, source_id) pairs, so a caller can avoid wiring two
	// chunks of the same source together.
	Chunks []ChunkRef
}

// RareEntityGroups returns entities carried by between 2 and maxChunks chunks,
// with their chunk lists.
//
// The upper bound is the whole point: an entity present in hundreds of chunks — a
// ubiquitous project name, not a rare proper noun — would wire the entire graph
// into one hub and make the walk return arbitrary chunks. Entities above the cap
// are skipped entirely rather than down-weighted, because their pair count is
// quadratic and paying to build edges that then score near zero is waste.
func (s *Store) RareEntityGroups(maxChunks int) ([]EntityGroup, error) {
	if maxChunks < 2 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		WITH counted AS (
			SELECT entity_id, COUNT(*) AS n
			FROM memory_chunk_entities
			GROUP BY entity_id
			HAVING COUNT(*) >= 2 AND COUNT(*) <= ?
		)
		SELECT e.id, e.kind, e.value_norm, ce.chunk_id, c.source_id
		FROM counted
		JOIN memory_entities e ON e.id = counted.entity_id
		JOIN memory_chunk_entities ce ON ce.entity_id = counted.entity_id
		JOIN memory_chunks c ON c.id = ce.chunk_id
		ORDER BY e.id, ce.chunk_id`, maxChunks)
	if err != nil {
		return nil, fmt.Errorf("query rare entity groups: %w", err)
	}
	defer rows.Close()

	var out []EntityGroup
	for rows.Next() {
		var id, kind, val, chunkID, sourceID string
		if err := rows.Scan(&id, &kind, &val, &chunkID, &sourceID); err != nil {
			return nil, fmt.Errorf("scan entity group: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].EntityID == id {
			out[n-1].Chunks = append(out[n-1].Chunks, ChunkRef{ChunkID: chunkID, SourceID: sourceID})
			continue
		}
		out = append(out, EntityGroup{
			EntityID: id, Kind: kind, ValueNorm: val,
			Chunks: []ChunkRef{{ChunkID: chunkID, SourceID: sourceID}},
		})
	}
	return out, rows.Err()
}

// EdgeCountsByKind summarizes the graph, for reporting and for tests that need to
// assert a pass actually wrote what it claimed.
func (s *Store) EdgeCountsByKind() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM memory_edges GROUP BY kind ORDER BY kind`)
	if err != nil {
		return nil, fmt.Errorf("edge counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// GetEdgesFromMany is GetEdgesFrom for a whole frontier, in two queries rather
// than two per node. The graph walk visits tens of nodes per hop, so the
// per-query cost is what would otherwise dominate its latency.
func (s *Store) GetEdgesFromMany(ids []string, bidirectionalKinds ...string) (map[string][]MemoryEdge, error) {
	out := make(map[string][]MemoryEdge, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += batchRows {
		end := min(start+batchRows, len(ids))
		batch := ids[start:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)+len(bidirectionalKinds))
		for i, id := range batch {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.Query(`
			SELECT src_chunk_id, dst_chunk_id, kind, weight
			FROM memory_edges WHERE src_chunk_id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query edges from many: %w", err)
		}
		for rows.Next() {
			var e MemoryEdge
			if err := rows.Scan(&e.SrcChunkID, &e.DstChunkID, &e.Kind, &e.Weight); err != nil {
				rows.Close()
				return nil, err
			}
			out[e.SrcChunkID] = append(out[e.SrcChunkID], e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		if len(bidirectionalKinds) == 0 {
			continue
		}
		kph := make([]string, len(bidirectionalKinds))
		kargs := make([]any, 0, len(batch)+len(bidirectionalKinds))
		kargs = append(kargs, args...)
		for i, k := range bidirectionalKinds {
			kph[i] = "?"
			kargs = append(kargs, k)
		}
		rows2, err := s.db.Query(`
			SELECT src_chunk_id, dst_chunk_id, kind, weight
			FROM memory_edges
			WHERE dst_chunk_id IN (`+strings.Join(ph, ",")+`)
			  AND kind IN (`+strings.Join(kph, ",")+`)`, kargs...)
		if err != nil {
			return nil, fmt.Errorf("query inbound edges from many: %w", err)
		}
		for rows2.Next() {
			var e MemoryEdge
			if err := rows2.Scan(&e.SrcChunkID, &e.DstChunkID, &e.Kind, &e.Weight); err != nil {
				rows2.Close()
				return nil, err
			}
			// Presented from the walker's point of view: it arrived at dst and is
			// traversing back to src.
			out[e.DstChunkID] = append(out[e.DstChunkID], MemoryEdge{
				SrcChunkID: e.DstChunkID, DstChunkID: e.SrcChunkID, Kind: e.Kind, Weight: e.Weight,
			})
		}
		rows2.Close()
		if err := rows2.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetEdgesFrom returns edges leaving a chunk, plus inbound edges of the given
// kinds — the walk treats link edges as bidirectional, since backlinks carry the
// same meaning as forward links.
func (s *Store) GetEdgesFrom(chunkID string, bidirectionalKinds ...string) ([]MemoryEdge, error) {
	out := make([]MemoryEdge, 0)

	rows, err := s.db.Query(
		`SELECT src_chunk_id, dst_chunk_id, kind, weight FROM memory_edges WHERE src_chunk_id = ?`, chunkID)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	for rows.Next() {
		var e MemoryEdge
		if err := rows.Scan(&e.SrcChunkID, &e.DstChunkID, &e.Kind, &e.Weight); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(bidirectionalKinds) > 0 {
		ph := make([]string, len(bidirectionalKinds))
		args := []any{chunkID}
		for i, k := range bidirectionalKinds {
			ph[i] = "?"
			args = append(args, k)
		}
		q := `SELECT src_chunk_id, dst_chunk_id, kind, weight FROM memory_edges
		      WHERE dst_chunk_id = ? AND kind IN (` + strings.Join(ph, ",") + `)`
		rows2, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("query inbound edges: %w", err)
		}
		defer rows2.Close()
		for rows2.Next() {
			var e MemoryEdge
			if err := rows2.Scan(&e.SrcChunkID, &e.DstChunkID, &e.Kind, &e.Weight); err != nil {
				return nil, err
			}
			// Present it from the walker's point of view: we arrived at src.
			out = append(out, MemoryEdge{
				SrcChunkID: e.DstChunkID, DstChunkID: e.SrcChunkID, Kind: e.Kind, Weight: e.Weight,
			})
		}
		if err := rows2.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------- BM25 corpus statistics ----------

func markStatsDirtyTx(tx *sql.Tx) error {
	_, err := tx.Exec(
		`INSERT INTO memory_meta (key, value) VALUES (?, '1')
		 ON CONFLICT (key) DO UPDATE SET value = '1'`, MetaStatsDirty)
	if err != nil {
		return fmt.Errorf("mark stats dirty: %w", err)
	}
	return nil
}

// MarkStatsDirty flags the BM25 corpus statistics as stale.
func (s *Store) MarkStatsDirty() error {
	return s.SetMeta(MetaStatsDirty, "1")
}

// BM25Stats returns (N, avgdl), recomputing from scratch when the stored values
// are marked dirty.
//
// Recompute-on-dirty rather than incremental decrement: deleting a session
// invalidates df for every term it touched, and getting that arithmetic subtly
// wrong drifts IDF with no error anywhere. A full recount over a local corpus is
// cheap, and it is provably correct.
func (s *Store) BM25Stats() (n int, avgdl float64, err error) {
	dirty, err := s.GetMeta(MetaStatsDirty)
	if err != nil {
		return 0, 0, err
	}
	if dirty != "1" {
		nStr, err := s.GetMeta(MetaStatsN)
		if err != nil {
			return 0, 0, err
		}
		aStr, err := s.GetMeta(MetaStatsAvgDL)
		if err != nil {
			return 0, 0, err
		}
		if nStr != "" && aStr != "" {
			var nn int
			var aa float64
			if _, e1 := fmt.Sscanf(nStr, "%d", &nn); e1 == nil {
				if _, e2 := fmt.Sscanf(aStr, "%g", &aa); e2 == nil {
					return nn, aa, nil
				}
			}
		}
	}
	return s.RecomputeBM25Stats()
}

// RecomputeBM25Stats rebuilds memory_terms.df and the corpus statistics from
// memory_chunk_terms, then clears the dirty flag.
func (s *Store) RecomputeBM25Stats() (int, float64, error) {
	defer s.lockWrites()()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Upsert-then-sweep, not truncate-then-refill.
	//
	// The obvious `DELETE FROM memory_terms` followed by a full reinsert deletes and
	// reinserts the *same primary keys* inside one transaction, and DuckDB reports that
	// at commit time as `write-write conflict on key: "<term>"` whenever another
	// transaction touched the same keys — which two concurrent searches do routinely,
	// since both recompute when the stats are dirty. Reproduced under load, not
	// theorized: 1 failure per ~2,000 recomputes, which is exactly the kind of rate that
	// looks like a flaky test rather than a bug.
	//
	// The store's write mutex does not help here: the conflict is between transactions
	// that never overlap in Go, but whose version chains for those keys do.
	if _, err := tx.Exec(`
		INSERT INTO memory_terms (term, df)
		SELECT term, COUNT(DISTINCT chunk_id) FROM memory_chunk_terms GROUP BY term
		ON CONFLICT (term) DO UPDATE SET df = excluded.df`); err != nil {
		return 0, 0, fmt.Errorf("rebuild df: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM memory_terms
		WHERE term NOT IN (SELECT term FROM memory_chunk_terms)`); err != nil {
		return 0, 0, fmt.Errorf("sweep vanished terms: %w", err)
	}

	var n int
	var avg sql.NullFloat64
	if err := tx.QueryRow(
		`SELECT COUNT(*), AVG(token_count) FROM memory_chunks`).Scan(&n, &avg); err != nil {
		return 0, 0, fmt.Errorf("corpus stats: %w", err)
	}
	avgdl := avg.Float64

	for k, v := range map[string]string{
		MetaStatsN:     fmt.Sprintf("%d", n),
		MetaStatsAvgDL: fmt.Sprintf("%g", avgdl),
		MetaStatsDirty: "0",
	} {
		if _, err := tx.Exec(
			`INSERT INTO memory_meta (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return 0, 0, fmt.Errorf("store stat %s: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit stats: %w", err)
	}
	return n, avgdl, nil
}

// TermDF returns document frequencies for the given terms.
func (s *Store) TermDF(terms []string) (map[string]int, error) {
	out := make(map[string]int, len(terms))
	if len(terms) == 0 {
		return out, nil
	}
	ph := make([]string, len(terms))
	args := make([]any, len(terms))
	for i, t := range terms {
		ph[i] = "?"
		args[i] = t
	}
	rows, err := s.db.Query(
		`SELECT term, df FROM memory_terms WHERE term IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("query df: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var df int
		if err := rows.Scan(&t, &df); err != nil {
			return nil, err
		}
		out[t] = df
	}
	return out, rows.Err()
}

// ---------- cascade ----------

// pruneOrphansTx removes entity and term rows no longer referenced by any chunk.
func pruneOrphansTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`
		DELETE FROM memory_entities
		WHERE id NOT IN (SELECT entity_id FROM memory_chunk_entities)`); err != nil {
		return fmt.Errorf("prune entities: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM memory_terms
		WHERE term NOT IN (SELECT term FROM memory_chunk_terms)`); err != nil {
		return fmt.Errorf("prune terms: %w", err)
	}
	return nil
}

// deleteMemoryForSessionTx removes all memory derived from a session — both its
// conversation turns and its artifacts. Directory-sourced memory has an empty
// session_id and is deliberately untouched: it is the primary corpus and has no
// session to belong to.
func deleteMemoryForSessionTx(tx *sql.Tx, sessionID string) error {
	rows, err := tx.Query(`SELECT id FROM memory_sources WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("find session sources: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		if err := deleteChunksForSourceTx(tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM memory_sources WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete session sources: %w", err)
	}
	return pruneOrphansTx(tx)
}

// cleanupOrphanedArtifacts deletes artifacts whose session no longer exists.
//
// Before the cascade below existed, DeleteSession removed messages, plan_steps
// and the session row but not artifacts, so any artifact of a deleted session
// survived as an orphan. That was unintentional. This runs on every Open — it is
// idempotent and self-healing — and reports how many rows it removed so the
// cleanup is visible rather than silent.
func (s *Store) cleanupOrphanedArtifacts() (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM artifacts WHERE session_id NOT IN (SELECT id FROM sessions)`)
	if err != nil {
		return 0, fmt.Errorf("cleanup orphaned artifacts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // driver may not report it; not worth failing Open over
	}
	return n, nil
}

// ---------- stats ----------

// MemoryStats summarizes the corpus.
func (s *Store) MemoryStats() (MemoryStats, error) {
	var st MemoryStats
	q := `SELECT
		(SELECT COUNT(*) FROM memory_sources),
		(SELECT COUNT(*) FROM memory_chunks),
		(SELECT COUNT(*) FROM memory_chunks WHERE embedding IS NOT NULL),
		(SELECT COUNT(*) FROM memory_terms),
		(SELECT COUNT(*) FROM memory_entities),
		(SELECT COUNT(*) FROM memory_edges),
		(SELECT COUNT(*) FROM memory_sources WHERE entity_pass = 'pending'),
		(SELECT COALESCE(AVG(token_count), 0) FROM memory_chunks)`
	if err := s.db.QueryRow(q).Scan(&st.Sources, &st.Chunks, &st.EmbeddedChunks,
		&st.Terms, &st.Entities, &st.Edges, &st.PendingEntities, &st.AvgDL); err != nil {
		return st, fmt.Errorf("memory stats: %w", err)
	}
	model, err := s.GetMeta(MetaEmbedModel)
	if err != nil {
		return st, err
	}
	st.EmbedModel = model
	dims, err := s.GetMeta(MetaEmbedDims)
	if err != nil {
		return st, err
	}
	if dims != "" {
		fmt.Sscanf(dims, "%d", &st.EmbedDims)
	}
	return st, nil
}

// ClearMemory removes every memory row, leaving sessions and artifacts intact.
func (s *Store) ClearMemory() error {
	defer s.lockWrites()()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, t := range []string{
		"memory_chunk_terms", "memory_chunk_entities", "memory_edges",
		"memory_chunks", "memory_sources", "memory_terms", "memory_entities",
	} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}
	if err := markStatsDirtyTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- vector conversion ----------

// vecToAny converts a float32 slice into the []any form go-duckdb v2 accepts for
// an array-typed parameter. A plain []float32 is not accepted, and the SQL must
// cast with ?::FLOAT[384] — both verified in Phase 0.5.
func vecToAny(v []float32) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// anyToVec converts a scanned array value back into a float32 slice. Elements
// come back as float32 inside a []any.
func anyToVec(v any) ([]float32, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("embedding scanned as %T, want []any", v)
	}
	out := make([]float32, len(raw))
	for i, x := range raw {
		f, ok := x.(float32)
		if !ok {
			return nil, fmt.Errorf("embedding element %d is %T, want float32", i, x)
		}
		out[i] = f
	}
	return out, nil
}
