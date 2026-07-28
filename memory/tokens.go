package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/normalizer"
	"github.com/sugarme/tokenizer/pretrained"
)

// TokenCounter counts tokens the way the embedding model does. Chunk boundaries
// are expressed in tokens, so an inaccurate counter produces chunks that either
// waste the model's window or overflow it — hence the exact BERT counter is
// preferred and the heuristic is only a fallback.
type TokenCounter interface {
	// Count returns the number of tokens in s, excluding [CLS]/[SEP].
	Count(s string) int
	// Truncate returns the longest prefix of s that fits in n tokens, cut on a
	// token boundary.
	Truncate(s string, n int) string
	// Name identifies the counter for logging and provenance.
	Name() string
}

// ---------- heuristic ----------

// heuristicCounter approximates WordPiece without needing tokenizer.json. It is
// deliberately conservative (it over-counts slightly) so that chunks built with
// it never overflow the real model's window.
type heuristicCounter struct{}

// NewHeuristicCounter returns a tokenizer-free approximation. Used when
// tokenizer.json is not available; see NewBERTCounter for the exact one.
func NewHeuristicCounter() TokenCounter { return heuristicCounter{} }

func (heuristicCounter) Name() string { return "heuristic" }

// Count estimates WordPiece output: short alphabetic words are usually one
// token, long words split into pieces, and punctuation is its own token.
func (heuristicCounter) Count(s string) int {
	n := 0
	for _, w := range splitForCount(s) {
		switch {
		case len(w) == 0:
			// nothing
		case !isWordy(w):
			n++ // punctuation and symbols tokenize individually
		case len(w) <= 6:
			n++
		default:
			// ~4 chars per WordPiece continuation, rounded up.
			n += 1 + (len(w)-6+3)/4
		}
	}
	return n
}

func (h heuristicCounter) Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if h.Count(s) <= n {
		return s
	}
	// Walk words, accumulating until the budget is spent.
	var b strings.Builder
	used := 0
	for _, field := range strings.Fields(s) {
		c := h.Count(field)
		if used+c > n {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(field)
		used += c
	}
	return b.String()
}

func splitForCount(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		default:
			flush()
			out = append(out, string(r))
		}
	}
	flush()
	return out
}

func isWordy(w string) bool {
	for _, r := range w {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return len(w) > 0
}

// ---------- exact BERT counter ----------

// bertCounter uses the model's real tokenizer. It needs only tokenizer.json
// (~0.7 MB), not the 133 MB ONNX weights, so exact token counts are available
// during ingestion even before the embedding model has been provisioned.
type bertCounter struct {
	mu sync.Mutex // sugarme's Tokenizer is not documented as goroutine-safe
	tk *tokenizer.Tokenizer
}

// NewBERTCounter loads the model's tokenizer for exact counts.
func NewBERTCounter(tokenizerJSON string) (TokenCounter, error) {
	tk, err := LoadTokenizer(tokenizerJSON)
	if err != nil {
		return nil, err
	}
	return &bertCounter{tk: tk}, nil
}

func (b *bertCounter) Name() string { return "bert-wordpiece" }

func (b *bertCounter) Count(s string) int {
	if s == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	enc, err := b.tk.EncodeSingle(s, false)
	if err != nil {
		return heuristicCounter{}.Count(s)
	}
	return len(enc.Ids)
}

// Truncate cuts on a token boundary, mapping back to the original string via the
// encoding's offsets so a multi-byte rune is never split.
func (b *bertCounter) Truncate(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	b.mu.Lock()
	enc, err := b.tk.EncodeSingle(s, false)
	b.mu.Unlock()
	if err != nil {
		return heuristicCounter{}.Truncate(s, n)
	}
	if len(enc.Ids) <= n {
		return s
	}
	if len(enc.Offsets) >= n && len(enc.Offsets[n-1]) == 2 {
		if end := enc.Offsets[n-1][1]; end > 0 && end <= len(s) {
			return strings.TrimSpace(s[:end])
		}
	}
	// Offsets unusable: fall back to a rune-safe proportional cut.
	r := []rune(s)
	cut := len(r) * n / len(enc.Ids)
	if cut < 1 {
		cut = 1
	}
	return strings.TrimSpace(string(r[:cut]))
}

// ---------- tokenizer loading (with the sugarme correction shim) ----------

// LoadTokenizer loads tokenizer.json and repairs two spec-compliance bugs in
// sugarme/tokenizer v0.3.0. Both were found by the Phase 0 parity test against
// HuggingFace's reference implementation, and both silently mangle non-ASCII
// text rather than erroring:
//
//  1. `strip_accents: null` is read as false. HuggingFace's rule is
//     `strip_accents.unwrap_or(lowercase)` — null inherits from `lowercase`.
//     bge-small-en-v1.5's tokenizer.json is exactly this shape.
//
//  2. Even with strip_accents true, BertNormalizer does not strip them, because
//     it applies StripAccents WITHOUT first applying NFD. StripAccents removes
//     Unicode Mn code points, but in NFC form "é" is a single precomposed rune
//     with no separate mark to remove. Verified in isolation:
//     BertNormalizer(strip=true) -> "café"   (unchanged)
//     NFD then StripAccents      -> "cafe"   (correct)
//
// Unfixed, every accented word becomes [UNK]: "Café naïve résumé" tokenizes to
// [UNK] [UNK] [UNK]. Nothing errors; retrieval just quietly degrades on any
// corpus containing non-ASCII text.
//
// Because this library is not a faithful port, every tokenizer behavior we rely
// on needs a fixture rather than an assumption — see the parity test.
func LoadTokenizer(tokenizerJSON string) (*tokenizer.Tokenizer, error) {
	tk, err := pretrained.FromFile(tokenizerJSON)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer %s: %w", tokenizerJSON, err)
	}

	cfg, err := readNormalizerConfig(tokenizerJSON)
	if err != nil {
		return nil, err
	}
	if cfg == nil || cfg.Type != "BertNormalizer" {
		return tk, nil
	}

	stripAccents := cfg.Lowercase // HF's null-defaulting rule
	if cfg.StripAccents != nil {
		stripAccents = *cfg.StripAccents
	}

	norms := make([]normalizer.Normalizer, 0, 3)
	if stripAccents {
		norms = append(norms, normalizer.NewNFD(), normalizer.NewStripAccents())
	}
	// strip=false: accents are handled by the two normalizers above, and leaving
	// it true would be a no-op that obscures where the work happens.
	norms = append(norms, normalizer.NewBertNormalizer(
		cfg.CleanText, cfg.Lowercase, cfg.HandleChineseChars, false,
	))
	tk.WithNormalizer(normalizer.NewSequence(norms))
	return tk, nil
}

type bertNormalizerConfig struct {
	Type               string `json:"type"`
	CleanText          bool   `json:"clean_text"`
	HandleChineseChars bool   `json:"handle_chinese_chars"`
	StripAccents       *bool  `json:"strip_accents"`
	Lowercase          bool   `json:"lowercase"`
}

func readNormalizerConfig(path string) (*bertNormalizerConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var outer struct {
		Normalizer *bertNormalizerConfig `json:"normalizer"`
	}
	if err := json.Unmarshal(b, &outer); err != nil {
		return nil, fmt.Errorf("parse tokenizer.json normalizer: %w", err)
	}
	return outer.Normalizer, nil
}
