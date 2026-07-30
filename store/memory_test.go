package store

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// Phase 1 exit criteria: create/upsert/delete of sources and chunks, a re-ingest
// exercising the delete-then-reinsert path, and a cascade test asserting no
// orphans, no dangling edges on either side, and BM25 statistics equal to a full
// recount.

func openMemoryStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	// openStoreAt applies the memory schema and its migrations too, via applySchema.
	return openStoreAt(t, path)
}

// chunk builds a chunk with terms derived from its words, so BM25 bookkeeping
// has something realistic to count.
func chunk(text string, entities ...ChunkEntity) MemoryChunk {
	terms := map[string]int{}
	word := ""
	for _, r := range text + " " {
		if r == ' ' || r == '\n' || r == '.' || r == ',' {
			if word != "" {
				terms[word]++
				word = ""
			}
			continue
		}
		word += string(r)
	}
	n := 0
	for _, c := range terms {
		n += c
	}
	return MemoryChunk{
		Text:       text,
		TokenCount: n,
		Terms:      terms,
		Entities:   entities,
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSchemaDimensionMatchesConstant(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	var typ string
	if err := s.db.QueryRow(`
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'memory_chunks' AND column_name = 'embedding'`).Scan(&typ); err != nil {
		t.Fatalf("introspect embedding column: %v", err)
	}
	want := fmt.Sprintf("FLOAT[%d]", EmbedDim)
	if typ != want {
		t.Errorf("embedding column is %s but EmbedDim says %s — these must agree", typ, want)
	}
	t.Logf("embedding column type = %s", typ)
}

func TestReplaceSourceRoundTrip(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	src := MemorySource{
		SourceType:  SourceDirectory,
		SourceRef:   "/vault/note.md",
		Path:        "/vault/note.md",
		Title:       "Note",
		ContentHash: "hash-v1",
	}
	chunks := []MemoryChunk{
		chunk("duckdb stores the chunks", ChunkEntity{Kind: "tag", ValueNorm: "database", Extractor: "tag"}),
		chunk("leiden groups sentences by topic"),
	}
	id, err := s.ReplaceSource(src, chunks)
	if err != nil {
		t.Fatalf("ReplaceSource: %v", err)
	}
	if id == "" {
		t.Fatal("empty source id")
	}

	got, found, err := s.FindSource(SourceDirectory, "/vault/note.md")
	if err != nil || !found {
		t.Fatalf("FindSource: found=%v err=%v", found, err)
	}
	if got.ContentHash != "hash-v1" || got.EntityPass != EntityPassPending {
		t.Errorf("source round-trip wrong: %+v", got)
	}
	if got.TokenCount == 0 {
		t.Error("token_count not aggregated from chunks")
	}

	back, err := s.GetChunksBySource(id)
	if err != nil {
		t.Fatalf("GetChunksBySource: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("got %d chunks, want 2", len(back))
	}
	if back[0].Ord != 1 || back[1].Ord != 2 {
		t.Errorf("ord not assigned in order: %d, %d", back[0].Ord, back[1].Ord)
	}
	if back[0].Embedding != nil {
		t.Error("chunk should have no embedding yet")
	}
	if back[0].CharLen == 0 {
		t.Error("char_len not derived")
	}
	if countRows(t, s, "memory_entities") != 1 {
		t.Error("entity not created")
	}
	if countRows(t, s, "memory_chunk_entities") != 1 {
		t.Error("chunk-entity link not created")
	}
}

// TestReplaceSourceReingest is the delete-then-reinsert path. Since DuckDB 1.4.1
// this runs in a single transaction (see TestARTDeleteReinsertSameTx); the test
// stays as the tripwire if that behavior ever regresses.
func TestReplaceSourceReingest(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	src := MemorySource{SourceType: SourceDirectory, SourceRef: "/vault/a.md", ContentHash: "v1"}
	first := []MemoryChunk{chunk("original alpha text"), chunk("original beta text")}
	id1, err := s.ReplaceSource(src, first)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Re-ingest the same ref with different content, repeatedly — the pattern a
	// watched-directory rescan produces.
	for i := 0; i < 3; i++ {
		src.ContentHash = fmt.Sprintf("v%d", i+2)
		id2, err := s.ReplaceSource(src, []MemoryChunk{chunk("replacement gamma text")})
		if err != nil {
			t.Fatalf("re-ingest %d: %v", i, err)
		}
		if id2 != id1 {
			t.Errorf("re-ingest changed source id: %s -> %s", id1, id2)
		}
	}

	if n := countRows(t, s, "memory_sources"); n != 1 {
		t.Errorf("sources = %d, want 1", n)
	}
	back, err := s.GetChunksBySource(id1)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Text != "replacement gamma text" {
		t.Fatalf("stale chunks survived re-ingest: %+v", back)
	}
	// The old chunks' term rows must be gone, not merely orphaned.
	if n := countRows(t, s, "memory_chunk_terms"); n != 3 {
		t.Errorf("chunk_terms = %d, want 3 (only the surviving chunk's)", n)
	}

	got, _, err := s.FindSource(SourceDirectory, "/vault/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != "v4" {
		t.Errorf("content_hash = %q, want v4", got.ContentHash)
	}
}

func TestEmbeddingWriteAndSQLRanking(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	r := rand.New(rand.NewSource(7))
	src := MemorySource{SourceType: SourceDirectory, SourceRef: "/vault/vec.md"}
	chunks := make([]MemoryChunk, 5)
	vecs := make([][]float32, 5)
	for i := range chunks {
		chunks[i] = chunk(fmt.Sprintf("chunk number %d", i))
		vecs[i] = randUnitVec(r, EmbedDim)
	}
	id, err := s.ReplaceSource(src, chunks)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := s.GetChunksBySource(id)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := s.ChunksMissingEmbedding(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 5 {
		t.Fatalf("ChunksMissingEmbedding = %d, want 5", len(missing))
	}

	byID := map[string][]float32{}
	for i, c := range stored {
		byID[c.ID] = vecs[i]
	}
	if err := s.SetChunkEmbeddings("bge-small-en-v1.5", byID); err != nil {
		t.Fatalf("SetChunkEmbeddings: %v", err)
	}
	if missing, err = s.ChunksMissingEmbedding(100); err != nil || len(missing) != 0 {
		t.Fatalf("after embedding: missing=%d err=%v", len(missing), err)
	}

	// Rank in SQL against chunk 2's own vector: it must come back first.
	target := stored[2].ID
	var gotID string
	var d float64
	if err := s.db.QueryRow(`
		SELECT id, array_cosine_distance(embedding, ?::FLOAT[384]) AS d
		FROM memory_chunks WHERE embedding IS NOT NULL ORDER BY d LIMIT 1`,
		vecToAny(vecs[2]),
	).Scan(&gotID, &d); err != nil {
		t.Fatalf("SQL ranking: %v", err)
	}
	if gotID != target {
		t.Errorf("nearest = %s, want %s", gotID, target)
	}
	if d > 1e-5 {
		t.Errorf("self-distance = %v, want ~0", d)
	}

	// Round-trip a vector back out to confirm fidelity through the store layer.
	var raw any
	if err := s.db.QueryRow(`SELECT embedding FROM memory_chunks WHERE id = ?`, target).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	back, err := anyToVec(raw)
	if err != nil {
		t.Fatalf("anyToVec: %v", err)
	}
	if len(back) != EmbedDim {
		t.Fatalf("round-trip dims = %d", len(back))
	}
	for i := range back {
		if diff := back[i] - vecs[2][i]; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("element %d drifted: %v vs %v", i, back[i], vecs[2][i])
		}
	}

	if err := s.ClearEmbeddings(); err != nil {
		t.Fatal(err)
	}
	if missing, err = s.ChunksMissingEmbedding(100); err != nil || len(missing) != 5 {
		t.Errorf("after ClearEmbeddings: missing=%d err=%v", len(missing), err)
	}
}

func TestBM25StatsRecomputeOnDirty(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	src := MemorySource{SourceType: SourceDirectory, SourceRef: "/vault/bm25.md"}
	if _, err := s.ReplaceSource(src, []MemoryChunk{
		chunk("duckdb duckdb analytics"),
		chunk("duckdb embedded database"),
		chunk("leiden community detection"),
	}); err != nil {
		t.Fatal(err)
	}

	n, avgdl, err := s.BM25Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("N = %d, want 3", n)
	}
	if avgdl <= 0 {
		t.Errorf("avgdl = %v, want > 0", avgdl)
	}

	df, err := s.TermDF([]string{"duckdb", "leiden", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	// "duckdb" appears in 2 chunks (df counts distinct chunks, not occurrences).
	if df["duckdb"] != 2 {
		t.Errorf("df[duckdb] = %d, want 2", df["duckdb"])
	}
	if df["leiden"] != 1 {
		t.Errorf("df[leiden] = %d, want 1", df["leiden"])
	}
	if _, ok := df["absent"]; ok {
		t.Error("df returned an entry for a term not in the corpus")
	}

	// Cached path: not dirty, so values come from meta.
	dirty, _ := s.GetMeta(MetaStatsDirty)
	if dirty != "0" {
		t.Errorf("stats_dirty = %q after recompute, want 0", dirty)
	}
	n2, _, err := s.BM25Stats()
	if err != nil || n2 != 3 {
		t.Errorf("cached stats wrong: n=%d err=%v", n2, err)
	}
}

// TestSessionCascade is the Phase 1 gate. It asserts the three things §3.4 warns
// are easy to get wrong: no orphaned rows, no edges referencing removed chunks
// from EITHER side, and BM25 statistics matching a full recount.
func TestSessionCascade(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	sessionID, err := s.CreateSession("Doomed session")
	if err != nil {
		t.Fatal(err)
	}
	keepSession, err := s.CreateSession("Surviving session")
	if err != nil {
		t.Fatal(err)
	}

	artifactID, err := s.CreateArtifact(sessionID, "Doomed artifact", "content", "markdown")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveMessage(sessionID, NewMessage{Role: "user", Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	// Three sources: a conversation turn and an artifact from the doomed
	// session, plus a directory note that must survive.
	convID, err := s.ReplaceSource(MemorySource{
		SourceType: SourceConversation, SourceRef: sessionID + "#1-2", SessionID: sessionID,
	}, []MemoryChunk{chunk("doomed conversation shared alpha")})
	if err != nil {
		t.Fatal(err)
	}
	artID, err := s.ReplaceSource(MemorySource{
		SourceType: SourceArtifact, SourceRef: artifactID, SessionID: sessionID,
	}, []MemoryChunk{chunk("doomed artifact shared alpha")})
	if err != nil {
		t.Fatal(err)
	}
	dirID, err := s.ReplaceSource(MemorySource{
		SourceType: SourceDirectory, SourceRef: "/vault/keep.md",
	}, []MemoryChunk{chunk("surviving vault note shared alpha")})
	if err != nil {
		t.Fatal(err)
	}
	// A source on the surviving session, to prove the cascade is scoped.
	if _, err := s.ReplaceSource(MemorySource{
		SourceType: SourceConversation, SourceRef: keepSession + "#1-2", SessionID: keepSession,
	}, []MemoryChunk{chunk("other session note")}); err != nil {
		t.Fatal(err)
	}

	convChunks, _ := s.GetChunksBySource(convID)
	artChunks, _ := s.GetChunksBySource(artID)
	dirChunks, _ := s.GetChunksBySource(dirID)

	// Edges in both directions between doomed and surviving chunks. The inbound
	// one — surviving -> doomed — is what a naive cascade leaves dangling.
	if err := s.InsertEdges([]MemoryEdge{
		{SrcChunkID: convChunks[0].ID, DstChunkID: dirChunks[0].ID, Kind: "similar", Weight: 0.8},
		{SrcChunkID: dirChunks[0].ID, DstChunkID: convChunks[0].ID, Kind: "similar", Weight: 0.8},
		{SrcChunkID: dirChunks[0].ID, DstChunkID: artChunks[0].ID, Kind: "link", Weight: 1.0},
		{SrcChunkID: convChunks[0].ID, DstChunkID: artChunks[0].ID, Kind: "entity", Weight: 0.3},
	}); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "memory_edges") != 4 {
		t.Fatalf("expected 4 edges before delete, got %d", countRows(t, s, "memory_edges"))
	}

	if _, _, err := s.RecomputeBM25Stats(); err != nil {
		t.Fatal(err)
	}

	// --- the cascade ---
	if err := s.DeleteSession(sessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Session-derived memory is gone.
	if _, found, _ := s.FindSource(SourceConversation, sessionID+"#1-2"); found {
		t.Error("conversation source survived session delete")
	}
	if _, found, _ := s.FindSource(SourceArtifact, artifactID); found {
		t.Error("artifact source survived session delete")
	}
	// Directory memory untouched.
	if _, found, _ := s.FindSource(SourceDirectory, "/vault/keep.md"); !found {
		t.Error("directory source was deleted — it has no session and must survive")
	}
	// Other session's memory untouched.
	if _, found, _ := s.FindSource(SourceConversation, keepSession+"#1-2"); !found {
		t.Error("another session's memory was deleted — cascade is not scoped")
	}

	// Artifacts cascade (the pre-existing orphaning bug).
	if _, err := s.GetArtifact(artifactID); err == nil {
		t.Error("artifact survived session delete — cascade missing")
	}

	// No orphaned chunk rows.
	var orphanChunks int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM memory_chunks
		WHERE source_id NOT IN (SELECT id FROM memory_sources)`).Scan(&orphanChunks); err != nil {
		t.Fatal(err)
	}
	if orphanChunks != 0 {
		t.Errorf("%d orphaned chunks", orphanChunks)
	}

	// No orphaned term/entity links.
	for _, q := range []struct{ name, sql string }{
		{"chunk_terms", `SELECT COUNT(*) FROM memory_chunk_terms WHERE chunk_id NOT IN (SELECT id FROM memory_chunks)`},
		{"chunk_entities", `SELECT COUNT(*) FROM memory_chunk_entities WHERE chunk_id NOT IN (SELECT id FROM memory_chunks)`},
		{"entities", `SELECT COUNT(*) FROM memory_entities WHERE id NOT IN (SELECT entity_id FROM memory_chunk_entities)`},
		{"terms", `SELECT COUNT(*) FROM memory_terms WHERE term NOT IN (SELECT term FROM memory_chunk_terms)`},
	} {
		var n int
		if err := s.db.QueryRow(q.sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if n != 0 {
			t.Errorf("%d orphaned %s rows", n, q.name)
		}
	}

	// No dangling edges on EITHER side.
	var danglingSrc, danglingDst int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM memory_edges
		WHERE src_chunk_id NOT IN (SELECT id FROM memory_chunks)`).Scan(&danglingSrc); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM memory_edges
		WHERE dst_chunk_id NOT IN (SELECT id FROM memory_chunks)`).Scan(&danglingDst); err != nil {
		t.Fatal(err)
	}
	if danglingSrc != 0 || danglingDst != 0 {
		t.Errorf("dangling edges: %d by src, %d by dst", danglingSrc, danglingDst)
	}
	if n := countRows(t, s, "memory_edges"); n != 0 {
		t.Errorf("expected all 4 edges removed (every one touched a doomed chunk), got %d", n)
	}

	// BM25 statistics must match a full recount, not the pre-delete values.
	nCached, avgCached, err := s.BM25Stats()
	if err != nil {
		t.Fatal(err)
	}
	nFresh, avgFresh, err := s.RecomputeBM25Stats()
	if err != nil {
		t.Fatal(err)
	}
	if nCached != nFresh {
		t.Errorf("BM25 N stale after cascade: %d, full recount says %d", nCached, nFresh)
	}
	if diff := avgCached - avgFresh; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("BM25 avgdl stale after cascade: %v vs %v", avgCached, avgFresh)
	}
	if nFresh != 2 {
		t.Errorf("surviving chunks = %d, want 2 (directory + other session)", nFresh)
	}

	// "shared" appeared in all three original chunks; only the directory one
	// survives, so df must have dropped to 1 rather than staying at 3.
	df, err := s.TermDF([]string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if df["shared"] != 1 {
		t.Errorf("df[shared] = %d after cascade, want 1 — IDF has drifted", df["shared"])
	}
}

// TestOrphanedArtifactCleanup covers the one-time repair for artifacts orphaned
// by the pre-cascade DeleteSession.
func TestOrphanedArtifactCleanup(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	sessionID, err := s.CreateSession("Gone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateArtifact(sessionID, "orphan", "body", "text"); err != nil {
		t.Fatal(err)
	}
	// Simulate the old behavior: delete the session row directly, leaving the
	// artifact behind.
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		t.Fatal(err)
	}
	if countRows(t, s, "artifacts") != 1 {
		t.Fatal("expected the orphan to exist before cleanup")
	}

	n, err := s.cleanupOrphanedArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleanup reported %d rows, want 1", n)
	}
	if countRows(t, s, "artifacts") != 0 {
		t.Error("orphan survived cleanup")
	}

	// Idempotent: a second run removes nothing.
	if n, err := s.cleanupOrphanedArtifacts(); err != nil || n != 0 {
		t.Errorf("second cleanup: n=%d err=%v", n, err)
	}
}

func TestInitMemoryMetaDetectsModelChange(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	changed, err := s.InitMemoryMeta("bge-small-en-v1.5", 384)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("first init reported a change")
	}
	if changed, err = s.InitMemoryMeta("bge-small-en-v1.5", 384); err != nil || changed {
		t.Errorf("same model reported changed=%v err=%v", changed, err)
	}
	if changed, err = s.InitMemoryMeta("nomic-embed-text", 768); err != nil || !changed {
		t.Errorf("different model reported changed=%v err=%v — vector spaces would be silently mixed", changed, err)
	}
}

func TestEdgeBidirectionalTraversal(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	id, err := s.ReplaceSource(
		MemorySource{SourceType: SourceDirectory, SourceRef: "/vault/edges.md"},
		[]MemoryChunk{chunk("alpha"), chunk("beta"), chunk("gamma")})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := s.GetChunksBySource(id)

	// beta -> alpha is a link; gamma -> alpha is similar.
	if err := s.InsertEdges([]MemoryEdge{
		{SrcChunkID: cs[1].ID, DstChunkID: cs[0].ID, Kind: "link", Weight: 1},
		{SrcChunkID: cs[2].ID, DstChunkID: cs[0].ID, Kind: "similar", Weight: 0.9},
	}); err != nil {
		t.Fatal(err)
	}

	// From alpha with no bidirectional kinds: nothing leaves alpha.
	out, err := s.GetEdgesFrom(cs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("expected no outbound edges from alpha, got %d", len(out))
	}

	// Treating link as bidirectional surfaces the backlink from beta, but not
	// the similar edge from gamma.
	out, err = s.GetEdgesFrom(cs[0].ID, "link")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 backlink, got %d: %+v", len(out), out)
	}
	if out[0].SrcChunkID != cs[0].ID || out[0].DstChunkID != cs[1].ID {
		t.Errorf("backlink not reoriented for the walker: %+v", out[0])
	}
}

func TestClearMemoryLeavesSessionsIntact(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	sessionID, err := s.CreateSession("Keep me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveMessage(sessionID, NewMessage{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateArtifact(sessionID, "keep", "body", "text"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceSource(
		MemorySource{SourceType: SourceConversation, SourceRef: sessionID + "#1-1", SessionID: sessionID},
		[]MemoryChunk{chunk("some memory", ChunkEntity{Kind: "tag", ValueNorm: "x"})}); err != nil {
		t.Fatal(err)
	}

	if err := s.ClearMemory(); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"memory_sources", "memory_chunks", "memory_chunk_terms",
		"memory_chunk_entities", "memory_entities", "memory_terms", "memory_edges"} {
		if n := countRows(t, s, tbl); n != 0 {
			t.Errorf("%s still has %d rows after ClearMemory", tbl, n)
		}
	}
	if n := countRows(t, s, "messages"); n != 1 {
		t.Errorf("messages = %d, want 1 — ClearMemory must not touch chat data", n)
	}
	if n := countRows(t, s, "artifacts"); n != 1 {
		t.Errorf("artifacts = %d, want 1", n)
	}
}

func TestMemoryStats(t *testing.T) {
	s := openMemoryStore(t)
	defer s.Close()

	if _, err := s.InitMemoryMeta("bge-small-en-v1.5", EmbedDim); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceSource(
		MemorySource{SourceType: SourceDirectory, SourceRef: "/vault/s.md"},
		[]MemoryChunk{
			chunk("one two three", ChunkEntity{Kind: "tag", ValueNorm: "t1"}),
			chunk("four five"),
		}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecomputeBM25Stats(); err != nil {
		t.Fatal(err)
	}

	st, err := s.MemoryStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sources != 1 || st.Chunks != 2 || st.EmbeddedChunks != 0 {
		t.Errorf("counts wrong: %+v", st)
	}
	if st.Entities != 1 || st.Terms != 5 {
		t.Errorf("entity/term counts wrong: %+v", st)
	}
	if st.PendingEntities != 1 {
		t.Errorf("PendingEntities = %d, want 1", st.PendingEntities)
	}
	if st.EmbedModel != "bge-small-en-v1.5" || st.EmbedDims != EmbedDim {
		t.Errorf("model meta wrong: %+v", st)
	}
	if st.AvgDL <= 0 {
		t.Errorf("AvgDL = %v", st.AvgDL)
	}
}

// TestMigrationFromPreviousSchema is the upgrade path an existing user takes: a
// database whose memory tables were created before Phase 7 added columns.
//
// TestSchemaMigrationIsIdempotent (in the memory package) covers reopening a
// current-schema file. This covers the case that actually breaks — the ALTER running
// against tables that already exist *with rows in them* — which no other test
// reaches, because every other test creates its tables at the current version.
func TestMigrationFromPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}

	// The pre-Phase-7 shape: memory_sources without file_size, memory_chunks without
	// the two context columns. Written out rather than derived from memorySchemaSQL so
	// the test keeps describing the old schema even as the current one moves on.
	const oldSchema = `
CREATE TABLE memory_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE memory_sources (
	id TEXT PRIMARY KEY, source_type TEXT NOT NULL, source_ref TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL DEFAULT '',
	mtime TIMESTAMP, ingested_at TIMESTAMP NOT NULL,
	token_count INTEGER NOT NULL DEFAULT 0,
	entity_pass TEXT NOT NULL DEFAULT 'pending');
CREATE TABLE memory_chunks (
	id TEXT PRIMARY KEY, source_id TEXT NOT NULL, ord INTEGER NOT NULL,
	text TEXT NOT NULL, heading_path TEXT NOT NULL DEFAULT '',
	token_count INTEGER NOT NULL DEFAULT 0, char_len INTEGER NOT NULL DEFAULT 0,
	embedding FLOAT[384], embed_model TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL);
`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	// Existing data, so the migration is exercised against populated tables.
	if _, err := db.Exec(`
		INSERT INTO memory_sources (id, source_type, source_ref, ingested_at)
		VALUES ('s1', 'directory', 'Note.md', now());
		INSERT INTO memory_chunks (id, source_id, ord, text, created_at)
		VALUES ('c1', 's1', 1, 'existing chunk text', now());`); err != nil {
		t.Fatalf("seed old data: %v", err)
	}
	db.Close()

	// Reopen through the real path.
	s, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open a pre-Phase-7 database: %v", err)
	}
	defer s.Close()

	// The pre-existing row must survive, with the new columns at their defaults.
	src, found, err := s.FindSource(SourceDirectory, "Note.md")
	if err != nil {
		t.Fatalf("read a migrated source: %v", err)
	}
	if !found {
		t.Fatal("the pre-existing source did not survive the migration")
	}
	if src.FileSize != 0 {
		t.Errorf("FileSize = %d, want the 0 default for a row written before the column existed", src.FileSize)
	}

	chunks, err := s.ChunksMissingEmbedding(10)
	if err != nil {
		t.Fatalf("read migrated chunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Text != "existing chunk text" {
		t.Fatalf("pre-existing chunk lost: %+v", chunks)
	}
	if chunks[0].ThreadContext != "" || chunks[0].CotContext != "" {
		t.Errorf("new context columns should default to empty, got %q / %q",
			chunks[0].ThreadContext, chunks[0].CotContext)
	}

	// And the new columns must be writable, not merely present.
	if _, err := s.ReplaceSource(MemorySource{
		SourceType: SourceConversation, SourceRef: "s#1-2", SessionID: "s", FileSize: 7,
	}, []MemoryChunk{{Text: "new", ThreadContext: "t", CotContext: "c", TokenCount: 1}}); err != nil {
		t.Fatalf("write through the migrated columns: %v", err)
	}
}
