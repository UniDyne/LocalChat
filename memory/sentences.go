package memory

import (
	"strings"
	"unicode"
)

// abbreviations that end in a period and do not end a sentence. Lowercased for
// comparison. Kept deliberately small: a long list mostly adds false negatives,
// and the surrounding heuristics (next-character checks) catch most cases.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sr": true, "jr": true,
	"st": true, "vs": true, "etc": true, "eg": true, "ie": true, "cf": true, "al": true,
	"fig": true, "no": true, "inc": true, "ltd": true, "co": true, "corp": true,
	"approx": true, "min": true, "max": true, "avg": true, "vol": true, "ch": true,
	"sec": true, "ref": true, "e.g": true, "i.e": true,
}

// SplitSentences splits prose into sentences.
//
// Hand-rolled rather than dependency-backed, matching the repo's preference for
// small purpose-built parsers. The cases that actually matter for this corpus:
// abbreviations, version numbers and decimals (1.4.1, $19.99), ellipses, inline
// code spans (which must pass through opaque), URLs and file paths, and closing
// punctuation after a quote or bracket.
func SplitSentences(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	runes := []rune(text)
	var out []string
	start := 0
	inCode := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// Inline code spans are opaque: a period inside `fmt.Sprintf` is not a
		// sentence boundary.
		if r == '`' {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}

		if r != '.' && r != '!' && r != '?' {
			continue
		}

		// Consume a run of terminators plus any closing quotes/brackets so
		// `He said "stop!"` breaks after the quote, not before it.
		end := i
		for end+1 < len(runes) && isTerminator(runes[end+1]) {
			end++
		}
		for end+1 < len(runes) && isCloser(runes[end+1]) {
			end++
		}

		if !endsSentence(runes, i, end) {
			i = end
			continue
		}

		out = appendSentence(out, string(runes[start:end+1]))
		// Skip the whitespace that follows.
		j := end + 1
		for j < len(runes) && unicode.IsSpace(runes[j]) {
			j++
		}
		start = j
		i = j - 1
	}

	if start < len(runes) {
		out = appendSentence(out, string(runes[start:]))
	}
	return out
}

func appendSentence(out []string, s string) []string {
	if t := strings.TrimSpace(s); t != "" {
		return append(out, t)
	}
	return out
}

func isTerminator(r rune) bool { return r == '.' || r == '!' || r == '?' }
func isCloser(r rune) bool {
	switch r {
	case '"', '\'', ')', ']', '}', '”', '’', '»':
		return true
	}
	return false
}

// endsSentence decides whether the terminator run ending at `end` closes a
// sentence. `i` is the first terminator in the run.
func endsSentence(runes []rune, i, end int) bool {
	// Must be followed by whitespace (or end of text).
	if end+1 < len(runes) && !unicode.IsSpace(runes[end+1]) {
		return false
	}

	// A single '.' has the ambiguous cases; '!' and '?' are almost always real.
	if runes[i] == '.' && end == i {
		// Decimal or version number: digit '.' digit.
		if i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]) {
			return false
		}
		// Single initial: "J. Smith" — one letter preceded by a boundary.
		if i > 0 && unicode.IsLetter(runes[i-1]) && (i == 1 || !unicode.IsLetter(runes[i-2])) {
			return false
		}
		// Known abbreviation.
		if isAbbreviationBefore(runes, i) {
			return false
		}
	}

	// Ellipsis mid-sentence: "wait ... then" continues if the next word is
	// lowercase.
	if end > i && runes[i] == '.' {
		if next, ok := nextWordFirstRune(runes, end+1); ok && unicode.IsLower(next) {
			return false
		}
	}

	// The next word should look like a sentence start: upper case, a digit, or
	// Markdown emphasis/link punctuation. A lowercase continuation usually means
	// we misjudged the terminator.
	if next, ok := nextWordFirstRune(runes, end+1); ok {
		if unicode.IsLower(next) {
			return false
		}
	}
	return true
}

func isAbbreviationBefore(runes []rune, dot int) bool {
	j := dot - 1
	for j >= 0 && (unicode.IsLetter(runes[j]) || runes[j] == '.') {
		j--
	}
	word := strings.ToLower(strings.Trim(string(runes[j+1:dot]), "."))
	return abbreviations[word]
}

func nextWordFirstRune(runes []rune, from int) (rune, bool) {
	for i := from; i < len(runes); i++ {
		if unicode.IsSpace(runes[i]) {
			continue
		}
		return runes[i], true
	}
	return 0, false
}
