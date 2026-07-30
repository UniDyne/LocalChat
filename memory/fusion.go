package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"simple-cot-chat/store"
)

// Fusion defaults. Candidate pool sizes per arm, and the signal weights.
const (
	CandidatesBM25   = 200
	CandidatesVector = 200
	CandidatesEntity = 100

	// RetrievalDedupThreshold drops a selected chunk too similar to one already
	// chosen. Complementary to ingest-side rejection at 0.9: that stops outright
	// copies entering the corpus, this stops merely-similar passages both being
	// *returned*.
	RetrievalDedupThreshold = 0.85

	// DefaultTokenBudget caps the returned context.
	DefaultTokenBudget = 2000
)

// FusionMode selects how the four signals combine.
type FusionMode string

const (
	// FusionWeighted is a weighted sum of min-max normalized scores.
	FusionWeighted FusionMode = "weighted"
	// FusionRRF is Reciprocal Rank Fusion, which combines ranks instead of scores.
	//
	// Kept behind the same interface because a weighted sum of normalized scores is
	// sensitive to outliers: one candidate with a freak BM25 value compresses every
	// other candidate into a narrow band. RRF is immune to that, at the cost of
	// discarding magnitude. Which wins is an empirical question for the eval set.
	FusionRRF FusionMode = "rrf"
)

// Weights are the four signal weights for FusionWeighted.
type Weights struct {
	BM25   float64
	Vector float64
	Entity float64
	Ngram  float64
}

// DefaultWeights are tuned, not guessed.
//
// The plan proposed 0.35/0.35/0.15/0.15. Measured against the 50-query eval set on
// the repository's own documentation, that ranked LAST of ten candidate settings
// (MRR 0.593). These weights scored MRR 0.657, R@1 0.540, R@5 0.780.
//
// The lesson from the sweep: the vector signal deserves far more weight than the plan
// assumed and BM25 rather less. Single-signal ablations agree — vector alone had the
// best R@1 (0.520) of any individual signal, while entity alone was almost useless
// for *ranking* (R@1 0.020) though still worth 0.500 R@5 as a recall net. Entity and
// n-gram earn small weights: they add recall but cost precision when weighted at
// 0.15.
//
// Caveat on precision: the corpus is three technical documents, and differences below
// roughly 0.02 MRR are within noise for 50 queries. The gap from the plan's defaults
// to these (0.064) is about three queries' worth, which is not.
//
// **Phase 8 update — these numbers are stale and the weights are deliberately not
// changed.** Two fixes altered the entity signal's behaviour: query-side extraction
// became a dictionary lookup rather than a capitalization guess, and the proper-noun
// regex stopped matching across line breaks. The entity signal improved substantially
// (entity-only MRR ~0.16 -> ~0.29) and these same weights scored MRR 0.630, R@1 0.520,
// R@5 0.760 at the time — 0.607 / 0.480 / 0.760 after Phase 9's documentation edits
// changed the eval corpus, which is read live from the repository's own Markdown. Treat
// the ordering of a sweep as the signal and absolute values as corpus-dependent. An 18-point re-sweep put the best setting at 0.30/0.45/0.05/0.20
// (MRR 0.643, R@5 0.800) — a 0.013 improvement, inside the noise floor, with the top
// four candidates separated by one rank position on one query.
//
// So the honest state is: tuned against a signal that no longer behaves the same way,
// and not re-tuned, because re-tuning on a three-document corpus fits noise. Phase 9's
// tuning pass should re-sweep from scratch on a larger corpus rather than adjust these.
func DefaultWeights() Weights {
	return Weights{BM25: 0.20, Vector: 0.60, Entity: 0.10, Ngram: 0.10}
}

// SearchOptions configures a search.
type SearchOptions struct {
	Limit       int
	TokenBudget int
	Mode        FusionMode
	Weights     Weights
	// SourceTypes filters by source type; empty means all.
	SourceTypes []string
	// Since and Until filter by ingestion date (RFC3339 or YYYY-MM-DD); empty means
	// unbounded.
	Since, Until string
	// ExcludeSessionID drops memory derived from one session — in practice the
	// session doing the searching.
	//
	// Needed since Phase 7 made conversations ingest automatically: without it, a
	// search can return the turns of the very conversation it is running inside,
	// which are already in the model's context. That spends the token budget to say
	// something the model can already see, and displaces a result that would have
	// been new. The tool's description discourages it, but a filter is a guarantee
	// and a description is a hope.
	ExcludeSessionID string
	// Explain includes the per-signal breakdown on each result. Without it, tuning
	// the weights is guesswork.
	Explain bool
	// Expand runs the graph walk over the fused top-K, admitting chunks that no
	// direct signal found. That is the whole purpose of the edge graph, and also its
	// risk: expansion can only help recall, so it is opt-in per search and measured
	// against the eval set rather than assumed beneficial.
	Expand bool
	// Walk tunes the expansion; the zero value uses DefaultWalkParams.
	Walk WalkParams
	// Candidates overrides the per-arm candidate pool sizes; zero fields take the
	// CandidatesBM25/Vector/Entity defaults.
	//
	// Tunable because the pool size relative to the corpus determines whether the
	// graph walk can contribute anything at all: when the pool already covers every
	// chunk, expansion is a no-op by construction. Measuring expansion on a small
	// corpus therefore requires scaling the pool down to the ratio a real vault
	// would have — see TestEvalGraphExpansion.
	Candidates CandidateLimits
}

// CandidateLimits sizes the three candidate-generation arms.
type CandidateLimits struct {
	BM25   int
	Vector int
	Entity int
}

func (c CandidateLimits) withDefaults() CandidateLimits {
	if c.BM25 <= 0 {
		c.BM25 = CandidatesBM25
	}
	if c.Vector <= 0 {
		c.Vector = CandidatesVector
	}
	if c.Entity <= 0 {
		c.Entity = CandidatesEntity
	}
	return c
}

func (o SearchOptions) withDefaults() SearchOptions {
	if o.Limit <= 0 {
		o.Limit = 8
	}
	if o.TokenBudget <= 0 {
		o.TokenBudget = DefaultTokenBudget
	}
	if o.Mode == "" {
		o.Mode = FusionWeighted
	}
	if o.Weights == (Weights{}) {
		o.Weights = DefaultWeights()
	}
	o.Candidates = o.Candidates.withDefaults()
	return o
}

// Signals holds one candidate's raw and normalized signal values.
type Signals struct {
	BM25   float64 `json:"bm25"`
	Vector float64 `json:"vector"`
	Entity float64 `json:"entity"`
	Ngram  float64 `json:"ngram"`
}

// Result is one retrieved chunk.
type Result struct {
	ChunkID     string  `json:"chunkId"`
	SourceRef   string  `json:"sourceRef"`
	SourceType  string  `json:"sourceType"`
	Title       string  `json:"title"`
	HeadingPath string  `json:"headingPath"`
	Text        string  `json:"text"`
	TokenCount  int     `json:"tokenCount"`
	Score       float64 `json:"score"`
	// Raw and Normalized are populated when Explain is set.
	Raw        *Signals `json:"raw,omitempty"`
	Normalized *Signals `json:"normalized,omitempty"`
	// Expanded marks a result the graph walk found rather than a direct signal.
	// Via names the edge kind it arrived by and Depth how many hops out it sits.
	Expanded bool   `json:"expanded,omitempty"`
	Via      string `json:"via,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}

// SearchReport describes how a search was answered, for debugging and tuning.
type SearchReport struct {
	Query          string        `json:"query"`
	Mode           FusionMode    `json:"mode"`
	Candidates     int           `json:"candidates"`
	FromBM25       int           `json:"fromBm25"`
	FromVector     int           `json:"fromVector"`
	FromEntity     int           `json:"fromEntity"`
	ProbeTerms     []string      `json:"probeTerms"`
	QueryEntities  []string      `json:"queryEntities"`
	VectorSkipped  string        `json:"vectorSkipped,omitempty"`
	DedupedResults int           `json:"dedupedResults"`
	BudgetTokens   int           `json:"budgetTokens"`
	Duration       time.Duration `json:"duration"`
	// Walk describes the expansion, when one ran.
	Walk *WalkReport `json:"walk,omitempty"`
	// ExpandedCandidates is how many walked-to chunks joined the candidate set as
	// new chunks; ExpandedReturned is how many survived into the results. The gap
	// between them is what says whether expansion is finding relevant material or
	// just more material.
	ExpandedCandidates int `json:"expandedCandidates"`
	ExpandedReturned   int `json:"expandedReturned"`
}

// Search runs the four-signal fused retrieval.
//
// Stage 1 generates candidates from three cheap arms (BM25, vector, entity), stage 2
// scores all four signals on every candidate, stage 3 normalizes and fuses, stage 4
// takes the best chunk per source, and stage 5 fills a token budget.
//
// Degrades rather than failing when vectors are unavailable: the other three signals
// still work, which is the whole reason an unprovisioned model leaves the app usable.
func (sys *System) Search(ctx context.Context, query string, opts SearchOptions) ([]Result, SearchReport, error) {
	started := time.Now()
	opts = opts.withDefaults()
	rep := SearchReport{Query: query, Mode: opts.Mode, BudgetTokens: opts.TokenBudget}

	if strings.TrimSpace(query) == "" {
		return nil, rep, fmt.Errorf("empty query")
	}

	// --- Stage 1: candidate generation ---

	queryTerms := ExtractTerms(query)
	bm25, err := NewBM25Scorer(sys.Store)
	if err != nil {
		return nil, rep, err
	}
	bmScores, probe, err := bm25.Score(queryTerms)
	if err != nil {
		return nil, rep, err
	}
	rep.ProbeTerms = probe
	rep.FromBM25 = len(bmScores)

	vecScores := map[string]float64{}
	if emb := sys.Embedder(); emb != nil {
		qv, err := emb.EmbedQuery(ctx, query)
		if err != nil {
			return nil, rep, fmt.Errorf("embed query: %w", err)
		}
		vecScores, err = sys.Store.NearestChunkIDs(qv, opts.Candidates.Vector)
		if err != nil {
			return nil, rep, err
		}
	} else {
		rep.VectorSkipped = sys.UnavailableReason()
	}
	rep.FromVector = len(vecScores)

	entValues, err := sys.queryEntityValues(query)
	if err != nil {
		return nil, rep, err
	}
	rep.QueryEntities = entValues

	entMatches, err := sys.Store.ChunksByEntities(entValues, opts.Candidates.Entity*4)
	if err != nil {
		return nil, rep, err
	}
	entHits := map[string]map[string]bool{}
	for _, m := range entMatches {
		if entHits[m.ChunkID] == nil {
			entHits[m.ChunkID] = map[string]bool{}
		}
		entHits[m.ChunkID][m.ValueNorm] = true
	}
	rep.FromEntity = len(entHits)

	// Union, with each arm capped so one cannot swamp the pool.
	pool := map[string]bool{}
	for _, id := range topNByScore(bmScores, opts.Candidates.BM25) {
		pool[id] = true
	}
	for _, id := range topNByScore(vecScores, opts.Candidates.Vector) {
		pool[id] = true
	}
	for i, id := range sortedKeys(entHits) {
		if i >= opts.Candidates.Entity {
			break
		}
		pool[id] = true
	}
	if len(pool) == 0 {
		rep.Duration = time.Since(started)
		return nil, rep, nil
	}

	ids := make([]string, 0, len(pool))
	for id := range pool {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic downstream ordering
	rep.Candidates = len(ids)

	meta, err := sys.Store.ChunksByIDs(ids)
	if err != nil {
		return nil, rep, err
	}

	// --- Stage 2: score all four signals ---

	queryNgrams := NewNgramSet(query)
	querySet := map[string]bool{}
	for _, v := range entValues {
		querySet[v] = true
	}

	type scored struct {
		chunk *store.ScoredChunk
		raw   Signals
	}
	cands := make([]scored, 0, len(ids))
	for _, id := range ids {
		c, ok := meta[id]
		if !ok {
			continue
		}
		if !passesFilters(c, opts) {
			continue
		}
		s := scored{chunk: c}
		s.raw.BM25 = bmScores[id]
		s.raw.Vector = vecScores[id]
		s.raw.Entity = entityOverlap(querySet, c.EntityValues)
		s.raw.Ngram = queryNgrams.Dice(NewNgramSet(c.Text), 0)
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		rep.Duration = time.Since(started)
		return nil, rep, nil
	}

	// --- Stage 3: normalize and fuse ---

	fused := make([]float64, len(cands))
	norms := make([]Signals, len(cands))

	switch opts.Mode {
	case FusionRRF:
		// Rank each signal independently, then sum 1/(k+rank).
		const rrfK = 60.0
		rankOf := func(get func(int) float64) []int {
			idx := make([]int, len(cands))
			for i := range idx {
				idx[i] = i
			}
			sort.SliceStable(idx, func(a, b int) bool { return get(idx[a]) > get(idx[b]) })
			ranks := make([]int, len(cands))
			for r, i := range idx {
				ranks[i] = r + 1
			}
			return ranks
		}
		rb := rankOf(func(i int) float64 { return cands[i].raw.BM25 })
		rv := rankOf(func(i int) float64 { return cands[i].raw.Vector })
		re := rankOf(func(i int) float64 { return cands[i].raw.Entity })
		rn := rankOf(func(i int) float64 { return cands[i].raw.Ngram })
		for i := range cands {
			// A signal that is zero for a candidate contributes nothing, so a
			// missing arm does not award rank credit it has not earned.
			add := func(raw float64, rank int) float64 {
				if raw <= 0 {
					return 0
				}
				return 1 / (rrfK + float64(rank))
			}
			fused[i] = add(cands[i].raw.BM25, rb[i]) + add(cands[i].raw.Vector, rv[i]) +
				add(cands[i].raw.Entity, re[i]) + add(cands[i].raw.Ngram, rn[i])
			norms[i] = Signals{
				BM25: float64(rb[i]), Vector: float64(rv[i]),
				Entity: float64(re[i]), Ngram: float64(rn[i]),
			}
		}
	default:
		nb := minMax(len(cands), func(i int) float64 { return cands[i].raw.BM25 })
		nv := minMax(len(cands), func(i int) float64 { return cands[i].raw.Vector })
		ne := minMax(len(cands), func(i int) float64 { return cands[i].raw.Entity })
		nn := minMax(len(cands), func(i int) float64 { return cands[i].raw.Ngram })
		w := opts.Weights
		for i := range cands {
			norms[i] = Signals{BM25: nb[i], Vector: nv[i], Entity: ne[i], Ngram: nn[i]}
			fused[i] = w.BM25*nb[i] + w.Vector*nv[i] + w.Entity*ne[i] + w.Ngram*nn[i]
		}
	}

	// --- Stage 3.5: graph walk expansion ---
	//
	// Runs after fusion because the walk's seeds are the fused top-K, and its scores
	// are parent-relative: an expansion is only ever a fraction of the score of the
	// chunk that led to it. That is what lets expanded and direct results share one
	// ranking without a second normalization step.

	expInfo := make([]*Expansion, len(cands))
	if opts.Expand {
		idx := make(map[string]int, len(cands))
		seedScores := make(map[string]float64, len(cands))
		direct := make(map[string]bool, len(cands))
		for i := range cands {
			id := cands[i].chunk.ChunkID
			idx[id] = i
			if fused[i] > 0 {
				seedScores[id] = fused[i]
			}
			r := cands[i].raw
			if r.BM25 > 0 || r.Vector > 0 || r.Entity > 0 || r.Ngram > 0 {
				direct[id] = true
			}
		}

		exps, wr, err := Walk(sys.Store, seedScores, direct, opts.Walk)
		if err != nil {
			return nil, rep, err
		}
		rep.Walk = &wr

		// One metadata query for everything the walk reached that is not already a
		// candidate.
		var newIDs []string
		for _, e := range exps {
			if _, ok := idx[e.ChunkID]; !ok {
				newIDs = append(newIDs, e.ChunkID)
			}
		}
		var newMeta map[string]*store.ScoredChunk
		if len(newIDs) > 0 {
			sort.Strings(newIDs)
			newMeta, err = sys.Store.ChunksByIDs(newIDs)
			if err != nil {
				return nil, rep, err
			}
		}

		bonus := opts.Walk.withDefaults().CoArrivalBonus
		for _, e := range exps {
			if i, ok := idx[e.ChunkID]; ok {
				// Corroboration: reached by the walk *and* found directly.
				fused[i] += bonus * e.Score
				continue
			}
			c := newMeta[e.ChunkID]
			if c == nil || !passesFilters(c, opts) {
				continue
			}
			// Raw signals stay zero, which is exactly the point: this chunk was
			// invisible to all four direct signals.
			ex := e
			cands = append(cands, scored{chunk: c})
			fused = append(fused, e.Score)
			norms = append(norms, Signals{})
			expInfo = append(expInfo, &ex)
			rep.ExpandedCandidates++
		}
	}

	// --- Stage 4: best chunk per source ---

	bestBySource := map[string]int{}
	for i := range cands {
		src := cands[i].chunk.SourceID
		if j, ok := bestBySource[src]; !ok || fused[i] > fused[j] {
			bestBySource[src] = i
		}
	}
	picked := make([]int, 0, len(bestBySource))
	for _, i := range bestBySource {
		picked = append(picked, i)
	}
	sort.Slice(picked, func(a, b int) bool {
		if fused[picked[a]] != fused[picked[b]] {
			return fused[picked[a]] > fused[picked[b]]
		}
		return cands[picked[a]].chunk.ChunkID < cands[picked[b]].chunk.ChunkID
	})

	// --- Stage 5: dedup near-identical text, then fill the token budget ---

	var out []Result
	var chosen []*NgramSet
	usedTokens := 0
	for _, i := range picked {
		if len(out) >= opts.Limit {
			break
		}
		c := cands[i].chunk
		set := NewNgramSet(c.Text)
		if isDuplicateAt(set, chosen, RetrievalDedupThreshold) {
			rep.DedupedResults++
			continue
		}
		tok := c.TokenCount
		if tok <= 0 {
			tok = len(c.Text) / 4
		}
		if usedTokens+tok > opts.TokenBudget && len(out) > 0 {
			continue // try smaller later candidates rather than stopping outright
		}
		chosen = append(chosen, set)
		usedTokens += tok

		r := Result{
			ChunkID: c.ChunkID, SourceRef: c.SourceRef, SourceType: c.SourceType,
			Title: c.Title, HeadingPath: c.HeadingPath, Text: c.Text,
			TokenCount: tok, Score: fused[i],
		}
		if e := expInfo[i]; e != nil {
			r.Expanded, r.Via, r.Depth = true, e.Via, e.Depth
			rep.ExpandedReturned++
		}
		if opts.Explain {
			raw := cands[i].raw
			n := norms[i]
			r.Raw = &raw
			r.Normalized = &n
		}
		out = append(out, r)
	}

	rep.Duration = time.Since(started)
	return out, rep, nil
}

// MaxQueryEntityWords is the longest multi-word entity a query is matched against.
// Entity values longer than this exist (a full file path with spaces), but the n-gram
// count grows with the cap and four words covers the overwhelming majority.
const MaxQueryEntityWords = 4

// queryEntityValues finds which of the corpus's known entities appear in the query.
//
// Two sources, combined:
//
//   - **Dictionary lookup over word n-grams.** The corpus's entity vocabulary is
//     known, so this asks which known entities the query mentions rather than trying
//     to recognize entities in it from scratch. Running the corpus-side heuristics on a
//     query does not work — they key on capitalization and a typed question is
//     lowercase — which left 47 of 50 eval queries with no entities at all and the
//     entity signal effectively inert (§4, Phase 8).
//   - **The heuristic extractor**, still, for the shapes n-grams split badly: dates,
//     paths and numbers whose punctuation is not word-boundary-like.
//
// The lookup is what makes corpus-side enrichment reachable: every entity the LLM pass
// adds becomes a term a query can match, whereas before it was invisible.
func (sys *System) queryEntityValues(query string) ([]string, error) {
	set := map[string]bool{}
	// The heuristic tier, for punctuation-heavy shapes.
	for _, e := range ExtractEntities(query, nil, nil) {
		set[e.ValueNorm] = true
	}

	if matched, err := sys.Store.ExistingEntityValues(queryNgrams(query)); err != nil {
		return nil, err
	} else {
		for _, v := range matched {
			set[v] = true
		}
	}

	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// queryNgrams builds the 1..MaxQueryEntityWords word n-grams of a query, normalized
// the same way entity values are stored.
func queryNgrams(query string) []string {
	words := strings.Fields(strings.ToLower(query))
	for i, w := range words {
		// Trim sentence punctuation but keep the characters that appear inside entity
		// values — dots in "store.open", slashes in "conf/deploy.yaml", hyphens in
		// dates and identifiers.
		words[i] = strings.Trim(w, `.,;:!?"'()[]{}`)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(words)*MaxQueryEntityWords)
	for n := 1; n <= MaxQueryEntityWords; n++ {
		for i := 0; i+n <= len(words); i++ {
			g := strings.Join(words[i:i+n], " ")
			if g == "" || seen[g] {
				continue
			}
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// entityOverlap is a weighted Jaccard over the query's and chunk's entity sets.
func entityOverlap(query map[string]bool, chunkVals []string) float64 {
	if len(query) == 0 || len(chunkVals) == 0 {
		return 0
	}
	chunkSet := make(map[string]bool, len(chunkVals))
	for _, v := range chunkVals {
		chunkSet[v] = true
	}
	inter := 0
	for v := range query {
		if chunkSet[v] {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	union := len(query) + len(chunkSet) - inter
	return float64(inter) / float64(union)
}

// minMax normalizes a signal within the candidate set.
//
// Guards the degenerate case where every candidate has the same value. For
// *ranking* that signal carries no information either way, since a constant is a
// constant offset on every fused score — so the choice of constant is free, and it
// is not free downstream:
//
//   - hi == lo == 0: the signal is absent. Normalizes to 0.
//   - hi == lo > 0: the signal is present and identical for everyone. Normalizes to
//     1, not 0.
//
// The second case used to return 0, which made a single-candidate search score
// 0.0 on every signal and therefore 0.0 overall. Ranking did not care, but the
// graph walk seeds itself from candidates with a positive fused score, so it got
// no seeds and expansion silently did nothing. Found by the Phase 6 exit-criterion
// test on a query with exactly one candidate; a narrow case, but a real one on any
// query with a single BM25 hit and no vectors.
func minMax(n int, get func(int) float64) []float64 {
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := 0; i < n; i++ {
		v := get(i)
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi-lo < 1e-12 {
		if hi > 0 {
			for i := range out {
				out[i] = 1
			}
		}
		return out
	}
	for i := 0; i < n; i++ {
		out[i] = (get(i) - lo) / (hi - lo)
	}
	return out
}

func passesFilters(c *store.ScoredChunk, opts SearchOptions) bool {
	if opts.ExcludeSessionID != "" && c.SessionID == opts.ExcludeSessionID {
		return false
	}
	if len(opts.SourceTypes) > 0 {
		ok := false
		for _, t := range opts.SourceTypes {
			if c.SourceType == t {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if opts.Since != "" && c.IngestedAt != "" && c.IngestedAt < opts.Since {
		return false
	}
	if opts.Until != "" && c.IngestedAt != "" && c.IngestedAt > opts.Until {
		return false
	}
	return true
}

func topNByScore(scores map[string]float64, n int) []string {
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if scores[ids[a]] != scores[ids[b]] {
			return scores[ids[a]] > scores[ids[b]]
		}
		return ids[a] < ids[b]
	})
	if len(ids) > n {
		ids = ids[:n]
	}
	return ids
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
