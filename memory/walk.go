package memory

import (
	"fmt"
	"sort"

	"simple-cot-chat/store"
)

// DefaultDecay is the per-kind score decay applied at each hop (§3.5).
//
// The ordering is a claim about evidence quality, not a tuning artifact:
//
//   - `link` ranks highest because a wikilink is a human assertion that two notes
//     belong together, not a statistical inference. On a vault corpus this is
//     likely the highest-yield edge kind in the system.
//   - `next`/`prev` next, because document adjacency is a fact about the source.
//   - `similar` below that: a measured cosine, but an inference.
//   - `inferred_link` sits deliberately *below* `similar`. An LLM-proposed
//     cross-reference is a guess, and ranking it above a measured similarity would
//     be dishonest about which signal is better evidenced. It earns its place on
//     coverage, not confidence.
//   - `entity` lowest: sharing a rare entity is the weakest of these claims, and
//     the one most prone to hub effects.
func DefaultDecay() map[string]float64 {
	return map[string]float64{
		EdgeLink:         0.6,
		EdgeNext:         0.5,
		EdgePrev:         0.5,
		EdgeSimilar:      0.4,
		EdgeInferredLink: 0.35,
		EdgeEntity:       0.3,
	}
}

// bidirectionalKinds are traversed against their stored direction as well as with
// it. Only the genuinely directed kinds appear here: `similar` and `entity` are
// symmetric and already stored both ways, and next/prev are each other's inverse.
var bidirectionalKinds = []string{EdgeLink, EdgeInferredLink}

// WalkParams bounds the expansion. Every cap here exists because an unbounded
// walk on a real graph returns the whole corpus: a hub chunk with hundreds of
// entity edges would otherwise dominate.
type WalkParams struct {
	// MaxDepth is how many hops from a seed. 2 is the plan's value: depth 1 is
	// "what this chunk points at", depth 2 is "what that points at", and beyond
	// that the decayed score is below anything that would be returned anyway.
	MaxDepth int
	// Seeds is how many top-ranked candidates start the walk.
	Seeds int
	// FrontierPerHop caps how many newly discovered nodes advance to the next hop,
	// keeping the highest-scoring.
	FrontierPerHop int
	// MaxVisited caps the total expanded set.
	MaxVisited int
	// MinScore prunes an expansion whose decayed score cannot matter.
	MinScore float64
	// Kinds restricts traversal to these edge kinds; empty means all. This is what
	// makes `link`-only expansion measurable separately from the rest.
	Kinds []string
	// Decay per kind; nil uses DefaultDecay.
	Decay map[string]float64
	// CoArrivalBonus is the fraction of an expansion score added to a chunk that
	// *also* has a nonzero direct score. Arriving at the same chunk by two routes is
	// real evidence, but it is corroboration rather than a second independent
	// signal, so it is a fraction and not a sum.
	CoArrivalBonus float64
}

// DefaultWalkParams are the plan's bounds.
func DefaultWalkParams() WalkParams {
	return WalkParams{
		MaxDepth:       2,
		Seeds:          10,
		FrontierPerHop: 30,
		MaxVisited:     200,
		MinScore:       1e-4,
		Decay:          DefaultDecay(),
		CoArrivalBonus: 0.25,
	}
}

func (p WalkParams) withDefaults() WalkParams {
	d := DefaultWalkParams()
	if p.MaxDepth == 0 {
		p.MaxDepth = d.MaxDepth
	}
	if p.Seeds == 0 {
		p.Seeds = d.Seeds
	}
	if p.FrontierPerHop == 0 {
		p.FrontierPerHop = d.FrontierPerHop
	}
	if p.MaxVisited == 0 {
		p.MaxVisited = d.MaxVisited
	}
	if p.MinScore == 0 {
		p.MinScore = d.MinScore
	}
	if p.Decay == nil {
		p.Decay = d.Decay
	}
	if p.CoArrivalBonus == 0 {
		p.CoArrivalBonus = d.CoArrivalBonus
	}
	return p
}

// Expansion is one chunk reached by the walk, with how it was reached.
//
// Via and ParentID are not decoration: without them an expanded result cannot be
// explained, and "why did memory return this?" is the question that decides
// whether the walk is helping or adding noise.
type Expansion struct {
	ChunkID  string  `json:"chunkId"`
	Score    float64 `json:"score"`
	Depth    int     `json:"depth"`
	Via      string  `json:"via"`
	ParentID string  `json:"parentId"`
}

// WalkReport describes one expansion, including what its caps cut off.
type WalkReport struct {
	Seeds int `json:"seeds"`
	// Discovered is the expanded set before the MaxVisited cap.
	Discovered int `json:"discovered"`
	// Expanded is how many expansions were returned.
	Expanded int `json:"expanded"`
	// Queries counts edge fetches, one per hop.
	Queries int `json:"queries"`
	// TruncatedFrontier and TruncatedVisited record caps actually biting, so a
	// bounded walk is visible rather than looking like an exhausted one.
	TruncatedFrontier int  `json:"truncatedFrontier"`
	TruncatedVisited  bool `json:"truncatedVisited"`
	// ByKind counts returned expansions per edge kind, which is what shows whether
	// a kind is pulling its weight.
	ByKind map[string]int `json:"byKind"`
}

func (r WalkReport) String() string {
	return fmt.Sprintf("walk: %d seeds -> %d expansions (%d discovered) in %d queries, by kind %v",
		r.Seeds, r.Expanded, r.Discovered, r.Queries, r.ByKind)
}

// edgeSource is the store surface the walk needs. Narrow enough that a test can
// supply a hand-built graph without a database.
type edgeSource interface {
	GetEdgesFromMany(ids []string, bidirectionalKinds ...string) (map[string][]store.MemoryEdge, error)
}

// Walk expands outward from seeds over the edge graph.
//
// seedScores maps chunk id to its fused score. direct is the set of chunks that
// already have a nonzero direct score — reaching one of those is corroboration
// (see CoArrivalBonus), not a discovery, and it is reported as an expansion so the
// caller can apply the bonus, but it does not count toward the discovered set.
//
// Scores propagate as parent x edge_weight x decay(kind), so a chunk two `entity`
// hops out carries at most 9% of its seed's score. That geometric decay, not the
// caps, is what actually keeps expansion from taking over the ranking; the caps
// bound the *work*.
func Walk(src edgeSource, seedScores map[string]float64, direct map[string]bool, p WalkParams) ([]Expansion, WalkReport, error) {
	p = p.withDefaults()
	rep := WalkReport{ByKind: map[string]int{}}
	if len(seedScores) == 0 {
		return nil, rep, nil
	}

	var kindAllowed map[string]bool
	if len(p.Kinds) > 0 {
		kindAllowed = make(map[string]bool, len(p.Kinds))
		for _, k := range p.Kinds {
			kindAllowed[k] = true
		}
	}

	// Only traverse a directed kind backwards when that kind is allowed at all.
	var backwards []string
	for _, k := range bidirectionalKinds {
		if kindAllowed == nil || kindAllowed[k] {
			backwards = append(backwards, k)
		}
	}

	seeds := topNByScore(seedScores, p.Seeds)
	rep.Seeds = len(seeds)

	best := map[string]Expansion{}
	visited := map[string]bool{}
	for _, id := range seeds {
		visited[id] = true
	}

	frontier := make([]Expansion, 0, len(seeds))
	for _, id := range seeds {
		frontier = append(frontier, Expansion{ChunkID: id, Score: seedScores[id]})
	}

	for depth := 1; depth <= p.MaxDepth && len(frontier) > 0; depth++ {
		ids := make([]string, len(frontier))
		parentScore := make(map[string]float64, len(frontier))
		for i, f := range frontier {
			ids[i] = f.ChunkID
			parentScore[f.ChunkID] = f.Score
		}
		edges, err := src.GetEdgesFromMany(ids, backwards...)
		if err != nil {
			return nil, rep, err
		}
		rep.Queries++

		// Candidates discovered at this depth, best score per chunk.
		found := map[string]Expansion{}
		for _, parent := range ids {
			for _, e := range edges[parent] {
				if kindAllowed != nil && !kindAllowed[e.Kind] {
					continue
				}
				decay, ok := p.Decay[e.Kind]
				if !ok || decay <= 0 {
					continue // an unknown kind is not silently given full credit
				}
				score := parentScore[parent] * e.Weight * decay
				if score < p.MinScore {
					continue
				}
				if visited[e.DstChunkID] {
					continue
				}
				if cur, ok := found[e.DstChunkID]; ok && cur.Score >= score {
					continue
				}
				found[e.DstChunkID] = Expansion{
					ChunkID: e.DstChunkID, Score: score, Depth: depth,
					Via: e.Kind, ParentID: parent,
				}
			}
		}
		if len(found) == 0 {
			break
		}

		// Rank this hop's discoveries and keep the strongest. Ties break on chunk
		// id so the same corpus and query always expand the same way.
		ranked := make([]Expansion, 0, len(found))
		for _, e := range found {
			ranked = append(ranked, e)
		}
		sort.Slice(ranked, func(a, b int) bool {
			if ranked[a].Score != ranked[b].Score {
				return ranked[a].Score > ranked[b].Score
			}
			return ranked[a].ChunkID < ranked[b].ChunkID
		})
		if len(ranked) > p.FrontierPerHop {
			rep.TruncatedFrontier += len(ranked) - p.FrontierPerHop
			ranked = ranked[:p.FrontierPerHop]
		}

		next := make([]Expansion, 0, len(ranked))
		for _, e := range ranked {
			if len(best) >= p.MaxVisited {
				rep.TruncatedVisited = true
				break
			}
			visited[e.ChunkID] = true
			best[e.ChunkID] = e
			if !direct[e.ChunkID] {
				rep.Discovered++
			}
			next = append(next, e)
		}
		if rep.TruncatedVisited {
			break
		}
		frontier = next
	}

	out := make([]Expansion, 0, len(best))
	for _, id := range sortedKeys(best) {
		out = append(out, best[id])
		rep.ByKind[best[id].Via]++
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].ChunkID < out[b].ChunkID
	})
	rep.Expanded = len(out)
	return out, rep, nil
}
