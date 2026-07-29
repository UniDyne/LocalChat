package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The retrieval eval harness.
//
// Sized and shaped in response to what Phase 5 discovered: its 12-query set was
// *saturated* — every chunker arm scored recall@5 = 1.000 — so it could not rank the
// arms at all, and the decision rested on an MRR gap of one rank position on one
// query. This set is larger (50) and deliberately harder in three ways:
//
//   - It targets a specific chunk, not just the right file. Getting the document
//     right is easy when the corpus has three documents; getting the right section is
//     the job.
//   - It includes queries phrased in the user's words rather than the document's, so
//     lexical overlap alone does not carry them.
//   - It includes adversarial pairs: near-miss queries whose obvious keyword match is
//     in the *wrong* section, which is where fusion should beat any single signal.
//
// recall@1 is reported alongside recall@5 because a saturated recall@5 hides
// everything; if recall@5 pins at 1.000 again, recall@1 and MRR are what discriminate.

// evalCase is one query with the phrase that must appear in the retrieved chunk.
type evalCase struct {
	query string
	// wantFile is the source_ref the answer lives in.
	wantFile string
	// wantPhrase must appear in the returned chunk's text. Targeting a phrase rather
	// than a file is what makes this test section-level.
	wantPhrase string
	// hard marks queries phrased away from the document's own wording.
	hard bool
}

// evalCases draws on the repository's own documentation, which is real prose written
// without any eye to being retrievable.
var evalCases = []evalCase{
	// --- storage and persistence ---
	{"what database stores sessions and messages", "ARCHITECTURE.md", "DuckDB", false},
	{"which tables exist in the database", "ARCHITECTURE.md", "sessions", false},
	{"how is the seq number assigned to a message", "ARCHITECTURE.md", "seq", false},
	{"where do I find the schema definition", "ARCHITECTURE.md", "store.Open", false},
	{"what happens to messages when a session is deleted", "ARCHITECTURE.md", "session", true},
	{"how are artifacts persisted separately from chat", "ARCHITECTURE.md", "artifacts", false},

	// --- chain of thought ---
	{"how does the hidden reasoning pass work", "ARCHITECTURE.md", "cot", true},
	{"why is reasoning folded into the user turn rather than the system prompt", "ARCHITECTURE.md", "cotAnswerWrapper", true},
	{"what limits the length of the evaluation pass", "ARCHITECTURE.md", "max_tokens", true},
	{"are the model's own notes replayed back to it later", "ARCHITECTURE.md", "persisted", true},

	// --- tool calling ---
	{"how are tool calls executed during a turn", "ARCHITECTURE.md", "chatWithTools", false},
	{"what stops the tool loop running forever", "ARCHITECTURE.md", "maxToolIterations", false},
	{"how does the plan advance from one step to the next", "ARCHITECTURE.md", "manage_plan", false},
	{"who decides what happens after the plan is written", "ARCHITECTURE.md", "frontend", true},
	{"why does calling the plan tool end the turn", "ARCHITECTURE.md", "manage_plan", true},

	// --- frontend structure ---
	{"which file owns the chat send and receive loop", "ARCHITECTURE.md", "app.js", false},
	{"how does the frontend talk to the Go backend", "ARCHITECTURE.md", "api.js", false},
	{"where is markdown rendered", "ARCHITECTURE.md", "content.js", false},
	{"how does the session list update its titles live", "ARCHITECTURE.md", "session:renamed", false},
	{"what keeps generated bindings in sync with Go methods", "ARCHITECTURE.md", "wailsjs", true},
	{"is message timing stored in the database", "ARCHITECTURE.md", "timing", true},

	// --- design decisions ---
	{"why is there no HTTP layer between frontend and backend", "ARCHITECTURE.md", "bindings", true},
	{"how do I add a new skill without changing code", "ARCHITECTURE.md", "conf", true},
	{"what happens to a turn that errors halfway through", "ARCHITECTURE.md", "persist", true},
	{"why are messages written as they are produced", "ARCHITECTURE.md", "Persist", true},

	// --- UI ---
	{"how are artifacts shown in the sidebar", "UI.md", "artifact", false},
	{"what controls the colours and theming", "UI.md", "custom propert", false},
	{"how does the message input grow as I type", "UI.md", "textarea", true},
	{"what visually distinguishes a plan-driven message", "UI.md", "msg-task", true},
	{"how is code highlighted in replies", "UI.md", "highlight", false},
	{"what shows while the model is still thinking", "UI.md", "status", true},
	{"how is the layout organised at the top level", "UI.md", "layout", false},
	{"can I collapse or hide the sidebar", "UI.md", "sidebar", true},

	// --- README / setup ---
	{"how do I point the app at a different ollama server", "README.md", "ollama_endpoint", true},
	{"what do I need installed to build this", "README.md", "wails", false},
	{"how do skills get discovered", "README.md", "skills", false},
	{"where does configuration live", "README.md", "config.json", false},
	{"how do I run it in development", "README.md", "dev", false},
	{"what is the two pass reasoning approach for", "README.md", "cot", true},

	// --- adversarial: the obvious keyword match is in the wrong place ---
	// "artifact" appears in ARCHITECTURE.md too, but the sidebar behaviour is UI.md.
	{"what does the artifact panel look like", "UI.md", "artifact", true},
	// "duckdb" appears in README.md, but the schema detail is ARCHITECTURE.md.
	{"which columns does the messages table have", "ARCHITECTURE.md", "tool_args", true},
	// "plan" appears in several places; the styling question is UI.md.
	{"how does a plan step look in the transcript", "UI.md", "task", true},
	// "model" is everywhere; this is about switching it.
	{"how do I change which model answers", "README.md", "model", true},
	// "skill" is in both; the file format is README.md.
	{"what frontmatter does a skill file need", "README.md", "description", true},

	// --- terse and referential, the shapes real users type ---
	{"cot modes", "ARCHITECTURE.md", "cot", false},
	{"tool registry", "ARCHITECTURE.md", "tools.go", false},
	{"pinned messages", "ARCHITECTURE.md", "pinned", false},
	{"artifact content types", "ARCHITECTURE.md", "content_type", false},
	{"where are icons stored", "README.md", "build", true},
	{"keyboard shortcuts", "UI.md", "key", true},
}

type evalScore struct {
	name      string
	recallAt1 float64
	recallAt5 float64
	fileAt5   float64
	mrr       float64
	hardR5    float64
	easyR5    float64
	queries   int
}

func (e evalScore) String() string {
	return fmt.Sprintf("%-22s R@1=%.3f R@5=%.3f MRR=%.3f | file@5=%.3f | easy=%.3f hard=%.3f",
		e.name, e.recallAt1, e.recallAt5, e.mrr, e.fileAt5, e.easyR5, e.hardR5)
}

// buildEvalCorpus copies the repository's documentation into a temp directory.
func buildEvalCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"ARCHITECTURE.md", "UI.md", "README.md"} {
		b, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Skipf("corpus file %s unavailable: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// evaluate runs the eval set against a system and scores it.
func evaluate(t *testing.T, name string, sys *System, opts SearchOptions) evalScore {
	t.Helper()
	sc := evalScore{name: name, queries: len(evalCases)}
	var rr float64
	var hit1, hit5, file5 int
	var hardTotal, hardHit, easyTotal, easyHit int

	for _, c := range evalCases {
		o := opts
		o.Limit = 5
		results, _, err := sys.Search(context.Background(), c.query, o)
		if err != nil {
			t.Fatalf("%s: search %q: %v", name, c.query, err)
		}
		rank, fileRank := 0, 0
		for i, r := range results {
			if fileRank == 0 && r.SourceRef == c.wantFile {
				fileRank = i + 1
			}
			if rank == 0 && r.SourceRef == c.wantFile && containsFold(r.Text, c.wantPhrase) {
				rank = i + 1
			}
		}
		if fileRank > 0 {
			file5++
		}
		if rank == 1 {
			hit1++
		}
		if rank > 0 {
			hit5++
			rr += 1 / float64(rank)
		}
		if c.hard {
			hardTotal++
			if rank > 0 {
				hardHit++
			}
		} else {
			easyTotal++
			if rank > 0 {
				easyHit++
			}
		}
	}
	n := float64(len(evalCases))
	sc.recallAt1 = float64(hit1) / n
	sc.recallAt5 = float64(hit5) / n
	sc.fileAt5 = float64(file5) / n
	sc.mrr = rr / n
	if hardTotal > 0 {
		sc.hardR5 = float64(hardHit) / float64(hardTotal)
	}
	if easyTotal > 0 {
		sc.easyR5 = float64(easyHit) / float64(easyTotal)
	}
	return sc
}

func containsFold(haystack, needle string) bool {
	return len(needle) == 0 || indexFold(haystack, needle) >= 0
}

func indexFold(s, sub string) int {
	ls, lsub := lower(s), lower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// newEvalSystem ingests the corpus and embeds it, ready for evaluation.
//
// A nil emb means "no embeddings at all": Config.ModelDir is pointed at a
// nonexistent path so resolution cannot quietly find a real model on the machine and
// turn a degraded-path test into a normal one.
func newEvalSystem(t *testing.T, emb Embedder) *System {
	t.Helper()
	s := openTestStore(t)
	cfg := Config{Embedder: emb}
	if emb == nil {
		cfg.ModelDir = filepath.Join(t.TempDir(), "absent")
	}
	sys := NewSystem(s, cfg, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	root := buildEvalCorpus(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if emb != nil {
		if _, err := sys.Backfill(context.Background(), 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	return sys
}

// TestEvalBaseline is the Phase 4 exit criterion: the recall figure every later phase
// is measured against.
func TestEvalBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("eval is slow; skipped with -short")
	}
	var emb Embedder
	if paths, err := FindModel(); err == nil {
		if e, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4}); err == nil {
			emb = e
			defer e.Close()
		}
	}
	if emb == nil {
		t.Skip("model not provisioned; the baseline must be measured with the real embedder")
	}

	sys := newEvalSystem(t, emb)
	st, _ := sys.Store.MemoryStats()
	t.Logf("corpus: %d sources, %d chunks, %d embedded", st.Sources, st.Chunks, st.EmbeddedChunks)

	base := evaluate(t, "fused (weighted)", sys, SearchOptions{Mode: FusionWeighted})
	fmt.Println("\n=== Phase 4 retrieval baseline (50 queries, chunk-level targets) ===")
	fmt.Println(base.String())

	// Single-signal ablations, which is what shows whether fusion is earning its
	// complexity or whether one signal is doing all the work.
	ablations := []struct {
		name string
		w    Weights
	}{
		{"bm25 only", Weights{BM25: 1}},
		{"vector only", Weights{Vector: 1}},
		{"entity only", Weights{Entity: 1}},
		{"ngram only", Weights{Ngram: 1}},
		{"bm25+vector", Weights{BM25: 0.5, Vector: 0.5}},
	}
	var all []evalScore
	all = append(all, base)
	for _, ab := range ablations {
		sc := evaluate(t, ab.name, sys, SearchOptions{Mode: FusionWeighted, Weights: ab.w})
		all = append(all, sc)
		fmt.Println(sc.String())
	}
	rrf := evaluate(t, "fused (RRF)", sys, SearchOptions{Mode: FusionRRF})
	all = append(all, rrf)
	fmt.Println(rrf.String())
	fmt.Println()

	// Report the ranking so a regression is obvious at a glance.
	sort.Slice(all, func(i, j int) bool { return all[i].mrr > all[j].mrr })
	fmt.Println("by MRR:")
	for i, sc := range all {
		fmt.Printf("  %d. %-22s MRR=%.3f R@1=%.3f R@5=%.3f\n", i+1, sc.name, sc.mrr, sc.recallAt1, sc.recallAt5)
	}
	fmt.Println()

	// Hard assertions: the eval set must not be saturated, and fusion must be at
	// least competitive with its best single signal.
	if base.recallAt5 >= 0.999 {
		t.Errorf("eval set is saturated (R@5 = %.3f) — it cannot discriminate between "+
			"configurations and needs harder queries", base.recallAt5)
	}
	if base.recallAt5 < 0.4 {
		t.Errorf("baseline recall@5 = %.3f is implausibly low; retrieval is likely broken",
			base.recallAt5)
	}
	bestSingle := 0.0
	for _, sc := range all[1:] {
		if sc.name != "fused (RRF)" && sc.name != "bm25+vector" && sc.mrr > bestSingle {
			bestSingle = sc.mrr
		}
	}
	if base.mrr < bestSingle*0.9 {
		t.Errorf("fusion MRR %.3f is worse than the best single signal %.3f — the weights "+
			"or normalization are wrong", base.mrr, bestSingle)
	}
}

// ---------- Phase 6: graph expansion ----------

// evalDetail records per-case outcomes, which is what makes "expansion rescued a
// query" and "expansion cost a query" distinguishable from a net MRR delta.
type evalDetail struct {
	score evalScore
	// rank per case, 0 for a miss.
	rank []int
	// expandedHit marks cases whose winning result came from the walk.
	expandedHit []bool
	// newCandidates counts chunks the walk admitted that candidate generation never
	// proposed — the population the exit criterion is actually about.
	newCandidates int
	expandedShown int
}

func evaluateDetailed(t *testing.T, name string, sys *System, opts SearchOptions) evalDetail {
	t.Helper()
	d := evalDetail{
		rank:        make([]int, len(evalCases)),
		expandedHit: make([]bool, len(evalCases)),
	}
	sc := evalScore{name: name, queries: len(evalCases)}
	var rr float64
	var hit1, hit5, file5 int
	var hardTotal, hardHit, easyTotal, easyHit int

	for ci, c := range evalCases {
		o := opts
		o.Limit = 5
		results, rep, err := sys.Search(context.Background(), c.query, o)
		if err != nil {
			t.Fatalf("%s: search %q: %v", name, c.query, err)
		}
		d.newCandidates += rep.ExpandedCandidates
		d.expandedShown += rep.ExpandedReturned

		rank, fileRank := 0, 0
		for i, r := range results {
			if fileRank == 0 && r.SourceRef == c.wantFile {
				fileRank = i + 1
			}
			if rank == 0 && r.SourceRef == c.wantFile && containsFold(r.Text, c.wantPhrase) {
				rank = i + 1
				d.expandedHit[ci] = r.Expanded
			}
		}
		d.rank[ci] = rank
		if fileRank > 0 {
			file5++
		}
		if rank == 1 {
			hit1++
		}
		if rank > 0 {
			hit5++
			rr += 1 / float64(rank)
		}
		if c.hard {
			hardTotal++
			if rank > 0 {
				hardHit++
			}
		} else {
			easyTotal++
			if rank > 0 {
				easyHit++
			}
		}
	}
	n := float64(len(evalCases))
	sc.recallAt1 = float64(hit1) / n
	sc.recallAt5 = float64(hit5) / n
	sc.fileAt5 = float64(file5) / n
	sc.mrr = rr / n
	if hardTotal > 0 {
		sc.hardR5 = float64(hardHit) / float64(hardTotal)
	}
	if easyTotal > 0 {
		sc.easyR5 = float64(easyHit) / float64(easyTotal)
	}
	d.score = sc
	return d
}

// TestEvalGraphExpansion is the Phase 6 exit measurement on the repo-docs corpus:
// does the walk add recall, and does it cost precision?
//
// Two limitations are structural and must not be glossed.
//
// **The candidate pool covers the whole corpus at default sizes.** The vector arm
// alone takes the 200 nearest chunks and this corpus has 45, so every chunk is
// already a candidate and the walk cannot admit anything new — measured, not
// assumed: at default pool sizes every arm below returns exactly 0 new candidates
// and dMRR +0.000. Expansion is a no-op by construction whenever the pool covers
// the corpus, which on a real vault (tens of thousands of chunks against a pool of
// ~400) is never the case. The sweep therefore scales the pool down to hold a
// vault-like pool-to-corpus ratio, and reports the default-size row as the control
// that shows the no-op.
//
// **This corpus has zero cross-document links**, so `link` expansion cannot be
// measured here at all. The plan calls for link-only expansion to be measured
// separately precisely because it may carry most of the benefit on a vault; that
// measurement lives in TestLinkExpansionSurfacesUnscoredChunks, on a corpus that
// has links.
func TestEvalGraphExpansion(t *testing.T) {
	if testing.Short() {
		t.Skip("eval is slow; skipped with -short")
	}
	var emb Embedder
	if paths, err := FindModel(); err == nil {
		if e, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4}); err == nil {
			emb = e
			defer e.Close()
		}
	}
	if emb == nil {
		t.Skip("model not provisioned; expansion must be measured with the real embedder")
	}

	sys := newEvalSystem(t, emb)
	er, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{})
	if err != nil {
		t.Fatalf("BuildEdges: %v", err)
	}
	t.Log(er.String())
	byKind, err := sys.Store.EdgeCountsByKind()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("\n=== Phase 6 graph expansion (50 queries, chunk-level targets) ===")
	fmt.Printf("edges by kind: %v\n", byKind)
	if byKind[EdgeLink] == 0 {
		fmt.Println("NOTE: this corpus has no cross-document links, so `link` expansion is " +
			"NOT measured here — see TestLinkExpansionSurfacesUnscoredChunks.")
	}

	st, _ := sys.Store.MemoryStats()
	arms := []struct {
		name  string
		kinds []string
	}{
		{"all kinds", nil},
		{"sequential", []string{EdgeNext, EdgePrev}},
		{"similar", []string{EdgeSimilar}},
		{"entity", []string{EdgeEntity}},
		{"link only", []string{EdgeLink}},
	}

	// Pool sizes as a fraction of the corpus. The default row is the control: it
	// covers everything, so expansion provably cannot contribute.
	pools := []struct {
		name  string
		limit CandidateLimits
	}{
		{"pool=default (covers corpus)", CandidateLimits{}},
		{"pool=1/2 corpus", CandidateLimits{BM25: st.Chunks / 2, Vector: st.Chunks / 2, Entity: st.Chunks / 4}},
		{"pool=1/4 corpus", CandidateLimits{BM25: st.Chunks / 4, Vector: st.Chunks / 4, Entity: st.Chunks / 8}},
		{"pool=1/8 corpus", CandidateLimits{BM25: st.Chunks / 8, Vector: st.Chunks / 8, Entity: st.Chunks / 16}},
	}

	anyContribution := false
	for _, pool := range pools {
		fmt.Printf("\n--- %s (corpus %d chunks) ---\n", pool.name, st.Chunks)
		base := evaluateDetailed(t, "no expansion", sys,
			SearchOptions{Mode: FusionWeighted, Candidates: pool.limit})
		fmt.Println(base.score.String())

		for _, arm := range arms {
			opts := SearchOptions{Mode: FusionWeighted, Expand: true, Candidates: pool.limit}
			opts.Walk.Kinds = arm.kinds
			d := evaluateDetailed(t, "expand: "+arm.name, sys, opts)

			var rescued, lost []string
			for i := range evalCases {
				switch {
				case base.rank[i] == 0 && d.rank[i] > 0:
					rescued = append(rescued, evalCases[i].query)
				case base.rank[i] > 0 && d.rank[i] == 0:
					lost = append(lost, evalCases[i].query)
				}
			}
			if d.newCandidates > 0 {
				anyContribution = true
			}
			fmt.Printf("%s  | new cands %d, shown %d, rescued %d, lost %d  (dMRR %+.3f)\n",
				d.score.String(), d.newCandidates, d.expandedShown, len(rescued), len(lost),
				d.score.mrr-base.score.mrr)
			for _, q := range rescued {
				fmt.Printf("    + rescued: %q\n", q)
			}
			for _, q := range lost {
				fmt.Printf("    - lost:    %q\n", q)
			}

			// The precision guard. Expansion only adds candidates, so a real drop
			// means the expansion scores are on the wrong scale and are displacing
			// better direct results. 0.02 MRR is roughly one rank position on one
			// query out of fifty, which is the noise floor at this eval size.
			if d.score.mrr < base.score.mrr-0.02 {
				t.Errorf("%s / %s: MRR %.3f is below the unexpanded baseline %.3f — "+
					"expansion is displacing better direct results",
					pool.name, arm.name, d.score.mrr, base.score.mrr)
			}
			if d.score.recallAt5 < base.score.recallAt5-0.02 {
				t.Errorf("%s / %s: R@5 %.3f below baseline %.3f",
					pool.name, arm.name, d.score.recallAt5, base.score.recallAt5)
			}
		}
	}
	fmt.Println()

	// If no pool size lets the walk admit a single chunk, the measurement is vacuous
	// and the "no precision loss" assertions above proved nothing. Better to fail
	// loudly than to record a green test as evidence.
	if !anyContribution {
		t.Error("the walk admitted no new candidates at any pool size — this measurement " +
			"cannot say anything about expansion, and must not be read as validating it")
	}
}

// findText returns the first result containing needle, or nil.
func findText(results []Result, needle string) *Result {
	for i := range results {
		if containsFold(results[i].Text, needle) {
			return &results[i]
		}
	}
	return nil
}

// writeLinkedVault builds a corpus whose answers are one wikilink away from the
// note that matches the query, and share no vocabulary with it.
//
// This shape is the whole justification for the edge graph: no direct signal can
// find "Rollback Protocol" from a question about deployment, because the two notes
// have no words in common. Only the link does.
func writeLinkedVault(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		// The link sits in the same chunk as the query's vocabulary, which is the
		// depth-1 case: retrieving the linking chunk is what makes its link
		// traversable.
		"Deployment.md": `# Deployment

Our deployment process runs the release checklist before every production push.
The release manager signs off on the checklist, and if it goes wrong we follow
[[Rollback Protocol]].
`,
		// Deliberately shares no terms with any deployment query.
		"Rollback Protocol.md": `# Rollback Protocol

Restore the prior snapshot using zzyzx-restore, then flip the traffic weight
back to the blue fleet. Confirm the quorum before draining the green fleet.
`,
		// The two-hop case: the query's vocabulary is in the intro, the link is in a
		// later section, so reaching the target needs a sequential hop first.
		"Hiring.md": `# Hiring

We interview candidates in three rounds, with a written exercise in the middle.
Each round has two interviewers and a scorecard.

## Offers

Offer letters follow [[Compensation Bands]] without exception.
`,
		"Compensation Bands.md": `# Compensation Bands

Band four spans one hundred and ten to one hundred and forty thousand. Band five
adds an annual equity refresher of qqwerty units.
`,
		"Onboarding.md": `# Onboarding

New joiners get a laptop, an account, and a buddy for their first fortnight.
`,
		"Expenses.md": `# Expenses

Submit receipts within thirty days. Anything over five hundred needs approval.
`,
		"Kitchen.md": `# Kitchen

The coffee machine needs descaling monthly. Milk lives in the door of the fridge.
`,
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestLinkExpansionSurfacesUnscoredChunks is the Phase 6 exit criterion, stated
// exactly as the plan states it: the walk must surface a relevant chunk that scored
// zero on all four direct signals.
//
// "Scored zero on all four" is checked the strict way — the chunk must never have
// entered the candidate pool, so BM25, vector and entity generation all failed to
// propose it and the n-gram signal never got a chance to score it. A test that only
// checked "the chunk came back" could pass on a corpus where a direct signal was
// quietly doing the work.
//
// That strictness forces one deliberate choice: **no embedder**. The vector arm
// takes the 200 nearest chunks, and a fixture corpus is far smaller than 200, so
// with vectors available *every* chunk is a candidate and no expansion can ever be
// the exclusive route to anything. Measured, not assumed: the first version of this
// test used the fake embedder and the target chunk arrived as a direct candidate.
// Proving the graph's exclusive contribution therefore needs either a >200-chunk
// corpus or a search where the vector arm is off; the second is hermetic and fast.
// TestEvalGraphExpansion covers the with-vectors case on the real corpus.
func TestLinkExpansionSurfacesUnscoredChunks(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })
	if sys.EmbeddingsAvailable() {
		t.Skip("embedder resolved despite an absent model dir; cannot isolate the graph")
	}

	root := writeLinkedVault(t)
	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LinksResolved == 0 {
		t.Fatal("no links resolved; the fixture is wrong")
	}
	if _, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{}); err != nil {
		t.Fatal(err)
	}

	const query = "deployment release checklist sign off"
	const target = "zzyzx-restore"

	// Without expansion the rollback note is unreachable: it shares no terms with
	// the query.
	plain, plainRep, err := sys.Search(context.Background(), query,
		SearchOptions{Limit: 5, Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plain {
		if containsFold(r.Text, target) {
			t.Fatalf("the target chunk was found without expansion; the fixture no longer "+
				"isolates the graph's contribution (%q)", truncate(r.Text, 80))
		}
	}
	t.Logf("unexpanded: %d results from %d candidates", len(plain), plainRep.Candidates)

	// With link-only expansion it must appear, marked as expanded and attributed to
	// the link edge.
	opts := SearchOptions{Limit: 5, Explain: true, Expand: true}
	opts.Walk.Kinds = []string{EdgeLink}
	expanded, expRep, err := sys.Search(context.Background(), query, opts)
	if err != nil {
		t.Fatal(err)
	}
	if expRep.Walk == nil {
		t.Fatal("no walk report on an expanded search")
	}
	t.Logf("expanded: %d results, %s, %d new candidates",
		len(expanded), expRep.Walk.String(), expRep.ExpandedCandidates)

	found := findText(expanded, target)
	if found == nil {
		t.Fatal("link expansion did not surface the linked chunk — this is the Phase 6 " +
			"exit criterion and it is not met")
	}
	if !found.Expanded {
		t.Error("the linked chunk is not marked as expanded")
	}
	if found.Via != EdgeLink {
		t.Errorf("Via = %q, want %q", found.Via, EdgeLink)
	}
	if found.Depth < 1 {
		t.Errorf("Depth = %d, want at least 1", found.Depth)
	}
	if found.Raw != nil && (found.Raw.BM25 != 0 || found.Raw.Vector != 0 || found.Raw.Entity != 0) {
		t.Errorf("the expanded chunk had direct signals after all: %+v — it was in the "+
			"candidate pool, so the walk is not what found it", *found.Raw)
	}
	if expRep.ExpandedCandidates == 0 {
		t.Error("no chunks were admitted by the walk, yet an expanded result was returned")
	}

	// Expansion must extend the ranking, not reorder it: the directly-scored results
	// keep their order and stay ahead of the expansion.
	var directOrder, expandedOrder []string
	for _, r := range plain {
		directOrder = append(directOrder, r.ChunkID)
	}
	for _, r := range expanded {
		if !r.Expanded {
			expandedOrder = append(expandedOrder, r.ChunkID)
		}
	}
	for i := range expandedOrder {
		if i < len(directOrder) && expandedOrder[i] != directOrder[i] {
			t.Errorf("expansion reordered the direct results at position %d", i)
			break
		}
	}
	if expanded[0].Expanded {
		t.Error("an expanded result outranked every direct one; expansion scores are " +
			"parent-relative and must sit below their parent")
	}

	// The two-hop case, and the reason `next`/`prev` edges are not merely a
	// convenience: when the link lives in a later section than the text the query
	// matched, link-only expansion cannot reach the target at all. A sequential hop
	// is what bridges the distance inside the source note.
	const hiringQuery = "hiring interview rounds scorecard"
	const hiringTarget = "qqwerty"

	linkOnly := SearchOptions{Limit: 5, Expand: true}
	linkOnly.Walk.Kinds = []string{EdgeLink}
	got, _, err := sys.Search(context.Background(), hiringQuery, linkOnly)
	if err != nil {
		t.Fatal(err)
	}
	if findText(got, hiringTarget) != nil {
		t.Log("link-only expansion reached the two-hop target; the chunker put the link " +
			"in the matched chunk, so this case is not exercising the sequential bridge")
	}

	bridged := SearchOptions{Limit: 5, Expand: true}
	bridged.Walk.Kinds = []string{EdgeLink, EdgeNext, EdgePrev}
	got, bridgedRep, err := sys.Search(context.Background(), hiringQuery, bridged)
	if err != nil {
		t.Fatal(err)
	}
	hit := findText(got, hiringTarget)
	if hit == nil {
		t.Errorf("sequential+link expansion did not reach the two-hop target; %s",
			bridgedRep.Walk.String())
	} else {
		if !hit.Expanded {
			t.Error("the two-hop target is not marked as expanded")
		}
		t.Logf("two-hop target reached at depth %d via %s", hit.Depth, hit.Via)
		if hit.Depth < 2 && hit.Via == EdgeLink {
			t.Log("reached at depth 1; the link was in the matched chunk after all")
		}
	}

	// Backlinks: a query that lands on the rollback note must reach the deployment
	// note, whose link points the other way.
	back, _, err := sys.Search(context.Background(), "zzyzx-restore snapshot blue fleet quorum", opts)
	if err != nil {
		t.Fatal(err)
	}
	var sawBacklink bool
	for _, r := range back {
		if r.Expanded && r.SourceRef == "Deployment.md" {
			sawBacklink = true
		}
	}
	if !sawBacklink {
		t.Error("expansion did not traverse the link backwards; backlinks are as " +
			"meaningful as forward links (§3.5)")
	}
}

// TestExpansionRescuesWithManySources tests the diagnosis TestEvalGraphExpansion
// points at rather than leaving it as a hypothesis.
//
// That measurement found expansion admitting up to 1822 candidates across 50 queries
// while only 28 ever reached the results and 0 queries were rescued. The reason is
// stage 4: per-source argmax keeps one chunk per source, and the walk overwhelmingly
// reaches *other chunks of sources that already have a direct candidate*, where an
// expanded chunk always loses to a direct one. On a three-document corpus there is
// almost nowhere else for it to land.
//
// If that diagnosis is right, expansion should rescue queries as soon as the corpus
// has many sources and the answer lives in a source with no direct candidate at all.
// This builds exactly that corpus: 24 topic/detail note pairs with disjoint
// vocabulary, linked one way, and 24 queries that can only reach the detail note
// through its link.
func TestExpansionRescuesWithManySources(t *testing.T) {
	root := t.TempDir()
	// Disjoint vocabularies by construction: topics use real words, details use
	// nonsense tokens that appear nowhere else in the corpus.
	topics := []struct{ topic, detail string }{
		{"quarterly budget forecast", "vixtrol quorum ledger"},
		{"customer churn analysis", "brimfast cohort delta"},
		{"warehouse inventory audit", "zelphon pallet tally"},
		{"payroll tax filing", "quoltis bracket schedule"},
		{"server capacity planning", "narvex throughput ceiling"},
		{"vendor contract renewal", "plimbeth clause rider"},
		{"employee training programme", "krendal module tier"},
		{"marketing campaign results", "objuvar impression yield"},
		{"product roadmap priorities", "shanvel milestone lane"},
		{"security incident response", "tarquim containment ring"},
		{"office lease negotiation", "brovald square footage"},
		{"supplier quality review", "fenwick defect ratio"},
		{"shipping logistics costs", "gralmoth freight band"},
		{"software licence audit", "wexlint seat count"},
		{"database migration plan", "yorbith cutover window"},
		{"recruitment pipeline health", "zandrel offer velocity"},
		{"customer support backlog", "murvane queue depth"},
		{"annual insurance renewal", "clefnor premium tier"},
		{"data retention policy", "vashlim purge horizon"},
		{"partner integration status", "hexpole handshake state"},
		{"travel expense limits", "brindaq per diem cap"},
		{"factory maintenance schedule", "olvrath downtime slot"},
		{"regulatory compliance gaps", "wembrix attestation set"},
		{"board meeting agenda", "znaphil resolution docket"},
	}
	for i, tc := range topics {
		topicFile := fmt.Sprintf("topic-%02d.md", i)
		detailFile := fmt.Sprintf("detail-%02d.md", i)
		topic := fmt.Sprintf("# Topic %d\n\nNotes on the %s for this period. The numbers "+
			"live in [[detail-%02d]].\n", i, tc.topic, i)
		detail := fmt.Sprintf("# Detail %d\n\nThe %s was recorded at forty-two units "+
			"against a baseline of thirty.\n", i, tc.detail)
		if err := os.WriteFile(filepath.Join(root, topicFile), []byte(topic), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, detailFile), []byte(detail), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := openTestStore(t)
	// No embedder, for the reason given in TestLinkExpansionSurfacesUnscoredChunks:
	// the vector arm's cap exceeds any fixture corpus, so with vectors on, every
	// chunk is a candidate and no expansion can be exclusive.
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })
	if sys.EmbeddingsAvailable() {
		t.Skip("embedder resolved despite an absent model dir")
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LinksResolved != len(topics) {
		t.Fatalf("resolved %d of %d links", rep.LinksResolved, len(topics))
	}
	er, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(er.String())

	var plainHits, expandedHits int
	for i, tc := range topics {
		want := strings.Fields(tc.detail)[0] // the nonsense token
		query := tc.topic

		plain, _, err := sys.Search(context.Background(), query, SearchOptions{Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		if findText(plain, want) != nil {
			plainHits++
		}

		exp, _, err := sys.Search(context.Background(), query,
			SearchOptions{Limit: 5, Expand: true})
		if err != nil {
			t.Fatal(err)
		}
		if r := findText(exp, want); r != nil {
			expandedHits++
			if !r.Expanded {
				t.Errorf("case %d: the detail note was not marked as expanded", i)
			}
		}
	}

	n := len(topics)
	t.Logf("detail note reached: %d/%d without expansion, %d/%d with expansion",
		plainHits, n, expandedHits, n)
	fmt.Printf("\n=== Phase 6: expansion on a %d-source corpus ===\n", n*2)
	fmt.Printf("linked detail note reached in %d/%d queries without expansion, %d/%d with\n\n",
		plainHits, n, expandedHits, n)

	if plainHits > 0 {
		t.Errorf("%d detail notes were reachable without expansion; the fixture's "+
			"vocabularies are not disjoint and the result proves nothing", plainHits)
	}
	// The diagnosis stands or falls here. Expansion is what makes these reachable at
	// all, and with many sources stage 4 no longer suppresses it.
	if expandedHits < n*3/4 {
		t.Errorf("expansion reached only %d/%d linked detail notes; the walk is not "+
			"delivering the recall the edge graph exists for", expandedHits, n)
	}
}

// TestSearchDegradesWithoutVectors is the property that makes an unprovisioned model
// survivable: three of the four signals still work.
func TestSearchDegradesWithoutVectors(t *testing.T) {
	sys := newEvalSystem(t, nil)
	if sys.EmbeddingsAvailable() {
		t.Skip("embedder present; cannot exercise the degraded path")
	}

	results, rep, err := sys.Search(context.Background(), "what database stores sessions",
		SearchOptions{Limit: 5, Explain: true})
	if err != nil {
		t.Fatalf("search without vectors: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results without vectors — the non-vector signals should still work")
	}
	if rep.VectorSkipped == "" {
		t.Error("report should say why the vector arm was skipped")
	}
	if rep.FromVector != 0 {
		t.Errorf("FromVector = %d with no embedder", rep.FromVector)
	}
	if rep.FromBM25 == 0 {
		t.Error("BM25 produced no candidates")
	}
	for _, r := range results {
		if r.Raw != nil && r.Raw.Vector != 0 {
			t.Errorf("vector signal nonzero with no embedder: %v", r.Raw.Vector)
		}
	}
	t.Logf("degraded search returned %d results; reason: %s", len(results), rep.VectorSkipped)
}

// ---------- Phase 8: does better entity extraction improve retrieval? ----------

// oracleExtractor is a generous, query-blind stand-in for a good LLM.
//
// It exists because the Phase 8 exit criterion — "the eval set shows recall
// improvement from heuristic-only → heuristic+LLM" — has two independent halves, and
// only one of them needs a model:
//
//  1. *Would* better entities improve retrieval on this corpus? That is a property of
//     the fusion layer and the corpus, and an oracle answers it exactly.
//  2. Does a particular model produce better entities? That needs the model.
//
// This measures (1), which is the half worth knowing first: if a generous oracle
// cannot move recall, no extraction model will either, and that is worth discovering
// before spending hours of model time on a vault.
//
// Deliberately query-blind — it never sees evalCases. An "oracle" that extracted the
// eval set's own target phrases would leak the labels into the index and measure
// nothing but its own cheating.
type oracleExtractor struct {
	calls int
}

func (o *oracleExtractor) ModelName() string { return "oracle (query-blind, generous)" }

var (
	oracleCapPhrase = regexp.MustCompile(`\b([A-Z][A-Za-z0-9]*(?:[ ][A-Z][A-Za-z0-9]*)*)\b`)
	oracleIdent     = regexp.MustCompile("`([^`\n]{2,60})`")
	oracleDotted    = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+)\b`)
)

func (o *oracleExtractor) Extract(_ context.Context, req ExtractRequest) (ExtractResult, error) {
	o.calls++
	var res ExtractResult
	seen := map[string]bool{}
	add := func(kind, v string) {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > MaxEntityValueChars || seen[strings.ToLower(v)] {
			return
		}
		seen[strings.ToLower(v)] = true
		res.Entities = append(res.Entities, ExtractedEntity{Kind: kind, Value: v})
	}
	// Backticked identifiers and dotted names first: these are what the heuristic
	// proper-noun regex misses most on technical prose, and what the eval's queries
	// are full of.
	for _, m := range oracleIdent.FindAllStringSubmatch(req.Text, -1) {
		add("code", m[1])
	}
	for _, m := range oracleDotted.FindAllStringSubmatch(req.Text, -1) {
		add("code", m[1])
	}
	for _, m := range oracleCapPhrase.FindAllStringSubmatch(req.Text, -1) {
		add("proper", m[1])
	}
	return res, nil
}

// TestEvalEntityEnrichmentHeadroom is the Phase 8 exit measurement, minus the half
// that needs a model.
//
// Reports heuristic-only against heuristic+oracle, both fused and entity-only. The
// entity-only arm is the one that answers the mechanism question: if better entities
// improve the entity signal but not the fused score, the finding is about the signal's
// *weight*, not about extraction — and those are very different conclusions.
func TestEvalEntityEnrichmentHeadroom(t *testing.T) {
	if testing.Short() {
		t.Skip("eval is slow; skipped with -short")
	}
	var emb Embedder
	if paths, err := FindModel(); err == nil {
		if e, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4}); err == nil {
			emb = e
			defer e.Close()
		}
	}
	if emb == nil {
		t.Skip("model not provisioned; the comparison must use the real embedder")
	}

	sys := newEvalSystem(t, emb)
	entityCount := func() int {
		st, err := sys.Store.MemoryStats()
		if err != nil {
			t.Fatal(err)
		}
		return st.Entities
	}

	fmt.Println("\n=== Phase 8 entity enrichment headroom (50 queries) ===")
	fmt.Printf("heuristic tier: %d distinct entities\n", entityCount())

	beforeFused := evaluate(t, "heuristic: fused", sys, SearchOptions{Mode: FusionWeighted})
	beforeEnt := evaluate(t, "heuristic: entity-only", sys,
		SearchOptions{Mode: FusionWeighted, Weights: Weights{Entity: 1}})
	fmt.Println(beforeFused.String())
	fmt.Println(beforeEnt.String())

	oracle := &oracleExtractor{}
	sys.SetExtractor(oracle)
	rep, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())
	fmt.Printf("\nafter enrichment: %d distinct entities (%d accepted from %d proposed, "+
		"%d rejected as not verbatim), %d extraction calls\n",
		entityCount(), rep.Guards.EntitiesAccepted, rep.Guards.EntitiesProposed,
		rep.Guards.RejectedNotVerbatim, oracle.calls)

	afterFused := evaluate(t, "heuristic+oracle: fused", sys, SearchOptions{Mode: FusionWeighted})
	afterEnt := evaluate(t, "heuristic+oracle: entity-only", sys,
		SearchOptions{Mode: FusionWeighted, Weights: Weights{Entity: 1}})
	fmt.Println(afterFused.String())
	fmt.Println(afterEnt.String())

	fmt.Printf("\ndelta fused:       MRR %+.3f  R@1 %+.3f  R@5 %+.3f\n",
		afterFused.mrr-beforeFused.mrr, afterFused.recallAt1-beforeFused.recallAt1,
		afterFused.recallAt5-beforeFused.recallAt5)
	fmt.Printf("delta entity-only: MRR %+.3f  R@1 %+.3f  R@5 %+.3f\n\n",
		afterEnt.mrr-beforeEnt.mrr, afterEnt.recallAt1-beforeEnt.recallAt1,
		afterEnt.recallAt5-beforeEnt.recallAt5)

	// The measurement must actually have happened. A pass that accepted nothing would
	// make every delta trivially zero and the test meaningless — the Phase 6 lesson.
	if rep.Guards.EntitiesAccepted == 0 {
		t.Fatal("the oracle produced no accepted entities; this comparison proves nothing")
	}
	if oracle.calls == 0 {
		t.Fatal("no extraction calls were made")
	}

	// Fusion must not get *worse*. Entities are additive, so a drop means the entity
	// signal is being diluted by volume — precisely the risk of an over-eager extractor,
	// and the thing the caps and guards exist to bound.
	if afterFused.mrr < beforeFused.mrr-0.02 {
		t.Errorf("enrichment cost fused MRR: %.3f -> %.3f. More entities should not make "+
			"retrieval worse; suspect the volume caps or the per-chunk attribution",
			beforeFused.mrr, afterFused.mrr)
	}
	if afterFused.recallAt5 < beforeFused.recallAt5-0.02 {
		t.Errorf("enrichment cost R@5: %.3f -> %.3f", beforeFused.recallAt5, afterFused.recallAt5)
	}
}
