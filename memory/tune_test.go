package memory

import (
	"fmt"
	"sort"
	"testing"
)

// TestTuneWeights searches a small set of weight hypotheses rather than a full grid.
//
// The ablations showed entity and n-gram helping recall but hurting precision at 0.15
// each, so the hypotheses cluster around downweighting them while keeping their
// recall contribution.
func TestTuneWeights(t *testing.T) {
	if testing.Short() {
		t.Skip("tuning is slow; skipped with -short")
	}
	paths, err := FindModel()
	if err != nil {
		t.Skip("model not provisioned")
	}
	emb, err := NewONNXEmbedder(ONNXConfig{ModelDir: paths.Dir, IntraOpThreads: 4})
	if err != nil {
		t.Skipf("onnx: %v", err)
	}
	defer emb.Close()

	sys := newEvalSystem(t, emb)

	cands := []struct {
		name string
		w    Weights
	}{
		{"plan's 35/35/15/15", Weights{0.35, 0.35, 0.15, 0.15}},
		// Phase 8 re-tune: the entity signal changed character when query-side
		// extraction became a dictionary lookup instead of a capitalization guess, so
		// weights tuned against a signal that fired on 3 of 50 queries no longer
		// describe the same system. These probe upward on entity.
		{"20/55/15/10", Weights{0.20, 0.55, 0.15, 0.10}},
		{"20/50/20/10", Weights{0.20, 0.50, 0.20, 0.10}},
		{"15/55/20/10", Weights{0.15, 0.55, 0.20, 0.10}},
		{"20/60/15/05", Weights{0.20, 0.60, 0.15, 0.05}},
		{"25/50/15/10", Weights{0.25, 0.50, 0.15, 0.10}},
		{"20/65/10/05", Weights{0.20, 0.65, 0.10, 0.05}},
		{"20/60/05/15", Weights{0.20, 0.60, 0.05, 0.15}},
		{"20/60/00/20", Weights{0.20, 0.60, 0.00, 0.20}},
		{"40/40/10/10", Weights{0.40, 0.40, 0.10, 0.10}},
		{"45/45/05/05", Weights{0.45, 0.45, 0.05, 0.05}},
		{"50/50/00/00", Weights{0.50, 0.50, 0.00, 0.00}},
		{"30/50/10/10", Weights{0.30, 0.50, 0.10, 0.10}},
		{"25/55/10/10", Weights{0.25, 0.55, 0.10, 0.10}},
		{"30/45/05/20", Weights{0.30, 0.45, 0.05, 0.20}},
		{"35/45/10/10", Weights{0.35, 0.45, 0.10, 0.10}},
		{"20/60/10/10", Weights{0.20, 0.60, 0.10, 0.10}},
		{"40/50/05/05", Weights{0.40, 0.50, 0.05, 0.05}},
	}

	var scores []evalScore
	for _, c := range cands {
		sc := evaluate(t, c.name, sys, SearchOptions{Mode: FusionWeighted, Weights: c.w})
		scores = append(scores, sc)
	}
	// Rank by MRR, then by R@5 as tie-break: MRR rewards putting the right chunk
	// first, R@5 that it is found at all.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].mrr != scores[j].mrr {
			return scores[i].mrr > scores[j].mrr
		}
		return scores[i].recallAt5 > scores[j].recallAt5
	})
	fmt.Println("\n=== weight tuning (50 queries) ===")
	for i, sc := range scores {
		fmt.Printf("  %2d. %s\n", i+1, sc.String())
	}
	fmt.Println()
}
