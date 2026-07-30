package memory

import (
	"context"
	"math"
	"sort"
)

// Sentence-graph construction for the Leiden chunker.
//
// Nodes are sentences; edges combine two sources:
//
//   - Semantic or lexical similarity, kept sparse via mutual top-k above a floor.
//   - A positional prior linking nearby sentences, decaying with distance.
//
// The positional term is NOT optional. Pure topical clustering happily produces
// communities whose sentences are scattered across a document, which is useless for
// chunking — a chunk has to be a contiguous span of text to be worth returning.
// The positional prior is what makes communities come out mostly contiguous, and
// the post-processing step then splits whatever is left into contiguous runs.

// GraphParams controls sentence-graph construction.
type GraphParams struct {
	// TopK keeps only each sentence's k most similar peers (mutual).
	TopK int
	// SimQuantile is the relative threshold: only pairs above this quantile of the
	// window's observed similarities become candidate edges.
	//
	// A *relative* threshold rather than an absolute one, because similarity scale
	// varies enormously by method and by document. Measured on the same 11-sentence
	// fixture: lexical similarities span 0.00-0.15 while bge-small spans 0.25-0.70.
	// A single absolute floor cannot serve both — 0.35 admits nothing lexically and
	// almost everything semantically.
	SimQuantile float64
	// SimFloor is an absolute safety net, kept low. Pairs below it are dropped
	// regardless of quantile, so a document whose sentences are all unrelated does
	// not get edges purely because something has to be in the top quantile.
	SimFloor float64
	// PositionAlpha scales the positional prior.
	PositionAlpha float64
	// PositionSpan is how many neighbours away the prior reaches.
	PositionSpan int
	// WindowSize caps how many sentences take part in one graph. The similarity
	// matrix is O(n²), so a long document is processed as overlapping windows
	// rather than as one graph.
	WindowSize int
	// WindowOverlap is how many sentences consecutive windows share, so a topic
	// straddling a boundary is not cut blindly.
	WindowOverlap int
}

// DefaultGraphParams returns the starting point. These are initial values to be
// tuned against the eval harness, not measured optima.
func DefaultGraphParams() GraphParams {
	return GraphParams{
		TopK:          10,
		SimQuantile:   0.80,
		SimFloor:      0.05,
		PositionAlpha: 0.6,
		PositionSpan:  3,
		WindowSize:    400,
		WindowOverlap: 40,
	}
}

// SentenceSimilarity computes pairwise similarity for a window of sentences.
//
// Two implementations exist because the cost difference is decisive: the semantic
// one needs an embedding per sentence, which Phase 0 measured at ~46 texts/sec —
// roughly 40 minutes for a 3,000-note vault, against ~9 minutes for chunk-level
// embeddings alone. The lexical one is effectively free. Which produces better
// chunks is an empirical question the eval harness answers.
type SentenceSimilarity interface {
	// Similarities returns an n×n matrix (upper triangle used) for the sentences.
	Similarities(ctx context.Context, sentences []string) ([][]float64, error)
	// Name identifies the method for reporting.
	Name() string
}

// ---------- lexical ----------

// LexicalSimilarity scores sentence pairs with tf-idf cosine over terms combined
// with character 3-gram Dice.
//
// The two are complementary: tf-idf catches shared vocabulary, and the n-gram term
// catches morphological variants and identifiers that tokenize differently. Both
// are computed within the window only, so "rare" means rare in this document —
// which is the right notion for grouping one document's sentences.
type LexicalSimilarity struct {
	// NgramWeight blends the two components: 0 is pure tf-idf, 1 pure n-gram.
	NgramWeight float64
}

func (l LexicalSimilarity) Name() string { return "lexical" }

func (l LexicalSimilarity) Similarities(_ context.Context, sentences []string) ([][]float64, error) {
	n := len(sentences)
	sim := newMatrix(n)
	if n < 2 {
		return sim, nil
	}

	// Document frequency within the window.
	termSets := make([]map[string]int, n)
	df := map[string]int{}
	for i, s := range sentences {
		terms := ExtractTerms(s)
		termSets[i] = terms
		for t := range terms {
			df[t]++
		}
	}

	// tf-idf vectors, L2-normalized so the dot product is a cosine.
	vecs := make([]map[string]float64, n)
	for i, terms := range termSets {
		v := make(map[string]float64, len(terms))
		var norm float64
		for t, tf := range terms {
			idf := math.Log(1 + float64(n)/float64(1+df[t]))
			w := (1 + math.Log(float64(tf))) * idf
			v[t] = w
			norm += w * w
		}
		norm = math.Sqrt(norm)
		if norm > 0 {
			for t := range v {
				v[t] /= norm
			}
		}
		vecs[i] = v
	}

	grams := make([]*NgramSet, n)
	for i, s := range sentences {
		grams[i] = NewNgramSet(s)
	}

	w := l.NgramWeight
	if w <= 0 {
		w = 0.35
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			cos := sparseDot(vecs[i], vecs[j])
			dice := grams[i].Dice(grams[j], 0)
			s := (1-w)*cos + w*dice
			sim[i][j] = s
			sim[j][i] = s
		}
	}
	return sim, nil
}

func sparseDot(a, b map[string]float64) float64 {
	if len(b) < len(a) {
		a, b = b, a
	}
	var dot float64
	for t, wa := range a {
		if wb, ok := b[t]; ok {
			dot += wa * wb
		}
	}
	return dot
}

// ---------- semantic ----------

// SemanticSimilarity scores sentence pairs by embedding cosine. Accurate and
// expensive: one embedding per sentence.
type SemanticSimilarity struct {
	Embedder Embedder
}

func (s SemanticSimilarity) Name() string { return "semantic" }

func (s SemanticSimilarity) Similarities(ctx context.Context, sentences []string) ([][]float64, error) {
	n := len(sentences)
	sim := newMatrix(n)
	if n < 2 {
		return sim, nil
	}
	vecs, err := s.Embedder.EmbedPassages(ctx, sentences)
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			c := Cosine(vecs[i], vecs[j])
			sim[i][j] = c
			sim[j][i] = c
		}
	}
	return sim, nil
}

func newMatrix(n int) [][]float64 {
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	return m
}

// ---------- graph assembly ----------

// BuildSentenceGraph turns a similarity matrix into a sparse weighted graph,
// combining mutual top-k similarity edges with the positional prior.
func BuildSentenceGraph(sim [][]float64, p GraphParams) *Graph {
	n := len(sim)
	g := NewGraph(n)
	if n < 2 {
		return g
	}

	// Resolve the relative threshold for this window.
	threshold := similarityThreshold(sim, p)

	// Mutual top-k: keep an edge only when each endpoint ranks the other highly.
	// One-directional top-k lets a hub sentence attach to everything, which
	// collapses the partition into one community.
	topk := make([]map[int]bool, n)
	for i := 0; i < n; i++ {
		type cand struct {
			j int
			s float64
		}
		cands := make([]cand, 0, n-1)
		for j := 0; j < n; j++ {
			if j != i && sim[i][j] >= threshold {
				cands = append(cands, cand{j, sim[i][j]})
			}
		}
		sort.Slice(cands, func(a, b int) bool {
			if cands[a].s != cands[b].s {
				return cands[a].s > cands[b].s
			}
			return cands[a].j < cands[b].j // deterministic ties
		})
		k := p.TopK
		if k > len(cands) {
			k = len(cands)
		}
		set := make(map[int]bool, k)
		for _, c := range cands[:k] {
			set[c.j] = true
		}
		topk[i] = set
	}

	for i := 0; i < n; i++ {
		for j := range topk[i] {
			if j > i && topk[j][i] {
				g.AddEdge(i, j, sim[i][j])
			}
		}
	}

	// Positional prior: w = α / (1 + d) for d up to PositionSpan.
	if p.PositionAlpha > 0 && p.PositionSpan > 0 {
		for i := 0; i < n; i++ {
			for d := 1; d <= p.PositionSpan && i+d < n; d++ {
				g.AddEdge(i, i+d, p.PositionAlpha/float64(1+d))
			}
		}
	}
	return g
}

// SentenceWindows splits a long sentence list into overlapping windows, bounding
// the O(n²) similarity cost. Returns [start, end) pairs.
func SentenceWindows(n int, p GraphParams) [][2]int {
	size := p.WindowSize
	if size <= 0 || n <= size {
		return [][2]int{{0, n}}
	}
	overlap := p.WindowOverlap
	if overlap < 0 || overlap >= size {
		overlap = size / 10
	}
	step := size - overlap
	var out [][2]int
	for start := 0; start < n; start += step {
		end := start + size
		if end >= n {
			out = append(out, [2]int{start, n})
			break
		}
		out = append(out, [2]int{start, end})
	}
	return out
}

// similarityThreshold resolves the effective edge threshold for one window: the
// higher of the absolute floor and the configured quantile of observed
// similarities. Returning a scale-adapted value is what lets one parameter set
// serve both the lexical and semantic arms.
func similarityThreshold(sim [][]float64, p GraphParams) float64 {
	n := len(sim)
	if n < 2 {
		return p.SimFloor
	}
	q := p.SimQuantile
	if q <= 0 || q >= 1 {
		return p.SimFloor
	}
	vals := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			vals = append(vals, sim[i][j])
		}
	}
	sort.Float64s(vals)
	idx := int(float64(len(vals)) * q)
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	if t := vals[idx]; t > p.SimFloor {
		return t
	}
	return p.SimFloor
}
