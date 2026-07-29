package memory

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"simple-cot-chat/store"
)

// stubEdges is a hand-built graph, so the walk's arithmetic and caps can be
// asserted without a database or an embedder.
type stubEdges struct {
	out map[string][]store.MemoryEdge
	// queries counts calls, which is how the "one query per hop" property is checked
	// — the walk batching its frontier is the difference between one query and
	// thirty.
	queries int
	err     error
}

func newStubEdges() *stubEdges {
	return &stubEdges{out: map[string][]store.MemoryEdge{}}
}

func (s *stubEdges) add(src, dst, kind string, w float64) *stubEdges {
	s.out[src] = append(s.out[src], store.MemoryEdge{
		SrcChunkID: src, DstChunkID: dst, Kind: kind, Weight: w,
	})
	return s
}

func (s *stubEdges) GetEdgesFromMany(ids []string, bidirectional ...string) (map[string][]store.MemoryEdge, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.queries++
	kinds := map[string]bool{}
	for _, k := range bidirectional {
		kinds[k] = true
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	res := map[string][]store.MemoryEdge{}
	for _, id := range ids {
		res[id] = append(res[id], s.out[id]...)
	}
	// Inbound traversal for the bidirectional kinds, presented from the walker's
	// point of view exactly as the store does.
	for src, edges := range s.out {
		for _, e := range edges {
			if !kinds[e.Kind] || !want[e.DstChunkID] {
				continue
			}
			res[e.DstChunkID] = append(res[e.DstChunkID], store.MemoryEdge{
				SrcChunkID: e.DstChunkID, DstChunkID: src, Kind: e.Kind, Weight: e.Weight,
			})
		}
	}
	return res, nil
}

func expByID(exps []Expansion) map[string]Expansion {
	out := make(map[string]Expansion, len(exps))
	for _, e := range exps {
		out[e.ChunkID] = e
	}
	return out
}

func approx(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// TestWalkScoreDecay checks the propagation rule the whole design rests on:
// parent x edge_weight x decay(kind), compounding per hop.
func TestWalkScoreDecay(t *testing.T) {
	g := newStubEdges().
		add("seed", "a", EdgeLink, 1.0).
		add("a", "b", EdgeSimilar, 0.8).
		add("seed", "c", EdgeEntity, 0.5)

	exps, rep, err := Walk(g, map[string]float64{"seed": 1.0}, nil, WalkParams{})
	if err != nil {
		t.Fatal(err)
	}
	byID := expByID(exps)
	t.Log(rep.String())

	// link at depth 1: 1.0 * 1.0 * 0.6
	if e, ok := byID["a"]; !ok {
		t.Fatal("depth-1 link expansion missing")
	} else if !approx(e.Score, 0.6) {
		t.Errorf("a score = %v, want 0.6", e.Score)
	} else if e.Depth != 1 || e.Via != EdgeLink || e.ParentID != "seed" {
		t.Errorf("a provenance = depth %d via %s from %s", e.Depth, e.Via, e.ParentID)
	}

	// similar at depth 2 compounds: 0.6 * 0.8 * 0.4
	if e, ok := byID["b"]; !ok {
		t.Fatal("depth-2 expansion missing")
	} else if !approx(e.Score, 0.6*0.8*0.4) {
		t.Errorf("b score = %v, want %v", e.Score, 0.6*0.8*0.4)
	} else if e.Depth != 2 {
		t.Errorf("b depth = %d, want 2", e.Depth)
	}

	// entity at depth 1: 1.0 * 0.5 * 0.3
	if e := byID["c"]; !approx(e.Score, 0.15) {
		t.Errorf("c score = %v, want 0.15", e.Score)
	}

	// Every expansion must score below its seed. If expansion could outrank the
	// chunk that led to it, the walk would be reordering results rather than
	// extending them.
	for _, e := range exps {
		if e.Score >= 1.0 {
			t.Errorf("expansion %s scores %v, at or above its seed", e.ChunkID, e.Score)
		}
	}
	if rep.Queries != 2 {
		t.Errorf("Queries = %d, want 2 (one per hop, frontier batched)", rep.Queries)
	}
}

// TestWalkRespectsDepth is the bound that keeps expansion from wandering the whole
// corpus.
func TestWalkRespectsDepth(t *testing.T) {
	g := newStubEdges().
		add("seed", "d1", EdgeLink, 1.0).
		add("d1", "d2", EdgeLink, 1.0).
		add("d2", "d3", EdgeLink, 1.0).
		add("d3", "d4", EdgeLink, 1.0)

	for depth := 1; depth <= 3; depth++ {
		exps, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil,
			WalkParams{MaxDepth: depth})
		if err != nil {
			t.Fatal(err)
		}
		if len(exps) != depth {
			t.Errorf("MaxDepth %d returned %d expansions, want %d", depth, len(exps), depth)
		}
		for _, e := range exps {
			if e.Depth > depth {
				t.Errorf("expansion at depth %d exceeds MaxDepth %d", e.Depth, depth)
			}
		}
	}
}

// TestWalkCapsFrontierAndVisited covers the hub case §3.5 warns about: a chunk
// with hundreds of entity edges must not be allowed to dominate.
func TestWalkCapsFrontierAndVisited(t *testing.T) {
	g := newStubEdges()
	// A hub with 200 outbound entity edges of descending weight.
	for i := 0; i < 200; i++ {
		g.add("seed", fmt.Sprintf("hub-%03d", i), EdgeEntity, 1.0-float64(i)/1000)
	}

	exps, rep, err := Walk(g, map[string]float64{"seed": 1.0}, nil,
		WalkParams{MaxDepth: 1, FrontierPerHop: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 5 {
		t.Fatalf("FrontierPerHop 5 returned %d expansions", len(exps))
	}
	if rep.TruncatedFrontier != 195 {
		t.Errorf("TruncatedFrontier = %d, want 195 — a bounded walk must say it was "+
			"bounded rather than look exhausted", rep.TruncatedFrontier)
	}
	// The cap must keep the strongest, not an arbitrary five.
	for _, e := range exps {
		if e.ChunkID > "hub-004" {
			t.Errorf("frontier cap kept %s; it should keep the highest-scoring edges", e.ChunkID)
		}
	}

	exps, rep, err = Walk(g, map[string]float64{"seed": 1.0}, nil,
		WalkParams{MaxDepth: 2, FrontierPerHop: 100, MaxVisited: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) > 20 {
		t.Errorf("MaxVisited 20 returned %d expansions", len(exps))
	}
	if !rep.TruncatedVisited {
		t.Error("TruncatedVisited not reported despite hitting the cap")
	}
}

// TestWalkTraversesLinksBothWays is the backlink property: "notes that link here"
// is as meaningful as forward links, exactly as Obsidian treats them.
func TestWalkTraversesLinksBothWays(t *testing.T) {
	// Only inbound edges exist for the seed: someone links *to* it.
	g := newStubEdges().
		add("backlinker", "seed", EdgeLink, 1.0).
		add("similar-to", "seed", EdgeSimilar, 0.9)

	exps, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil, WalkParams{MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	byID := expByID(exps)
	if _, ok := byID["backlinker"]; !ok {
		t.Error("a note linking to the seed was not reached; backlinks must be traversed")
	}
	// `similar` is symmetric and stored both ways at build time, so it must NOT be
	// traversed backwards here — doing so would double-count the one direction the
	// builder chose to omit under its cap.
	if _, ok := byID["similar-to"]; ok {
		t.Error("similar edge traversed against its direction; only genuinely directed " +
			"kinds are bidirectional")
	}
}

// TestWalkKindFilter is what makes `link`-only expansion measurable separately,
// which the plan calls for because on a vault corpus links may carry most of the
// benefit on their own.
func TestWalkKindFilter(t *testing.T) {
	g := newStubEdges().
		add("seed", "via-link", EdgeLink, 1.0).
		add("seed", "via-entity", EdgeEntity, 1.0).
		add("inbound", "seed", EdgeLink, 1.0)

	exps, rep, err := Walk(g, map[string]float64{"seed": 1.0}, nil,
		WalkParams{MaxDepth: 1, Kinds: []string{EdgeLink}})
	if err != nil {
		t.Fatal(err)
	}
	byID := expByID(exps)
	if _, ok := byID["via-entity"]; ok {
		t.Error("entity edge traversed despite Kinds restricting to link")
	}
	if _, ok := byID["via-link"]; !ok {
		t.Error("link edge not traversed")
	}
	if _, ok := byID["inbound"]; !ok {
		t.Error("inbound link not traversed with Kinds=[link]")
	}
	if rep.ByKind[EdgeLink] != 2 {
		t.Errorf("ByKind[link] = %d, want 2", rep.ByKind[EdgeLink])
	}

	// An entity-only walk must not silently pick up the link edges either.
	exps, _, err = Walk(g, map[string]float64{"seed": 1.0}, nil,
		WalkParams{MaxDepth: 1, Kinds: []string{EdgeEntity}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 1 || exps[0].ChunkID != "via-entity" {
		t.Errorf("entity-only walk returned %+v", exps)
	}
}

// TestWalkUnknownKindIgnored guards against a future edge kind getting full credit
// by accident: an unknown kind has no decay, and defaulting it to 1.0 would make
// it the strongest edge in the graph.
func TestWalkUnknownKindIgnored(t *testing.T) {
	g := newStubEdges().add("seed", "mystery", "some_future_kind", 1.0)
	exps, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil, WalkParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 0 {
		t.Errorf("unknown edge kind was traversed: %+v", exps)
	}
}

// TestWalkPrunesAndDeduplicates covers MinScore and the best-score-wins rule when
// two routes reach the same chunk.
func TestWalkPrunesAndDeduplicates(t *testing.T) {
	g := newStubEdges().
		add("seed", "weak", EdgeEntity, 0.001).
		add("seed", "both", EdgeEntity, 0.5).
		add("seed", "strong", EdgeLink, 1.0).
		add("strong", "both", EdgeLink, 1.0)

	exps, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil,
		WalkParams{MinScore: 0.01})
	if err != nil {
		t.Fatal(err)
	}
	byID := expByID(exps)
	if _, ok := byID["weak"]; ok {
		t.Error("expansion below MinScore was kept")
	}
	// "both" is reachable at depth 1 via entity (0.3 * 0.5 = 0.15). The depth-2
	// link route (0.6 * 1.0 * 0.6 = 0.36) is stronger but arrives later, and a
	// breadth-first walk claims the node at the first depth that reaches it. The
	// point of the assertion is that it is claimed exactly once.
	if e, ok := byID["both"]; !ok {
		t.Fatal("both not reached")
	} else if e.Depth != 1 {
		t.Errorf("both claimed at depth %d; breadth-first should claim it at 1", e.Depth)
	}
	count := 0
	for _, e := range exps {
		if e.ChunkID == "both" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("both appears %d times; each chunk must be expanded once", count)
	}
}

// TestWalkSeedsAreNotExpanded keeps expansion additive: a seed must not be handed
// back as its own discovery.
func TestWalkSeedsAreNotExpanded(t *testing.T) {
	g := newStubEdges().
		add("s1", "s2", EdgeLink, 1.0).
		add("s2", "s1", EdgeLink, 1.0).
		add("s1", "new", EdgeLink, 1.0)

	seeds := map[string]float64{"s1": 1.0, "s2": 0.9}
	exps, rep, err := Walk(g, seeds, map[string]bool{"s1": true, "s2": true}, WalkParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range exps {
		if _, isSeed := seeds[e.ChunkID]; isSeed {
			t.Errorf("seed %s returned as an expansion", e.ChunkID)
		}
	}
	if rep.Discovered != 1 {
		t.Errorf("Discovered = %d, want 1", rep.Discovered)
	}
}

// TestWalkSeedCap checks that only the top-K candidates seed the walk, and that it
// is the top-K by score rather than by iteration order.
func TestWalkSeedCap(t *testing.T) {
	g := newStubEdges()
	seeds := map[string]float64{}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("s%d", i)
		seeds[id] = float64(i) / 10
		g.add(id, fmt.Sprintf("out%d", i), EdgeLink, 1.0)
	}

	exps, rep, err := Walk(g, seeds, nil, WalkParams{MaxDepth: 1, Seeds: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Seeds != 3 {
		t.Errorf("Seeds = %d, want 3", rep.Seeds)
	}
	got := map[string]bool{}
	for _, e := range exps {
		got[e.ChunkID] = true
	}
	// s9, s8, s7 are the highest scoring.
	for _, want := range []string{"out9", "out8", "out7"} {
		if !got[want] {
			t.Errorf("missing %s; the seed cap must keep the highest-scoring candidates", want)
		}
	}
	if got["out0"] {
		t.Error("lowest-scoring candidate seeded the walk")
	}
}

// TestWalkPropagatesStoreError makes sure a failed edge query surfaces rather than
// silently degrading to an unexpanded search.
func TestWalkPropagatesStoreError(t *testing.T) {
	g := newStubEdges()
	g.err = errors.New("boom")
	if _, _, err := Walk(g, map[string]float64{"seed": 1}, nil, WalkParams{}); err == nil {
		t.Fatal("expected the store error to propagate")
	}
}

// TestWalkEmptySeeds is the no-candidates case: no query, no error.
func TestWalkEmptySeeds(t *testing.T) {
	g := newStubEdges()
	exps, rep, err := Walk(g, nil, nil, WalkParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) != 0 || rep.Queries != 0 {
		t.Errorf("empty seeds produced %d expansions in %d queries", len(exps), rep.Queries)
	}
}

// TestWalkDeterministic is required for the eval harness to mean anything: the same
// graph and seeds must expand identically every time, including where a cap cuts.
func TestWalkDeterministic(t *testing.T) {
	g := newStubEdges()
	// Deliberate ties, so the tie-break rule is what decides.
	for i := 0; i < 40; i++ {
		g.add("seed", fmt.Sprintf("t%02d", i), EdgeEntity, 0.5)
	}
	p := WalkParams{MaxDepth: 1, FrontierPerHop: 7}

	first, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _, err := Walk(g, map[string]float64{"seed": 1.0}, nil, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d expansions, first returned %d", i, len(again), len(first))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("run %d differs at %d: %+v vs %+v", i, j, first[j], again[j])
			}
		}
	}
}
