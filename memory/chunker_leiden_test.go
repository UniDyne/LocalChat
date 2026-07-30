package memory

import (
	"context"
	"strings"
	"testing"
)

func lexicalCfg() LeidenChunkerConfig {
	return LeidenChunkerConfig{
		Graph:      DefaultGraphParams(),
		Resolution: DefaultResolution,
		Similarity: LexicalSimilarity{},
	}
}

// multiTopicDoc is prose with three clearly distinct topics under one heading, so a
// topical chunker has something real to find that heading-splitting alone cannot.
const multiTopicDoc = `# Engineering Notes

The database stores every session and chunk inside an embedded DuckDB file.
DuckDB is columnar, so single-row inserts are slow and writes must be batched.
The storage layer keeps all queries behind typed methods for consistency.
Vector similarity runs in SQL using array_cosine_distance over fixed-width arrays.

Deployment happens through a Wails build that embeds the frontend assets.
The installer must ship the ONNX Runtime shared library alongside the binary.
On macOS the dylib has to be codesigned with the same team identity as the app.
Windows packaging uses an NSIS template that needs the extra file registered.

My cat Biscuit sits on the keyboard whenever a long build is running.
She has knocked three pens off the desk this week and shows no remorse.
Feeding her early does not help, and she prefers the warm laptop vent.
`

func TestChunkLeidenBasics(t *testing.T) {
	tc := NewHeuristicCounter()
	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc), tc, lexicalCfg())
	if err != nil {
		t.Fatalf("ChunkLeiden: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	for i, c := range chunks {
		if c.TokenCount > MaxChunkTokens {
			t.Errorf("chunk %d over budget: %d > %d", i, c.TokenCount, MaxChunkTokens)
		}
		if strings.TrimSpace(c.Text) == "" {
			t.Errorf("chunk %d is empty", i)
		}
		if c.HeadingPath != "Engineering Notes" {
			t.Errorf("chunk %d heading path = %q", i, c.HeadingPath)
		}
	}
	t.Logf("produced %d chunks from %d sentences", len(chunks), len(SplitSentences(multiTopicDoc)))
}

// TestChunkLeidenSeparatesTopicsWithRealModel is the test that actually exercises
// topical chunking, because only the real embeddings carry enough signal: the
// lexical arm measures AUC 0.513 for separating same-topic from cross-topic
// sentence pairs, against 0.875 for bge-small.
func TestChunkLeidenSeparatesTopicsWithRealModel(t *testing.T) {
	paths, err := FindModel()
	if err != nil {
		t.Skipf("model not provisioned: %v", err)
	}
	emb, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 2})
	if err != nil {
		t.Skipf("onnx embedder unavailable: %v", err)
	}
	defer emb.Close()

	tc := NewHeuristicCounter()
	cfg := LeidenChunkerConfig{
		Graph:      DefaultGraphParams(),
		Resolution: DefaultResolution,
		Similarity: SemanticSimilarity{Embedder: emb},
	}
	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc), tc, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The document has three clearly distinct topics: storage, deployment, and a
	// cat. A topical chunker should not put the cat with the database.
	for i, c := range chunks {
		hasDB := strings.Contains(c.Text, "DuckDB") || strings.Contains(c.Text, "columnar")
		hasCat := strings.Contains(c.Text, "Biscuit") || strings.Contains(c.Text, "cat ")
		if hasDB && hasCat {
			t.Errorf("chunk %d mixes unrelated topics: %q", i, truncStr(c.Text, 150))
		}
	}
	if len(chunks) < 3 {
		t.Errorf("three distinct topics produced only %d chunk(s) — the small-chunk merge "+
			"must not join two communities", len(chunks))
	}
	// And the cat must not have been merged into the deployment topic either.
	for i, c := range chunks {
		hasDeploy := strings.Contains(c.Text, "NSIS") || strings.Contains(c.Text, "codesigned")
		hasCat := strings.Contains(c.Text, "Biscuit")
		if hasDeploy && hasCat {
			t.Errorf("chunk %d merged unrelated communities: %q", i, truncStr(c.Text, 150))
		}
	}
	t.Logf("real model produced %d chunks for 3 topics", len(chunks))
	for i, c := range chunks {
		t.Logf("  chunk %d [sent %d..%d]: %s", i, c.SentFrom, c.SentTo, truncStr(stripHeading(c.Text), 70))
	}
}

func stripHeading(text string) string {
	lines := strings.SplitN(text, "\n", 2)
	if len(lines) == 2 {
		if _, _, ok := atxHeading(strings.TrimSpace(lines[0])); ok {
			return strings.TrimSpace(lines[1])
		}
	}
	return text
}

// TestChunkLeidenDeterministic is the requirement that makes incremental re-ingest
// viable: identical input must give byte-identical chunks.
func TestChunkLeidenDeterministic(t *testing.T) {
	tc := NewHeuristicCounter()
	blocks := ParseBlocks(multiTopicDoc)

	first, err := ChunkLeiden(context.Background(), blocks, tc, lexicalCfg())
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 4; run++ {
		again, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc), tc, lexicalCfg())
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d chunks, first produced %d", run, len(again), len(first))
		}
		for i := range first {
			if again[i].Text != first[i].Text {
				t.Fatalf("run %d chunk %d differs:\n got %q\nwant %q", run, i,
					truncStr(again[i].Text, 80), truncStr(first[i].Text, 80))
			}
		}
	}
}

// TestChunkLeidenContiguity is the property the contiguous-run post-processing
// exists to guarantee: a chunk must be a consecutive span of the document, not a
// topical gather-up of scattered sentences.
//
// Asserted against the sentence ranges the chunker records, rather than by
// re-splitting the joined chunk text — that approach silently drops sentences the
// splitter regroups differently and produces phantom gaps.
func TestChunkLeidenContiguity(t *testing.T) {
	tc := NewHeuristicCounter()
	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc), tc, lexicalCfg())
	if err != nil {
		t.Fatal(err)
	}

	lastEnd := 0
	for i, c := range chunks {
		if c.SentTo <= c.SentFrom {
			t.Errorf("chunk %d has an empty sentence range [%d,%d)", i, c.SentFrom, c.SentTo)
			continue
		}
		if c.SentFrom < lastEnd {
			t.Errorf("chunk %d range [%d,%d) overlaps the previous chunk ending at %d",
				i, c.SentFrom, c.SentTo, lastEnd)
		}
		if c.SentFrom > lastEnd {
			t.Errorf("chunk %d range [%d,%d) skips sentences from %d — content was dropped",
				i, c.SentFrom, c.SentTo, lastEnd)
		}
		lastEnd = c.SentTo
	}
	// Every sentence must be covered: the merge step must not drop content, and a
	// stale range would hide it.
	var total int
	for _, b := range ParseBlocks(multiTopicDoc) {
		if b.Kind == BlockHeading || b.Kind.Atomic() || b.Kind == BlockThematic {
			continue
		}
		total += len(SplitSentences(b.Text))
	}
	if lastEnd != total {
		t.Errorf("chunks cover sentences [0,%d) but the document has %d — content lost",
			lastEnd, total)
	}
	t.Logf("%d chunks covering all %d sentences contiguously", len(chunks), total)
}

// TestChunkLeidenNeverSplitsAtomicBlocks carries the Phase 2 guarantee through the
// new chunker: statistical clustering must not be allowed to cut a code fence.
func TestChunkLeidenNeverSplitsAtomicBlocks(t *testing.T) {
	tc := NewHeuristicCounter()
	doc := "# Doc\n\nSome intro prose that is long enough to matter here.\n" +
		"A second sentence about an unrelated topic entirely for contrast.\n\n" +
		"```go\n" + strings.Repeat("// a line of code inside the fence\n", 30) + "```\n\n" +
		"| col | val |\n|---|---|\n| a | 1 |\n| b | 2 |\n\n" +
		"Trailing prose after the atomic blocks.\n"

	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(doc), tc, lexicalCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if n := unclosedFences(c.Text); n != 0 {
			t.Errorf("chunk leaves %d fence(s) open: %q", n, truncStr(c.Text, 100))
		}
	}
	fenceChunks, tableChunks := 0, 0
	for _, c := range chunks {
		if strings.Contains(c.Text, "a line of code inside the fence") {
			fenceChunks++
		}
		if strings.Contains(c.Text, "| a | 1 |") {
			tableChunks++
			if !strings.Contains(c.Text, "| b | 2 |") {
				t.Error("table rows split across chunks")
			}
		}
	}
	if fenceChunks != 1 {
		t.Errorf("code fence spread across %d chunks, want 1", fenceChunks)
	}
	if tableChunks != 1 {
		t.Errorf("table spread across %d chunks, want 1", tableChunks)
	}
}

// TestChunkLeidenRespectsHeadings confirms a statistical method is not permitted to
// cross a boundary the author chose deliberately.
func TestChunkLeidenRespectsHeadings(t *testing.T) {
	tc := NewHeuristicCounter()
	doc := `# Top

## Storage

The database keeps chunks in a columnar store with batched writes for speed.
Single row inserts are slow because per statement overhead dominates the work.

## Retrieval

Four scoring signals are fused together to rank the candidate chunks.
The graph walk expands beyond what direct scoring alone manages to find.
`
	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(doc), tc, lexicalCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		hasStorage := strings.Contains(c.Text, "columnar store")
		hasRetrieval := strings.Contains(c.Text, "scoring signals")
		if hasStorage && hasRetrieval {
			t.Errorf("a chunk spans two headings: %q", truncStr(c.Text, 120))
		}
	}
	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.HeadingPath] = true
	}
	for _, want := range []string{"Top › Storage", "Top › Retrieval"} {
		if !paths[want] {
			t.Errorf("missing heading path %q (got %v)", want, keys(paths))
		}
	}
}

func TestChunkLeidenRequiresSimilarity(t *testing.T) {
	_, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc),
		NewHeuristicCounter(), LeidenChunkerConfig{})
	if err == nil {
		t.Error("expected an error when no similarity function is configured")
	}
}

func TestChunkLeidenSemanticArm(t *testing.T) {
	tc := NewHeuristicCounter()
	cfg := LeidenChunkerConfig{
		Graph:      DefaultGraphParams(),
		Resolution: DefaultResolution,
		Similarity: SemanticSimilarity{Embedder: NewFakeEmbedder()},
	}
	chunks, err := ChunkLeiden(context.Background(), ParseBlocks(multiTopicDoc), tc, cfg)
	if err != nil {
		t.Fatalf("semantic arm: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("semantic arm produced no chunks")
	}
	for _, c := range chunks {
		if c.TokenCount > MaxChunkTokens {
			t.Errorf("chunk over budget: %d", c.TokenCount)
		}
	}
}

func TestSetChunkerValidation(t *testing.T) {
	s := openTestStore(t)
	ing := NewIngester(s, NewHeuristicCounter())

	if err := ing.SetChunker("nonsense", nil, LeidenChunkerConfig{}); err == nil {
		t.Error("expected an error for an unknown chunker")
	}
	// Semantic without an embedder must fail loudly rather than silently falling
	// back to baseline chunks that look like Leiden's.
	if err := ing.SetChunker(ChunkerLeidenSemantic, nil, LeidenChunkerConfig{}); err == nil {
		t.Error("expected an error for the semantic chunker with no embedder")
	}
	if err := ing.SetChunker(ChunkerLeidenLexical, nil, LeidenChunkerConfig{}); err != nil {
		t.Errorf("lexical chunker should not need an embedder: %v", err)
	}
	if ing.ChunkerName() != string(ChunkerLeidenLexical) {
		t.Errorf("ChunkerName = %q", ing.ChunkerName())
	}
	if err := ing.SetChunker(ChunkerLeidenSemantic, NewFakeEmbedder(), LeidenChunkerConfig{}); err != nil {
		t.Errorf("semantic chunker with an embedder: %v", err)
	}
}

// TestIngestWithLeidenChunker runs the whole pipeline with Leiden selected, since
// chunker changes have to survive dedup, entity extraction and storage.
func TestIngestWithLeidenChunker(t *testing.T) {
	s := openTestStore(t)
	ing := NewIngester(s, NewHeuristicCounter())
	if err := ing.SetChunker(ChunkerLeidenLexical, nil, LeidenChunkerConfig{}); err != nil {
		t.Fatal(err)
	}

	root := writeVault(t)
	rep, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("IngestDirectory with Leiden: %v", err)
	}
	t.Logf("leiden-lexical: %s", rep.String())
	if rep.ChunksWritten == 0 {
		t.Fatal("no chunks written")
	}

	// Re-ingest must be a no-op, which requires the chunker to be deterministic
	// all the way through the hash check.
	again, err := ing.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesIngested != 0 {
		t.Errorf("re-ingest with Leiden ingested %d files, want 0 — chunking is not stable",
			again.FilesIngested)
	}
}
