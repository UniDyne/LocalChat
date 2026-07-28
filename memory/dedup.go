package memory

import (
	"sort"
	"strings"
	"unicode"
)

// DedupThreshold is the 3-gram Dice coefficient above which a candidate chunk is
// treated as a duplicate and skipped at ingest.
//
// Rejecting at ingest rather than filtering at retrieval keeps the corpus
// statistics honest: a duplicate that reaches the tables inflates df for its
// terms (deflating IDF for exactly the distinctive words BM25 exists to reward),
// gets a fresh ingested_at that skews recency, and defeats per-source argmax
// because each copy has a different source_id. Filtering at query time papers
// over all three instead of preventing them.
//
// Set at 0.9 so that near-identical text is dropped while merely similar
// passages — which are legitimately worth storing separately — survive. §3.5's
// retrieval-time filter at 0.85 catches those, so the two are complementary
// rather than redundant.
const DedupThreshold = 0.9

// trigrams returns the set of character 3-grams of a normalized string.
// Normalization collapses whitespace and case so that reformatting alone does not
// make a duplicate look novel.
func trigrams(s string) map[string]struct{} {
	norm := normalizeForNgrams(s)
	out := make(map[string]struct{})
	r := []rune(norm)
	if len(r) < 3 {
		if len(r) > 0 {
			out[string(r)] = struct{}{}
		}
		return out
	}
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = struct{}{}
	}
	return out
}

func normalizeForNgrams(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// NgramSet is a precomputed 3-gram set. Building it is the expensive part of a
// Dice comparison, so callers that compare one text against many should build
// each set once rather than per pair — doing otherwise made ingestion
// Dice-dominated.
type NgramSet struct {
	grams map[string]struct{}
}

// NewNgramSet precomputes the 3-gram set for a text.
func NewNgramSet(s string) *NgramSet { return &NgramSet{grams: trigrams(s)} }

// Size reports the number of distinct 3-grams.
func (n *NgramSet) Size() int { return len(n.grams) }

// Dice is the Sørensen–Dice coefficient: 2|A∩B| / (|A|+|B|). Chosen over Jaccard
// because it weights the intersection more heavily, matching the intuition that
// two texts sharing most of their substance are duplicates even if one has extra
// material.
//
// atLeast lets a caller skip the intersection entirely when the result cannot
// possibly reach a threshold: since |A∩B| <= min(|A|,|B|), Dice is bounded above
// by 2·min/(|A|+|B|). For duplicate detection most candidate pairs differ enough
// in length to be rejected by that bound alone.
func (n *NgramSet) Dice(other *NgramSet, atLeast float64) float64 {
	la, lb := len(n.grams), len(other.grams)
	if la == 0 && lb == 0 {
		return 1
	}
	if la == 0 || lb == 0 {
		return 0
	}
	small, large := n.grams, other.grams
	if len(large) < len(small) {
		small, large = large, small
	}
	if bound := 2 * float64(len(small)) / float64(la+lb); bound < atLeast {
		return bound // cannot reach the threshold; skip the intersection
	}
	inter := 0
	for g := range small {
		if _, ok := large[g]; ok {
			inter++
		}
	}
	return 2 * float64(inter) / float64(la+lb)
}

// DiceSimilarity compares two strings directly. Convenient for one-off
// comparisons and tests; use NgramSet when comparing one text against many.
func DiceSimilarity(a, b string) float64 {
	return NewNgramSet(a).Dice(NewNgramSet(b), 0)
}

// rarestTerms returns up to n terms from a chunk, preferring the least frequent
// within the chunk itself. Used to fetch dedup candidates: a rare term is far
// more selective than a common one, so this keeps the candidate set small without
// needing corpus-wide df.
func rarestTerms(terms map[string]int, n int) []string {
	type kv struct {
		term string
		tf   int
	}
	all := make([]kv, 0, len(terms))
	for t, c := range terms {
		if len(t) < 4 {
			continue // short tokens are rarely selective
		}
		all = append(all, kv{t, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].tf != all[j].tf {
			return all[i].tf < all[j].tf
		}
		return all[i].term < all[j].term // deterministic
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, x := range all {
		out[i] = x.term
	}
	return out
}
