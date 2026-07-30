package memory

import (
	"math/rand"
	"sort"
)

// Leiden community detection (Traag, Waltman & van Eck, 2019).
//
// No Leiden implementation exists for Go — gonum offers only Louvain — so this is
// in-house. It is Louvain plus a refinement phase, and that phase is the whole
// point: Louvain can produce communities that are internally *disconnected*, which
// for chunking would mean a "topic" whose sentences have no relationship to each
// other. Leiden's refinement guarantees every community is internally connected.
//
// Quality function is CPM (Constant Potts Model) rather than modularity:
//
//	Q = Σ_c [ e_c − γ · w_c(w_c−1)/2 ]
//
// where e_c is the total internal edge weight of community c and w_c its total
// node weight. CPM is preferred because γ maps directly onto "how fine do I want
// the communities", and because modularity suffers a resolution limit that makes
// small communities impossible to recover at any setting.

// Graph is a weighted undirected graph with node weights, which aggregation needs
// (a coarse node's weight is the total weight of the nodes it represents).
type Graph struct {
	// adj[i] holds i's neighbours. Undirected edges appear in both lists.
	adj [][]Neighbor
	// nodeWeight[i] is i's weight; 1 for an original node.
	nodeWeight []float64
	// selfLoop[i] is internal weight collapsed into node i by aggregation.
	selfLoop []float64
}

// Neighbor is one weighted edge endpoint.
type Neighbor struct {
	To     int
	Weight float64
}

// NewGraph builds an empty graph with n nodes, each of weight 1.
func NewGraph(n int) *Graph {
	g := &Graph{
		adj:        make([][]Neighbor, n),
		nodeWeight: make([]float64, n),
		selfLoop:   make([]float64, n),
	}
	for i := range g.nodeWeight {
		g.nodeWeight[i] = 1
	}
	return g
}

// N returns the node count.
func (g *Graph) N() int { return len(g.adj) }

// AddEdge adds an undirected edge. Repeated pairs accumulate weight, so callers
// may add the same pair from two sources (semantic plus positional, say) without
// having to combine them first.
func (g *Graph) AddEdge(a, b int, w float64) {
	if a == b {
		g.selfLoop[a] += w
		return
	}
	for i := range g.adj[a] {
		if g.adj[a][i].To == b {
			g.adj[a][i].Weight += w
			for j := range g.adj[b] {
				if g.adj[b][j].To == a {
					g.adj[b][j].Weight += w
					return
				}
			}
			return
		}
	}
	g.adj[a] = append(g.adj[a], Neighbor{To: b, Weight: w})
	g.adj[b] = append(g.adj[b], Neighbor{To: a, Weight: w})
}

// Degree returns the total incident edge weight of a node, excluding self-loops.
func (g *Graph) Degree(i int) float64 {
	var s float64
	for _, n := range g.adj[i] {
		s += n.Weight
	}
	return s
}

// LeidenOptions controls the algorithm.
type LeidenOptions struct {
	// Resolution is CPM's γ. Higher means more, smaller communities. This is the
	// single most useful tuning knob for chunk granularity.
	Resolution float64
	// Seed makes the result reproducible.
	//
	// Determinism is a requirement, not a nicety: the reference implementation
	// randomizes node visit order, but re-ingesting an unchanged file must yield
	// byte-identical chunks or incremental re-ingest churns the whole index and no
	// test can assert anything.
	Seed int64
	// MaxIterations bounds the outer loop; convergence is normally much sooner.
	MaxIterations int
}

func (o LeidenOptions) withDefaults() LeidenOptions {
	if o.Resolution <= 0 {
		o.Resolution = 0.25
	}
	if o.MaxIterations <= 0 {
		o.MaxIterations = 20
	}
	return o
}

// Leiden partitions the graph, returning a community index per node. Indices are
// renumbered so that they are contiguous from 0 and ordered by each community's
// lowest member — which keeps output stable regardless of internal bookkeeping.
func Leiden(g *Graph, opts LeidenOptions) []int {
	opts = opts.withDefaults()
	if g.N() == 0 {
		return nil
	}
	if g.N() == 1 {
		return []int{0}
	}

	rng := rand.New(rand.NewSource(opts.Seed))

	// membership maps original nodes to communities in the current coarse graph.
	membership := make([]int, g.N())
	for i := range membership {
		membership[i] = i
	}

	cur := g
	// nodeToCoarse maps an original node to its node index in `cur`.
	nodeToCoarse := make([]int, g.N())
	for i := range nodeToCoarse {
		nodeToCoarse[i] = i
	}

	for iter := 0; iter < opts.MaxIterations; iter++ {
		part := singletons(cur.N())
		moved := moveNodes(cur, part, opts.Resolution, rng)

		// Every node in its own community means nothing can merge further.
		if countCommunities(part) == cur.N() && !moved {
			break
		}

		// Refinement: split each community into well-connected subsets. This is
		// what rules out internally-disconnected communities.
		refined := refine(cur, part, opts.Resolution, rng)

		// Project the refined partition back onto original nodes.
		for i := range membership {
			membership[i] = refined[nodeToCoarse[i]]
		}

		next, coarseOf := aggregate(cur, refined)
		if next.N() == cur.N() {
			break // no coarsening happened; converged
		}
		for i := range nodeToCoarse {
			nodeToCoarse[i] = coarseOf[refined[nodeToCoarse[i]]]
		}
		cur = next
	}

	return renumber(membership)
}

func singletons(n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return p
}

func countCommunities(part []int) int {
	seen := map[int]bool{}
	for _, c := range part {
		seen[c] = true
	}
	return len(seen)
}

// communityState tracks the aggregates CPM's gain formula needs.
type communityState struct {
	weight   []float64 // total node weight per community
	internal []float64 // total internal edge weight per community
}

func newCommunityState(g *Graph, part []int) *communityState {
	n := g.N()
	cs := &communityState{
		weight:   make([]float64, n),
		internal: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		cs.weight[part[i]] += g.nodeWeight[i]
		cs.internal[part[i]] += g.selfLoop[i]
	}
	for i := 0; i < n; i++ {
		for _, nb := range g.adj[i] {
			if nb.To > i && part[nb.To] == part[i] {
				cs.internal[part[i]] += nb.Weight
			}
		}
	}
	return cs
}

// moveNodes is Leiden's fast local-moving phase: a queue of nodes to revisit,
// rather than Louvain's repeated full sweeps. Returns whether anything moved.
func moveNodes(g *Graph, part []int, gamma float64, rng *rand.Rand) bool {
	n := g.N()
	cs := newCommunityState(g, part)

	order := rng.Perm(n)
	queue := make([]int, 0, n)
	inQueue := make([]bool, n)
	for _, v := range order {
		queue = append(queue, v)
		inQueue[v] = true
	}

	anyMoved := false
	// Bound the work: each node may be revisited, but not unboundedly.
	for steps := 0; len(queue) > 0 && steps < 100*n+1000; steps++ {
		v := queue[0]
		queue = queue[1:]
		inQueue[v] = false

		from := part[v]
		// Edge weight from v into each neighbouring community.
		toComm := map[int]float64{from: 0}
		for _, nb := range g.adj[v] {
			toComm[part[nb.To]] += nb.Weight
		}

		wv := g.nodeWeight[v]
		// Removing v from `from` leaves that community's weight lower.
		bestComm, bestGain := from, 0.0
		removeGain := toComm[from] - gamma*wv*(cs.weight[from]-wv)

		// Deterministic iteration over candidate communities.
		cands := make([]int, 0, len(toComm))
		for c := range toComm {
			cands = append(cands, c)
		}
		sort.Ints(cands)

		for _, c := range cands {
			if c == from {
				continue
			}
			addGain := toComm[c] - gamma*wv*cs.weight[c]
			gain := addGain - removeGain
			if gain > bestGain+1e-12 {
				bestGain, bestComm = gain, c
			}
		}

		if bestComm == from {
			continue
		}

		// Apply the move.
		cs.weight[from] -= wv
		cs.internal[from] -= toComm[from]
		cs.weight[bestComm] += wv
		cs.internal[bestComm] += toComm[bestComm]
		part[v] = bestComm
		anyMoved = true

		// Neighbours outside the new community may now want to move.
		for _, nb := range g.adj[v] {
			if part[nb.To] != bestComm && !inQueue[nb.To] {
				queue = append(queue, nb.To)
				inQueue[nb.To] = true
			}
		}
	}
	return anyMoved
}

// refine splits each community into well-connected subsets.
//
// Starting from singletons within a community, a node may merge only into a subset
// that is itself sufficiently connected to the rest of the community. That
// constraint is what guarantees the result contains no internally-disconnected
// community — the property Louvain lacks and the reason Leiden is worth
// implementing at all.
//
// The reference algorithm picks among candidate merges randomly (proportional to
// exp(ΔQ/θ)); this picks the best deterministically. That trades a little
// exploration for reproducibility, which incremental re-ingest needs.
func refine(g *Graph, part []int, gamma float64, rng *rand.Rand) []int {
	n := g.N()
	refined := singletons(n)

	byComm := map[int][]int{}
	for v := 0; v < n; v++ {
		byComm[part[v]] = append(byComm[part[v]], v)
	}

	comms := make([]int, 0, len(byComm))
	for c := range byComm {
		comms = append(comms, c)
	}
	sort.Ints(comms)

	// Per-subset aggregates, indexed by subset id (initially the node id).
	subsetWeight := make([]float64, n)
	for i := range subsetWeight {
		subsetWeight[i] = g.nodeWeight[i]
	}

	for _, c := range comms {
		members := byComm[c]
		if len(members) <= 1 {
			continue
		}
		inComm := make(map[int]bool, len(members))
		for _, v := range members {
			inComm[v] = true
		}

		// Visit in a seeded order for reproducibility.
		visit := append([]int{}, members...)
		rng.Shuffle(len(visit), func(i, j int) { visit[i], visit[j] = visit[j], visit[i] })

		for _, v := range visit {
			// Only nodes still alone may join a subset, which keeps subsets from
			// being rearranged after formation.
			if subsetWeight[refined[v]] != g.nodeWeight[v] {
				continue
			}

			toSubset := map[int]float64{}
			for _, nb := range g.adj[v] {
				if inComm[nb.To] {
					toSubset[refined[nb.To]] += nb.Weight
				}
			}
			delete(toSubset, refined[v])

			cands := make([]int, 0, len(toSubset))
			for s := range toSubset {
				cands = append(cands, s)
			}
			sort.Ints(cands)

			wv := g.nodeWeight[v]
			best, bestGain := -1, 0.0
			for _, s := range cands {
				gain := toSubset[s] - gamma*wv*subsetWeight[s]
				if gain > bestGain+1e-12 {
					best, bestGain = s, gain
				}
			}
			if best < 0 {
				continue
			}
			subsetWeight[refined[v]] -= wv
			subsetWeight[best] += wv
			refined[v] = best
		}
	}
	return renumber(refined)
}

// aggregate collapses each community into a single node, summing edge weights
// between communities and node weights within them. Returns the coarse graph and
// a mapping from community id to coarse node index.
func aggregate(g *Graph, part []int) (*Graph, []int) {
	maxComm := -1
	for _, c := range part {
		if c > maxComm {
			maxComm = c
		}
	}
	coarseOf := make([]int, maxComm+1)
	for i := range coarseOf {
		coarseOf[i] = -1
	}
	next := 0
	for _, c := range part {
		if coarseOf[c] == -1 {
			coarseOf[c] = next
			next++
		}
	}

	out := NewGraph(next)
	for i := range out.nodeWeight {
		out.nodeWeight[i] = 0
	}
	for v := 0; v < g.N(); v++ {
		cv := coarseOf[part[v]]
		out.nodeWeight[cv] += g.nodeWeight[v]
		out.selfLoop[cv] += g.selfLoop[v]
	}
	for v := 0; v < g.N(); v++ {
		cv := coarseOf[part[v]]
		for _, nb := range g.adj[v] {
			if nb.To <= v {
				continue // each undirected edge once
			}
			cu := coarseOf[part[nb.To]]
			if cu == cv {
				out.selfLoop[cv] += nb.Weight
			} else {
				out.AddEdge(cv, cu, nb.Weight)
			}
		}
	}
	return out, coarseOf
}

// renumber makes community ids contiguous from 0, ordered by lowest member, so
// output does not depend on internal id churn.
func renumber(part []int) []int {
	first := map[int]int{}
	for i, c := range part {
		if _, ok := first[c]; !ok {
			first[c] = i
		}
	}
	ids := make([]int, 0, len(first))
	for c := range first {
		ids = append(ids, c)
	}
	sort.Slice(ids, func(a, b int) bool { return first[ids[a]] < first[ids[b]] })
	remap := make(map[int]int, len(ids))
	for newID, c := range ids {
		remap[c] = newID
	}
	out := make([]int, len(part))
	for i, c := range part {
		out[i] = remap[c]
	}
	return out
}

// CPMQuality evaluates the objective, for tests and for comparing settings.
func CPMQuality(g *Graph, part []int, gamma float64) float64 {
	cs := newCommunityState(g, part)
	var q float64
	for c := range cs.weight {
		w := cs.weight[c]
		if w == 0 {
			continue
		}
		q += cs.internal[c] - gamma*w*(w-1)/2
	}
	return q
}

// CommunitiesConnected reports whether every community is internally connected —
// the invariant refinement exists to provide, and the thing that distinguishes
// this from Louvain.
func CommunitiesConnected(g *Graph, part []int) bool {
	byComm := map[int][]int{}
	for v, c := range part {
		byComm[c] = append(byComm[c], v)
	}
	for _, members := range byComm {
		if len(members) <= 1 {
			continue
		}
		inComm := make(map[int]bool, len(members))
		for _, v := range members {
			inComm[v] = true
		}
		// BFS from the first member, staying inside the community.
		seen := map[int]bool{members[0]: true}
		queue := []int{members[0]}
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			for _, nb := range g.adj[v] {
				if inComm[nb.To] && !seen[nb.To] {
					seen[nb.To] = true
					queue = append(queue, nb.To)
				}
			}
		}
		if len(seen) != len(members) {
			return false
		}
	}
	return true
}
