package memory

import (
	"regexp"
	"strings"
	"unicode"

	"simple-cot-chat/store"
)

// ExtractTerms tokenizes text for BM25. Stopwords are deliberately kept: IDF
// already discounts them, and dropping them would break phrase-ish queries where
// a common word carries the distinction.
//
// Identifiers are emitted twice — once whole ("tools_plan.go") and once split
// into parts ("tools", "plan", "go") — so a query can match either the exact
// token or a word inside it. That is cheap here and saves the n-gram signal from
// carrying the whole burden of identifier matching.
func ExtractTerms(text string) map[string]int {
	terms := map[string]int{}
	for _, raw := range tokenizeWords(text) {
		w := strings.ToLower(raw)
		if w == "" || len(w) > 64 {
			continue
		}
		terms[w]++
		if isIdentifierLike(w) {
			for _, part := range splitIdentifier(w) {
				if len(part) > 1 {
					terms[part]++
				}
			}
		}
	}
	return terms
}

// tokenizeWords splits on whitespace and punctuation, but keeps runs joined by
// the characters that appear inside identifiers and paths.
func tokenizeWords(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.Trim(cur.String(), "._-/"))
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		case r == '_' || r == '-' || r == '.' || r == '/':
			// Only meaningful between alphanumerics; a trailing one is trimmed.
			if cur.Len() > 0 {
				cur.WriteRune(r)
			}
		default:
			flush()
		}
	}
	flush()
	res := out[:0]
	for _, w := range out {
		if w != "" {
			res = append(res, w)
		}
	}
	return res
}

func isIdentifierLike(w string) bool {
	return strings.ContainsAny(w, "._-/")
}

func splitIdentifier(w string) []string {
	return strings.FieldsFunc(w, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/'
	})
}

// ---------- entities ----------

var (
	// A run of capitalized words: "Florida Hardware", "DuckDB".
	//
	// The separator is `[ \t]+`, not `\s+`. With `\s+` the run crossed line breaks and
	// swallowed a heading plus the next sentence's opening word — a real bug, which
	// produced entities like "Deployment\n\nThe Falkirk Wheel" that no query could ever
	// match. Capitalization runs are a within-line phenomenon; a line break ends a name.
	properNounRe = regexp.MustCompile(`\b(\p{Lu}[\p{L}\d]*(?:[ \t]+\p{Lu}[\p{L}\d]*)*)\b`)
	// ISO and common written dates.
	isoDateRe   = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	writtenDate = regexp.MustCompile(`\b((?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.?\s+\d{1,2},?\s+\d{4})\b`)
	// File paths and identifiers with a recognizable shape.
	pathRe = regexp.MustCompile(`\b([\w.-]+/[\w./-]+|\w+\.(?:go|js|ts|md|py|json|yaml|yml|sql|sh|html|css|txt))\b`)
	// Numbers with units or currency; bare integers are too noisy to be useful.
	numberRe = regexp.MustCompile(`([$€£]\s?\d[\d,]*(?:\.\d+)?|\b\d[\d,]*(?:\.\d+)?\s?(?:%|ms|s|kb|mb|gb|tb|px|em|rem)\b)`)
)

// commonSentenceStarters are capitalized only because they begin a sentence, so
// treating them as proper nouns would flood the entity table with noise.
var commonSentenceStarters = map[string]bool{
	"the": true, "a": true, "an": true, "this": true, "that": true, "these": true,
	"those": true, "it": true, "we": true, "i": true, "you": true, "they": true,
	"he": true, "she": true, "there": true, "here": true, "if": true, "when": true,
	"while": true, "for": true, "and": true, "but": true, "or": true, "so": true,
	"in": true, "on": true, "at": true, "to": true, "from": true, "by": true,
	"with": true, "without": true, "as": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "no": true, "not": true, "yes": true,
	"note": true, "however": true, "because": true, "since": true, "after": true,
	"before": true, "once": true, "then": true, "also": true, "both": true,
}

// trimLeadingArticle drops a leading English article from a capitalized phrase,
// unless the article is the whole of it.
func trimLeadingArticle(v string) string {
	for _, art := range []string{"The ", "A ", "An "} {
		if strings.HasPrefix(v, art) {
			rest := strings.TrimSpace(v[len(art):])
			if rest != "" {
				return rest
			}
		}
	}
	return v
}

// ExtractEntities pulls heuristic entities from text, plus the vault-native
// signals (tags, frontmatter dates) supplied by the caller.
//
// Heuristics are noisy by design: §3.3 commits to an LLM pass for real quality,
// and this layer exists so search works before that pass has run. Every entity is
// marked with the extractor that produced it, so the tiers can be weighted
// separately or the LLM tier rolled back wholesale.
func ExtractEntities(text string, tags []string, dates []string) []store.ChunkEntity {
	seen := map[string]*store.ChunkEntity{}
	add := func(kind, value, extractor string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 {
			return
		}
		norm := strings.ToLower(value)
		key := kind + "\x00" + norm
		if e, ok := seen[key]; ok {
			e.Count++
			return
		}
		seen[key] = &store.ChunkEntity{
			Kind: kind, ValueNorm: norm, Count: 1, Extractor: extractor,
		}
	}

	// Vault-native signals first: they are user-curated and outrank heuristics.
	for _, t := range tags {
		add("tag", t, "tag")
	}
	for _, d := range dates {
		add("date", d, "tag")
	}

	clean := stripCode(text)
	for _, m := range isoDateRe.FindAllStringSubmatch(clean, -1) {
		add("date", m[1], "heuristic")
	}
	for _, m := range writtenDate.FindAllStringSubmatch(clean, -1) {
		add("date", m[1], "heuristic")
	}
	for _, m := range pathRe.FindAllStringSubmatch(clean, -1) {
		add("path", m[1], "heuristic")
	}
	for _, m := range numberRe.FindAllStringSubmatch(clean, -1) {
		add("number", strings.TrimSpace(m[1]), "heuristic")
	}
	for _, m := range properNounRe.FindAllStringSubmatch(clean, -1) {
		v := strings.TrimSpace(m[1])
		// A leading article belongs to the sentence, not the name: "The Falkirk Wheel"
		// and "Falkirk Wheel" are the same entity, and keeping both spellings means a
		// query mentioning one cannot match a chunk carrying the other.
		v = trimLeadingArticle(v)
		if v == "" {
			continue
		}
		// Single common word capitalized only by sentence position.
		if !strings.Contains(v, " ") && commonSentenceStarters[strings.ToLower(v)] {
			continue
		}
		// A single short all-caps token is usually an acronym worth keeping, but
		// a single capitalized common word is not.
		if len(v) < 3 && v == strings.ToUpper(v) {
			continue
		}
		add("proper", v, "heuristic")
	}

	out := make([]store.ChunkEntity, 0, len(seen))
	for _, e := range seen {
		out = append(out, *e)
	}
	return out
}
