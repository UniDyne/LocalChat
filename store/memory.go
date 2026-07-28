package store

import (
	"database/sql"
	"fmt"
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
CREATE INDEX IF NOT EXISTS idx_memory_chunks_source ON memory_chunks (source_id);
CREATE INDEX IF NOT EXISTS idx_memory_sources_session ON memory_sources (session_id);
CREATE INDEX IF NOT EXISTS idx_memory_sources_ref ON memory_sources (source_type, source_ref);
CREATE INDEX IF NOT EXISTS idx_memory_chunk_terms_term ON memory_chunk_terms (term);
CREATE INDEX IF NOT EXISTS idx_memory_chunk_entities_entity ON memory_chunk_entities (entity_id);
CREATE INDEX IF NOT EXISTS idx_memory_edges_dst ON memory_edges (dst_chunk_id);
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
const MemorySchemaVersion = "1"

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
	IngestedAt  string `json:"ingestedAt"`
	TokenCount  int    `json:"tokenCount"`
	EntityPass  string `json:"entityPass"`
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
		       mtime, ingested_at, token_count, entity_pass
		FROM memory_sources WHERE source_type = ? AND source_ref = ?`,
		sourceType, sourceRef,
	).Scan(&src.ID, &src.SourceType, &src.SourceRef, &src.SessionID, &src.Title,
		&src.Path, &src.ContentHash, &mtime, &src.IngestedAt, &src.TokenCount, &src.EntityPass)
	if err == sql.ErrNoRows {
		return MemorySource{}, false, nil
	}
	if err != nil {
		return MemorySource{}, false, fmt.Errorf("find source: %w", err)
	}
	src.MTime = mtime.String
	return src, true, nil
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
			 mtime, ingested_at, token_count, entity_pass)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, src.SourceType, src.SourceRef, src.SessionID, src.Title, src.Path,
		src.ContentHash, mtime, src.IngestedAt, src.TokenCount, src.EntityPass,
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
	if err := insertChunkEntitiesTx(tx, chunks); err != nil {
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
				(id, source_id, ord, text, heading_path, token_count, char_len, embedding, embed_model, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?::FLOAT[384], ?, ?)`,
			c.ID, c.SourceID, c.Ord, c.Text, c.HeadingPath, c.TokenCount, c.CharLen,
			vecToAny(c.Embedding), c.EmbedModel, c.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert chunk %s: %w", c.ID, err)
		}
	}

	return execBatches(tx,
		`memory_chunks (id, source_id, ord, text, heading_path, token_count, char_len, embed_model, created_at)`,
		9, len(plain),
		func(i int) []any {
			c := plain[i]
			return []any{c.ID, c.SourceID, c.Ord, c.Text, c.HeadingPath,
				c.TokenCount, c.CharLen, c.EmbedModel, c.CreatedAt}
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
func insertChunkEntitiesTx(tx *sql.Tx, chunks []MemoryChunk) error {
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
	return execBatches(tx, "memory_chunk_entities (chunk_id, entity_id, count, extractor)", 4, len(links),
		func(i int) []any {
			return []any{links[i].chunkID, links[i].entityID, links[i].count, links[i].extractor}
		})
}

// execBatches issues multi-row INSERTs over n logical rows, cols values each,
// obtaining each row's arguments from argsAt.
func execBatches(tx *sql.Tx, target string, cols, n int, argsAt func(i int) []any) error {
	if n == 0 {
		return nil
	}
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	for start := 0; start < n; start += batchRows {
		end := min(start+batchRows, n)
		count := end - start
		tuples := make([]string, count)
		args := make([]any, 0, count*cols)
		for i := 0; i < count; i++ {
			tuples[i] = tuple
			args = append(args, argsAt(start+i)...)
		}
		q := `INSERT INTO ` + target + ` VALUES ` + strings.Join(tuples, ",")
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

// SetSourceEntityPass records progress of the background LLM entity pass so an
// interrupted run resumes instead of restarting.
func (s *Store) SetSourceEntityPass(sourceID, state string) error {
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
		SELECT id, source_id, ord, text, heading_path, token_count
		FROM memory_chunks WHERE embedding IS NULL ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unembedded chunks: %w", err)
	}
	defer rows.Close()

	out := make([]MemoryChunk, 0)
	for rows.Next() {
		var c MemoryChunk
		if err := rows.Scan(&c.ID, &c.SourceID, &c.Ord, &c.Text, &c.HeadingPath, &c.TokenCount); err != nil {
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

// ClearEmbeddings nulls every vector, for use when the embedding model changes.
// Chunks and their term/entity data are kept — only the vector space is invalid.
func (s *Store) ClearEmbeddings() error {
	_, err := s.db.Exec(`UPDATE memory_chunks SET embedding = NULL, embed_model = ''`)
	if err != nil {
		return fmt.Errorf("clear embeddings: %w", err)
	}
	return nil
}

// ---------- edges ----------

// InsertEdges upserts graph edges.
func (s *Store) InsertEdges(edges []MemoryEdge) error {
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
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM memory_terms`); err != nil {
		return 0, 0, fmt.Errorf("clear terms: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO memory_terms (term, df)
		SELECT term, COUNT(DISTINCT chunk_id) FROM memory_chunk_terms GROUP BY term`); err != nil {
		return 0, 0, fmt.Errorf("rebuild df: %w", err)
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
