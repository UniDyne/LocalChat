package memory

import (
	"math"
	"sort"

	"simple-cot-chat/store"
)

// BM25 parameters. The standard defaults; k1 controls term-frequency saturation and
// b how strongly document length normalizes.
const (
	BM25K1 = 1.2
	BM25B  = 0.75
)

// maxProbeTerms limits how many query terms drive candidate generation. Postings
// for a very common term can cover most of the corpus, so the rarest terms are used
// first — they are far more selective and cheaper to fetch.
const maxProbeTerms = 12

// BM25Scorer scores chunks against a query using corpus statistics from the store.
//
// Computed in Go rather than by DuckDB's `fts` extension, which is not present in
// the prebuilt libraries (§3.0). That is a feature here: the arithmetic is
// unit-testable against hand-computed values, which a black-box extension would not
// be.
type BM25Scorer struct {
	store *store.Store
	// N is the corpus size in chunks; AvgDL the mean chunk length in tokens.
	N     int
	AvgDL float64
}

// NewBM25Scorer loads corpus statistics, recomputing them if they are stale.
func NewBM25Scorer(s *store.Store) (*BM25Scorer, error) {
	n, avgdl, err := s.BM25Stats()
	if err != nil {
		return nil, err
	}
	if avgdl <= 0 {
		avgdl = 1
	}
	return &BM25Scorer{store: s, N: n, AvgDL: avgdl}, nil
}

// Score returns BM25 scores for every chunk matching any query term.
//
// Also returns the terms actually probed, which the caller reports for
// explainability: knowing that a query fell back to two rare terms explains a
// surprising result far better than the score alone.
func (b *BM25Scorer) Score(queryTerms map[string]int) (map[string]float64, []string, error) {
	if len(queryTerms) == 0 || b.N == 0 {
		return map[string]float64{}, nil, nil
	}

	terms := make([]string, 0, len(queryTerms))
	for t := range queryTerms {
		terms = append(terms, t)
	}
	df, err := b.store.TermDF(terms)
	if err != nil {
		return nil, nil, err
	}

	// Order by rarity: a term absent from the corpus contributes nothing, and the
	// rarest present terms are the most selective.
	present := make([]string, 0, len(terms))
	for _, t := range terms {
		if df[t] > 0 {
			present = append(present, t)
		}
	}
	sort.Slice(present, func(i, j int) bool {
		if df[present[i]] != df[present[j]] {
			return df[present[i]] < df[present[j]]
		}
		return present[i] < present[j] // deterministic
	})
	probe := present
	if len(probe) > maxProbeTerms {
		probe = probe[:maxProbeTerms]
	}
	if len(probe) == 0 {
		return map[string]float64{}, nil, nil
	}

	postings, err := b.store.Postings(probe, 0)
	if err != nil {
		return nil, nil, err
	}

	idf := make(map[string]float64, len(probe))
	for _, t := range probe {
		idf[t] = bm25IDF(b.N, df[t])
	}

	scores := make(map[string]float64, 256)
	for _, p := range postings {
		w, ok := idf[p.Term]
		if !ok {
			continue
		}
		dl := float64(p.TokenCount)
		if dl <= 0 {
			dl = 1
		}
		tf := float64(p.TF)
		norm := tf + BM25K1*(1-BM25B+BM25B*dl/b.AvgDL)
		if norm == 0 {
			continue
		}
		scores[p.ChunkID] += w * (tf * (BM25K1 + 1)) / norm
	}
	return scores, probe, nil
}

// bm25IDF is the standard BM25 inverse document frequency with the +0.5 smoothing.
//
// The max(…, 0) floor matters: for a term appearing in more than half the corpus the
// raw formula goes negative, which would let a common term *penalize* a chunk that
// contains it. Clamping is the usual remedy.
func bm25IDF(n, df int) float64 {
	if df <= 0 {
		return 0
	}
	v := math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
	if v < 0 {
		return 0
	}
	return v
}
