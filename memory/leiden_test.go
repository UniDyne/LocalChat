package memory

import (
	"fmt"
	"math/rand"
	"testing"
)

// plantedGraph builds k cliques of the given size, connected by a few weak bridges.
// Recovering these communities is the minimum bar for a community-detection
// implementation.
func plantedGraph(k, size int, bridgeWeight float64) (*Graph, []int) {
	n := k * size
	g := NewGraph(n)
	truth := make([]int, n)
	for c := 0; c < k; c++ {
		base := c * size
		for i := 0; i < size; i++ {
			truth[base+i] = c
			for j := i + 1; j < size; j++ {
				g.AddEdge(base+i, base+j, 1.0)
			}
		}
	}
	// One weak bridge between consecutive cliques.
	for c := 0; c+1 < k; c++ {
		g.AddEdge(c*size, (c+1)*size, bridgeWeight)
	}
	return g, truth
}

// agreement is the fraction of node pairs the partition classifies the same way as
// the ground truth (the Rand index), which handles arbitrary community labels.
func agreement(a, b []int) float64 {
	n := len(a)
	if n < 2 {
		return 1
	}
	same, total := 0, 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			total++
			if (a[i] == a[j]) == (b[i] == b[j]) {
				same++
			}
		}
	}
	return float64(same) / float64(total)
}

func TestLeidenRecoversPlantedCommunities(t *testing.T) {
	for _, tc := range []struct{ k, size int }{{3, 6}, {4, 5}, {5, 8}} {
		t.Run(fmt.Sprintf("k%d_size%d", tc.k, tc.size), func(t *testing.T) {
			g, truth := plantedGraph(tc.k, tc.size, 0.05)
			part := Leiden(g, LeidenOptions{Resolution: 0.3, Seed: 1})

			if got := agreement(part, truth); got < 0.95 {
				t.Errorf("pair agreement with ground truth = %.3f, want >= 0.95\ngot  %v\nwant %v",
					got, part, truth)
			}
			if n := countCommunities(part); n != tc.k {
				t.Logf("found %d communities, planted %d (acceptable if agreement is high)", n, tc.k)
			}
		})
	}
}

// TestLeidenRefinementGuaranteesConnectivity is the property that justifies
// implementing Leiden rather than Louvain: no community may be internally
// disconnected. For chunking that would mean a "topic" whose sentences have no
// relationship to one another.
func TestLeidenRefinementGuaranteesConnectivity(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 25; trial++ {
		n := 20 + rng.Intn(40)
		g := NewGraph(n)
		// A sparse random graph — the shape most likely to produce disconnected
		// communities under a naive optimizer.
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Float64() < 0.08 {
					g.AddEdge(i, j, 0.5+rng.Float64())
				}
			}
		}
		part := Leiden(g, LeidenOptions{Resolution: 0.2, Seed: int64(trial)})
		if !CommunitiesConnected(g, part) {
			t.Fatalf("trial %d produced an internally disconnected community: %v", trial, part)
		}
	}
}

// TestLeidenDeterministic is a hard requirement, not a nicety: re-ingesting an
// unchanged document must produce identical chunks, or incremental re-ingest churns
// the whole index and no test can assert anything.
func TestLeidenDeterministic(t *testing.T) {
	g, _ := plantedGraph(4, 6, 0.1)
	first := Leiden(g, LeidenOptions{Resolution: 0.25, Seed: 42})
	for i := 0; i < 5; i++ {
		again := Leiden(g, LeidenOptions{Resolution: 0.25, Seed: 42})
		if len(again) != len(first) {
			t.Fatalf("run %d returned a different length", i)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("run %d differs at node %d: %v vs %v", i, j, first, again)
			}
		}
	}

	// A different seed may legitimately differ; only same-seed stability is
	// promised.
	other := Leiden(g, LeidenOptions{Resolution: 0.25, Seed: 7})
	if len(other) != len(first) {
		t.Error("partition length should not depend on the seed")
	}
}

func TestLeidenResolutionControlsGranularity(t *testing.T) {
	g, _ := plantedGraph(6, 5, 0.05)

	low := countCommunities(Leiden(g, LeidenOptions{Resolution: 0.05, Seed: 3}))
	high := countCommunities(Leiden(g, LeidenOptions{Resolution: 2.0, Seed: 3}))

	if high < low {
		t.Errorf("higher resolution produced fewer communities (%d) than lower (%d) — "+
			"gamma should increase granularity", high, low)
	}
	t.Logf("gamma=0.05 -> %d communities, gamma=2.0 -> %d", low, high)
}

func TestLeidenEdgeCases(t *testing.T) {
	if got := Leiden(NewGraph(0), LeidenOptions{}); len(got) != 0 {
		t.Errorf("empty graph returned %v", got)
	}
	if got := Leiden(NewGraph(1), LeidenOptions{}); len(got) != 1 || got[0] != 0 {
		t.Errorf("single node returned %v", got)
	}
	// No edges: every node is its own community, and nothing crashes.
	iso := Leiden(NewGraph(5), LeidenOptions{Resolution: 0.25, Seed: 1})
	if countCommunities(iso) != 5 {
		t.Errorf("isolated nodes merged: %v", iso)
	}
	// One fully connected clique should stay together at low resolution.
	g := NewGraph(6)
	for i := 0; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			g.AddEdge(i, j, 1)
		}
	}
	if n := countCommunities(Leiden(g, LeidenOptions{Resolution: 0.1, Seed: 1})); n != 1 {
		t.Errorf("a clique split into %d communities at low resolution", n)
	}
}

func TestCPMQualityImprovesOverSingletons(t *testing.T) {
	g, _ := plantedGraph(4, 6, 0.05)
	singleton := singletons(g.N())
	found := Leiden(g, LeidenOptions{Resolution: 0.25, Seed: 5})

	qs := CPMQuality(g, singleton, 0.25)
	qf := CPMQuality(g, found, 0.25)
	if qf <= qs {
		t.Errorf("Leiden quality %.4f did not improve on the singleton partition %.4f", qf, qs)
	}
	t.Logf("CPM: singletons=%.4f leiden=%.4f", qs, qf)
}

func TestGraphAddEdgeAccumulates(t *testing.T) {
	g := NewGraph(3)
	g.AddEdge(0, 1, 0.5)
	g.AddEdge(0, 1, 0.25) // same pair again — e.g. semantic plus positional
	if len(g.adj[0]) != 1 {
		t.Fatalf("duplicate edge created a second entry: %+v", g.adj[0])
	}
	if w := g.adj[0][0].Weight; w != 0.75 {
		t.Errorf("weight = %v, want 0.75", w)
	}
	// Symmetry.
	if g.adj[1][0].Weight != 0.75 {
		t.Errorf("reverse direction not updated: %+v", g.adj[1])
	}
	// Self-loops go to the self-loop accumulator, not the adjacency list.
	g.AddEdge(2, 2, 1.5)
	if len(g.adj[2]) != 0 || g.selfLoop[2] != 1.5 {
		t.Errorf("self-loop mishandled: adj=%v selfLoop=%v", g.adj[2], g.selfLoop[2])
	}
}

func TestContiguousRuns(t *testing.T) {
	cases := []struct {
		name string
		part []int
		want [][2]int
	}{
		{"all_one", []int{0, 0, 0}, [][2]int{{0, 3}}},
		{"two_blocks", []int{0, 0, 1, 1}, [][2]int{{0, 2}, {2, 4}}},
		// The case that matters: a community whose members are scattered becomes
		// several runs rather than one incoherent chunk.
		{"scattered", []int{0, 0, 1, 1, 0, 0}, [][2]int{{0, 2}, {2, 4}, {4, 6}}},
		{"alternating", []int{0, 1, 0, 1}, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contiguousRuns(c.part)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("run %d = %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestDedupeOverlappingRuns(t *testing.T) {
	// Overlapping windows must yield a partition: every index covered exactly once,
	// or chunk texts would duplicate content.
	runs := [][2]int{{0, 5}, {3, 8}, {6, 10}}
	out := dedupeOverlappingRuns(runs, 10)

	covered := make([]int, 10)
	for _, r := range out {
		for i := r[0]; i < r[1]; i++ {
			covered[i]++
		}
	}
	for i, c := range covered {
		if c != 1 {
			t.Errorf("index %d covered %d times, want exactly 1 (runs=%v)", i, c, out)
		}
	}
}

func TestSentenceWindows(t *testing.T) {
	p := GraphParams{WindowSize: 100, WindowOverlap: 10}

	if w := SentenceWindows(50, p); len(w) != 1 || w[0] != [2]int{0, 50} {
		t.Errorf("short input windowed as %v", w)
	}

	w := SentenceWindows(250, p)
	if len(w) < 2 {
		t.Fatalf("long input produced %d windows", len(w))
	}
	// Windows must cover everything and overlap as configured.
	if w[0][0] != 0 {
		t.Error("first window does not start at 0")
	}
	if w[len(w)-1][1] != 250 {
		t.Errorf("last window ends at %d, want 250", w[len(w)-1][1])
	}
	for i := 1; i < len(w); i++ {
		if w[i][0] >= w[i-1][1] {
			t.Errorf("windows %d and %d do not overlap: %v %v", i-1, i, w[i-1], w[i])
		}
	}
}

func TestBuildSentenceGraphMutualTopK(t *testing.T) {
	// A hub similar to everything, plus two mutually-similar pairs. Without mutual
	// top-k the hub attaches to all and the partition collapses to one community.
	n := 6
	sim := newMatrix(n)
	set := func(i, j int, v float64) { sim[i][j] = v; sim[j][i] = v }
	for j := 1; j < n; j++ {
		set(0, j, 0.6) // hub
	}
	set(1, 2, 0.95)
	set(3, 4, 0.95)

	p := GraphParams{TopK: 1, SimFloor: 0.3, PositionAlpha: 0, PositionSpan: 0}
	g := BuildSentenceGraph(sim, p)

	// With k=1, the hub's single best peer is one node; the mutual requirement
	// keeps the rest from attaching to it.
	if deg := g.Degree(0); deg > 0.7 {
		t.Errorf("hub degree %.2f suggests mutual top-k was not applied", deg)
	}
	// The strong pairs survive.
	if g.Degree(1) == 0 || g.Degree(3) == 0 {
		t.Error("strongly similar pairs lost their edges")
	}
}

func TestBuildSentenceGraphPositionalPrior(t *testing.T) {
	n := 5
	sim := newMatrix(n) // no similarity at all
	p := GraphParams{TopK: 5, SimFloor: 0.9, PositionAlpha: 0.6, PositionSpan: 2}
	g := BuildSentenceGraph(sim, p)

	// With no similarity edges, the positional prior alone must still connect
	// consecutive sentences — otherwise Leiden would see an edgeless graph and
	// every sentence would become its own chunk.
	if g.Degree(0) == 0 {
		t.Fatal("positional prior produced no edges")
	}
	if !CommunitiesConnected(g, make([]int, n)) {
		t.Error("positional prior left the chain disconnected")
	}
	// Weight decays with distance.
	var w1, w2 float64
	for _, nb := range g.adj[0] {
		if nb.To == 1 {
			w1 = nb.Weight
		}
		if nb.To == 2 {
			w2 = nb.Weight
		}
	}
	if !(w1 > w2 && w2 > 0) {
		t.Errorf("positional weights should decay: d1=%v d2=%v", w1, w2)
	}
}
