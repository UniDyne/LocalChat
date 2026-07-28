package memory

import (
	"regexp"
	"strings"
	"time"
)

// Frontmatter is a parsed YAML frontmatter block, reduced to the shapes that
// actually appear in vault notes: scalars and lists of scalars.
//
// Deliberately not a full YAML parser. skill/skill.go's reader handles only flat
// single-line fields, which is too little here (vault frontmatter routinely has
// lists), but a complete YAML implementation is far more than is needed. Nested
// maps are skipped rather than mis-parsed.
type Frontmatter struct {
	Fields map[string][]string
}

// Get returns the first value for a key, or "".
func (f Frontmatter) Get(key string) string {
	if v := f.Fields[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// List returns all values for a key.
func (f Frontmatter) List(key string) []string { return f.Fields[key] }

// ParseFrontmatter reads the supported subset:
//
//	title: Some Note
//	tags: [a, b]
//	aliases:
//	  - First Alias
//	  - Second Alias
//	date: 2026-07-28
func ParseFrontmatter(raw string) Frontmatter {
	fm := Frontmatter{Fields: map[string][]string{}}
	if strings.TrimSpace(raw) == "" {
		return fm
	}

	var currentKey string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		// Continuation of a block list: "  - value"
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			if currentKey != "" {
				if v := cleanScalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))); v != "" {
					fm.Fields[currentKey] = append(fm.Fields[currentKey], v)
				}
			}
			continue
		}

		// A nested map ("key:" followed by indented "sub: value") is skipped:
		// indented non-list lines belong to a structure we do not model.
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			continue
		}

		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		rest := strings.TrimSpace(line[idx+1:])
		currentKey = key

		switch {
		case rest == "":
			// Block list or empty value; values arrive on following lines.
			if _, ok := fm.Fields[key]; !ok {
				fm.Fields[key] = nil
			}
		case strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]"):
			for _, part := range strings.Split(rest[1:len(rest)-1], ",") {
				if v := cleanScalar(strings.TrimSpace(part)); v != "" {
					fm.Fields[key] = append(fm.Fields[key], v)
				}
			}
		default:
			if v := cleanScalar(rest); v != "" {
				fm.Fields[key] = append(fm.Fields[key], v)
			}
		}
	}
	return fm
}

func cleanScalar(s string) string {
	s = strings.TrimSpace(s)
	// Strip an inline comment, but not a '#' inside quotes (a tag value).
	if !strings.HasPrefix(s, `"`) && !strings.HasPrefix(s, `'`) {
		if i := strings.Index(s, " #"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	return strings.TrimSpace(s)
}

// ---------- wikilinks ----------

// Link is a reference from one note to another, extracted before resolution.
type Link struct {
	// Target is the raw link target as written, e.g. "Some Note" or "dir/Note".
	Target string
	// Heading is the "#section" part, if any — used to aim the edge at the chunk
	// carrying that heading rather than the note's first chunk.
	Heading string
	// Alias is the "|display text" part, if any.
	Alias string
	// Embed is true for transclusions (![[...]]).
	Embed bool
}

var (
	// [[Target#Heading|Alias]] with optional leading ! for embeds.
	wikilinkRe = regexp.MustCompile(`(!?)\[\[([^\]\[|#]+)(#[^\]\[|]+)?(\|[^\]\[]+)?\]\]`)
	// [text](target) — only local .md targets are treated as note links.
	mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
)

// ExtractLinks finds wikilinks, embeds, and local Markdown links. Code spans and
// fenced blocks must be stripped first (see stripCode) or examples in
// documentation become phantom links.
func ExtractLinks(text string) []Link {
	var out []Link
	clean := stripCode(text)

	for _, m := range wikilinkRe.FindAllStringSubmatch(clean, -1) {
		l := Link{
			Embed:  m[1] == "!",
			Target: strings.TrimSpace(m[2]),
		}
		if m[3] != "" {
			l.Heading = strings.TrimSpace(strings.TrimPrefix(m[3], "#"))
		}
		if m[4] != "" {
			l.Alias = strings.TrimSpace(strings.TrimPrefix(m[4], "|"))
		}
		if l.Target != "" {
			out = append(out, l)
		}
	}

	for _, m := range mdLinkRe.FindAllStringSubmatch(clean, -1) {
		target := strings.TrimSpace(m[2])
		if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "#") {
			continue // external URL or intra-document anchor
		}
		heading := ""
		if i := strings.Index(target, "#"); i >= 0 {
			heading = target[i+1:]
			target = target[:i]
		}
		if !strings.HasSuffix(strings.ToLower(target), ".md") &&
			!strings.HasSuffix(strings.ToLower(target), ".markdown") {
			continue
		}
		out = append(out, Link{Target: target, Heading: heading, Alias: m[1]})
	}
	return out
}

// stripCode blanks fenced blocks and inline spans so link/tag extraction does not
// pick up examples written in documentation.
func stripCode(text string) string {
	var b strings.Builder
	lines := strings.Split(text, "\n")
	inFence := false
	var fence string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			if f, ok := codeFence(trimmed); ok {
				inFence, fence = true, f
				b.WriteByte('\n')
				continue
			}
		} else {
			if closesFence(trimmed, fence) {
				inFence = false
			}
			b.WriteByte('\n')
			continue
		}
		// Blank inline code spans.
		inSpan := false
		for _, r := range line {
			if r == '`' {
				inSpan = !inSpan
				b.WriteByte(' ')
				continue
			}
			if inSpan {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ---------- tags ----------

// tagRe matches an Obsidian inline tag: # followed by a letter, then letters,
// digits, -, _, or / for nested tags. Requires a preceding boundary so that
// "C#" and URL fragments do not match.
var tagRe = regexp.MustCompile(`(^|[\s(\[{,;:])#([\p{L}][\p{L}\p{N}\-_/]*)`)

// ExtractTags finds inline #tags, excluding ATX headings and anything in code.
func ExtractTags(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(stripCode(text), "\n") {
		// An ATX heading is "# Title", not a tag — the space after # is what
		// distinguishes them, and tagRe already requires a letter, but a heading
		// like "#Title" would otherwise match. Skip heading lines outright.
		if _, _, ok := atxHeading(strings.TrimSpace(line)); ok {
			continue
		}
		for _, m := range tagRe.FindAllStringSubmatch(line, -1) {
			tag := strings.ToLower(m[2])
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	return out
}

// ---------- daily notes ----------

var dailyNoteFormats = []string{
	"2006-01-02", "2006_01_02", "20060102",
	"02-01-2006", "01-02-2006", "2006-01-02 Monday",
}

// DailyNoteDate extracts a date from a note's filename, which is what makes
// "what was I working on last week" answerable over a vault of daily notes.
// Returns the date in ISO form and whether one was found.
func DailyNoteDate(filename string) (string, bool) {
	base := filename
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".markdown"), ".md")
	base = strings.TrimSpace(base)

	for _, layout := range dailyNoteFormats {
		if len(base) < len(layout) {
			continue
		}
		// Try the whole name first, then a leading prefix, since daily notes are
		// often "2026-07-28 Meeting notes".
		for _, cand := range []string{base, base[:len(layout)]} {
			if t, err := time.Parse(layout, cand); err == nil {
				// Reject implausible years to avoid matching version strings.
				if t.Year() >= 1900 && t.Year() <= 2200 {
					return t.Format("2006-01-02"), true
				}
			}
		}
	}
	return "", false
}

// ---------- vault path resolution ----------

// LinkResolver resolves wikilink targets to source references, honoring
// Obsidian's shortest-unique-path rule and frontmatter aliases.
type LinkResolver struct {
	// byBase maps a lowercased basename (no extension) to full relative paths.
	byBase map[string][]string
	// byPath maps a lowercased relative path (no extension) to itself.
	byPath map[string]string
	// byAlias maps a lowercased alias to a relative path.
	byAlias map[string]string
}

// NewLinkResolver builds a resolver from every note path in the vault.
func NewLinkResolver() *LinkResolver {
	return &LinkResolver{
		byBase:  map[string][]string{},
		byPath:  map[string]string{},
		byAlias: map[string]string{},
	}
}

// AddNote registers a note by its vault-relative path.
func (r *LinkResolver) AddNote(relPath string, aliases []string) {
	norm := normalizeNotePath(relPath)
	r.byPath[norm] = relPath
	base := norm
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	r.byBase[base] = append(r.byBase[base], relPath)
	for _, a := range aliases {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			r.byAlias[a] = relPath
		}
	}
}

// Resolve maps a link target to a vault-relative note path. Ambiguous basenames
// (the same filename in two folders) resolve only when the link includes enough
// path to disambiguate — guessing would wire unrelated notes together.
func (r *LinkResolver) Resolve(target string) (string, bool) {
	norm := normalizeNotePath(target)
	if p, ok := r.byPath[norm]; ok {
		return p, true
	}
	if p, ok := r.byAlias[norm]; ok {
		return p, true
	}
	base := norm
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if paths := r.byBase[base]; len(paths) == 1 {
		return paths[0], true
	}
	return "", false
}

func normalizeNotePath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	low := strings.ToLower(p)
	low = strings.TrimSuffix(strings.TrimSuffix(low, ".markdown"), ".md")
	return low
}
