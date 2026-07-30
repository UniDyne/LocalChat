package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Three-arm chunker comparison.
//
// IMPORTANT CAVEAT ON WHAT THIS MEASURES. The plan's Phase 5 exit criterion is a
// recall comparison against the *fused* scorer (BM25 + vector + entity + n-gram),
// which Phase 4 builds. That does not exist yet, so this holds retrieval constant
// at vector-only search and varies only the chunker. That is a fair comparison
// between arms — the same retrieval method for each — but it is not the full
// system, and it favours whichever chunker produces the most embedding-friendly
// spans. Treat the numbers as directional.
//
// The eval set below is also far smaller than the 50+ pairs §6 says are needed to
// resolve small differences. A one-query swing here is ~8%. It is sized to catch a
// gross regression, not to settle a close call.

type evalQuery struct {
	query string
	// want is the source_ref that should be retrieved.
	want string
}

// evalSet asks about content that genuinely exists in the repository's own
// documentation, so the corpus is real prose rather than something written to be
// easy.
var evalSet = []evalQuery{
	{"what database stores sessions and messages", "ARCHITECTURE.md"},
	{"how does the chain of thought evaluation pass work", "ARCHITECTURE.md"},
	{"where are the wails javascript bindings maintained", "ARCHITECTURE.md"},
	{"what happens when a tool call is dispatched during a turn", "ARCHITECTURE.md"},
	{"how is the plan advanced between steps", "ARCHITECTURE.md"},
	{"which files hold the frontend chat controller", "ARCHITECTURE.md"},
	{"how are artifacts displayed in the sidebar", "UI.md"},
	{"what css custom properties control theming", "UI.md"},
	{"how does the message input area behave", "UI.md"},
	{"how do I configure the ollama endpoint", "README.md"},
	{"what is required to build the project", "README.md"},
	{"how do skills get loaded from markdown files", "README.md"},
}

type armResult struct {
	name      string
	chunks    int
	ingest    time.Duration
	embed     time.Duration
	recallAt3 float64
	recallAt5 float64
	mrr       float64
	avgTokens float64
	medTokens int
}

// TestCompareChunkers runs the arms head to head. Skipped without the real model:
// the fake embedder cannot distinguish the arms meaningfully, since its whole
// purpose is determinism rather than semantic fidelity.
func TestCompareChunkers(t *testing.T) {
	if testing.Short() {
		t.Skip("comparison is slow; skipped with -short")
	}
	paths, err := FindModel()
	if err != nil {
		t.Skipf("model not provisioned: %v", err)
	}
	emb, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4})
	if err != nil {
		t.Skipf("onnx embedder unavailable: %v", err)
	}
	defer emb.Close()

	corpus := buildCompareCorpus(t)

	arms := []struct {
		name ChunkerKind
		cfg  LeidenChunkerConfig
	}{
		{ChunkerHeadings, LeidenChunkerConfig{}},
		{ChunkerLeidenLexical, LeidenChunkerConfig{Similarity: LexicalSimilarity{}}},
		{ChunkerLeidenSemantic, LeidenChunkerConfig{Similarity: SemanticSimilarity{Embedder: emb}}},
	}

	var results []armResult
	for _, arm := range arms {
		res, err := runArm(t, arm.name, arm.cfg, emb, corpus)
		if err != nil {
			t.Fatalf("arm %s: %v", arm.name, err)
		}
		results = append(results, res)
	}

	fmt.Println("\n=== chunker comparison (vector-only retrieval, 12 queries) ===")
	fmt.Printf("%-18s %7s %9s %9s %8s %8s %7s %9s\n",
		"arm", "chunks", "ingest", "embed", "R@3", "R@5", "MRR", "med tok")
	for _, r := range results {
		fmt.Printf("%-18s %7d %9s %9s %8.3f %8.3f %7.3f %9d\n",
			r.name, r.chunks, r.ingest.Round(time.Millisecond), r.embed.Round(time.Millisecond),
			r.recallAt3, r.recallAt5, r.mrr, r.medTokens)
	}

	// Report the cost multiple, which is the other half of the decision.
	base := results[0]
	for _, r := range results[1:] {
		totalBase := base.ingest + base.embed
		total := r.ingest + r.embed
		if totalBase > 0 {
			fmt.Printf("  %s total cost = %.1fx the headings baseline\n",
				r.name, float64(total)/float64(totalBase))
		}
	}
	fmt.Println()

	// The only hard assertion: no arm may collapse. A real regression shows up as
	// near-zero recall, and that should fail rather than be reported quietly.
	for _, r := range results {
		if r.chunks == 0 {
			t.Errorf("arm %s produced no chunks", r.name)
		}
		if r.recallAt5 < 0.25 {
			t.Errorf("arm %s recall@5 = %.3f, implausibly low — likely broken rather than merely worse",
				r.name, r.recallAt5)
		}
	}
}

// buildCompareCorpus copies a curated subset of the repository's documentation into
// a temp directory. A subset rather than the whole repo because the semantic arm
// embeds every sentence, and Memory Plan.md alone would dominate the runtime.
func buildCompareCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"ARCHITECTURE.md", "UI.md", "README.md"} {
		src := filepath.Join("..", name)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("corpus file %s unavailable: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runArm(t *testing.T, kind ChunkerKind, cfg LeidenChunkerConfig, emb Embedder, corpus string) (armResult, error) {
	t.Helper()
	res := armResult{name: string(kind)}

	s := openTestStore(t)
	sys := NewSystem(s, Config{Embedder: emb}, nil)
	defer func() {
		// Do not Close the system: it would close the shared embedder that later
		// arms still need.
		sys.Queue.Stop()
	}()

	if err := sys.Ingester.SetChunker(kind, emb, cfg); err != nil {
		return res, err
	}

	start := time.Now()
	rep, err := sys.Ingester.IngestDirectory(context.Background(), corpus, nil)
	if err != nil {
		return res, err
	}
	res.ingest = time.Since(start)
	res.chunks = rep.ChunksWritten

	start = time.Now()
	if _, err := sys.Backfill(context.Background(), 0, nil); err != nil {
		return res, err
	}
	res.embed = time.Since(start)

	// Chunk size distribution, which explains a lot of any recall difference.
	var sizes []int
	rows, err := s.DB().Query(`SELECT token_count FROM memory_chunks ORDER BY token_count`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return res, err
		}
		sizes = append(sizes, n)
	}
	rows.Close()
	if len(sizes) > 0 {
		total := 0
		for _, n := range sizes {
			total += n
		}
		res.avgTokens = float64(total) / float64(len(sizes))
		res.medTokens = sizes[len(sizes)/2]
	}

	// Evaluate.
	var rrSum float64
	hits3, hits5 := 0, 0
	for _, q := range evalSet {
		hits, err := sys.SearchVector(context.Background(), q.query, 5)
		if err != nil {
			return res, err
		}
		rank := 0
		for i, h := range hits {
			if h.SourceRef == q.want {
				rank = i + 1
				break
			}
		}
		if rank > 0 {
			rrSum += 1 / float64(rank)
			if rank <= 3 {
				hits3++
			}
			hits5++
		}
	}
	n := float64(len(evalSet))
	res.recallAt3 = float64(hits3) / n
	res.recallAt5 = float64(hits5) / n
	res.mrr = rrSum / n
	return res, nil
}

// TestChunkSizeDistribution reports how the arms differ structurally, which is
// often more informative than a small-sample recall number.
func TestChunkSizeDistribution(t *testing.T) {
	tc := NewHeuristicCounter()
	body, err := os.ReadFile(filepath.Join("..", "ARCHITECTURE.md"))
	if err != nil {
		t.Skip("ARCHITECTURE.md unavailable")
	}
	_, md := SplitFrontmatter(string(body))
	blocks := ParseBlocks(md)

	report := func(name string, chunks []Chunk) {
		if len(chunks) == 0 {
			t.Errorf("%s produced no chunks", name)
			return
		}
		sizes := make([]int, len(chunks))
		for i, c := range chunks {
			sizes[i] = c.TokenCount
		}
		sort.Ints(sizes)
		sum := 0
		over := 0
		for _, n := range sizes {
			sum += n
			if n > MaxChunkTokens {
				over++
			}
		}
		t.Logf("%-16s n=%-4d min=%-4d p50=%-4d p90=%-4d max=%-4d mean=%.0f over-budget=%d",
			name, len(sizes), sizes[0], sizes[len(sizes)/2], sizes[int(float64(len(sizes))*0.9)],
			sizes[len(sizes)-1], float64(sum)/float64(len(sizes)), over)
	}

	report("headings", ChunkHeadings(blocks, tc))

	lex, err := ChunkLeiden(context.Background(), blocks, tc, lexicalCfg())
	if err != nil {
		t.Fatal(err)
	}
	report("leiden-lexical", lex)

	if paths, err := FindModel(); err == nil {
		if emb, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4}); err == nil {
			defer emb.Close()
			sem, err := ChunkLeiden(context.Background(), blocks, tc, LeidenChunkerConfig{
				Graph:      DefaultGraphParams(),
				Resolution: DefaultResolution,
				Similarity: SemanticSimilarity{Embedder: emb},
			})
			if err != nil {
				t.Fatal(err)
			}
			report("leiden-semantic", sem)
		}
	}
}
