package memory

import (
	"strings"
	"testing"
)

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"basic",
			"First sentence. Second sentence! Third one?",
			[]string{"First sentence.", "Second sentence!", "Third one?"},
		},
		{
			"abbreviation",
			"See Dr. Smith about it. Then leave.",
			[]string{"See Dr. Smith about it.", "Then leave."},
		},
		{
			"version_number",
			"We target DuckDB 1.4.1 for now. It is the LTS line.",
			[]string{"We target DuckDB 1.4.1 for now.", "It is the LTS line."},
		},
		{
			"currency",
			"It costs $19.99 today. Tomorrow is different.",
			[]string{"It costs $19.99 today.", "Tomorrow is different."},
		},
		{
			"code_span",
			"Call `fmt.Sprintf` first. Then check the result.",
			[]string{"Call `fmt.Sprintf` first.", "Then check the result."},
		},
		{
			"initials",
			"J. R. Tolkien wrote it. Many read it.",
			[]string{"J. R. Tolkien wrote it.", "Many read it."},
		},
		{
			"quote_closer",
			`He said "stop!" Then he left.`,
			[]string{`He said "stop!"`, "Then he left."},
		},
		{
			"ellipsis_lowercase",
			"Wait ... then continue reading. Done.",
			[]string{"Wait ... then continue reading.", "Done."},
		},
		{
			"eg_abbrev",
			"Use a cheap guard, e.g. a hash check. It works.",
			[]string{"Use a cheap guard, e.g. a hash check.", "It works."},
		},
		{
			"single",
			"Only one sentence here",
			[]string{"Only one sentence here"},
		},
		{"empty", "   ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitSentences(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %d sentences %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("sentence %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseFrontmatter(t *testing.T) {
	raw := `title: My Note
tags: [alpha, beta]
aliases:
  - First Alias
  - Second Alias
date: 2026-07-28
empty:
quoted: "a: colon inside"
commented: value # trailing comment
nested:
  sub: ignored`

	fm := ParseFrontmatter(raw)
	if got := fm.Get("title"); got != "My Note" {
		t.Errorf("title = %q", got)
	}
	if got := fm.List("tags"); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("tags = %v", got)
	}
	if got := fm.List("aliases"); len(got) != 2 || got[0] != "First Alias" || got[1] != "Second Alias" {
		t.Errorf("aliases = %v", got)
	}
	if got := fm.Get("date"); got != "2026-07-28" {
		t.Errorf("date = %q", got)
	}
	if got := fm.Get("quoted"); got != "a: colon inside" {
		t.Errorf("quoted = %q", got)
	}
	if got := fm.Get("commented"); got != "value" {
		t.Errorf("comment not stripped: %q", got)
	}
	if got := fm.Get("empty"); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestExtractLinks(t *testing.T) {
	body := "See [[Some Note]] and [[dir/Other#Section]] plus [[Third|display text]].\n" +
		"An embed: ![[Attachment Note]]\n" +
		"A markdown link: [text](other-note.md) and [ext](https://example.com).\n" +
		"In code it must not count: `[[Not A Link]]`\n" +
		"```\n[[Also Not A Link]]\n```\n"

	links := ExtractLinks(body)
	byTarget := map[string]Link{}
	for _, l := range links {
		byTarget[l.Target] = l
	}

	for _, want := range []string{"Some Note", "dir/Other", "Third", "Attachment Note", "other-note.md"} {
		if _, ok := byTarget[want]; !ok {
			t.Errorf("missing link to %q (got %+v)", want, links)
		}
	}
	for _, unwanted := range []string{"Not A Link", "Also Not A Link"} {
		if _, ok := byTarget[unwanted]; ok {
			t.Errorf("link %q was extracted from code", unwanted)
		}
	}
	if l := byTarget["dir/Other"]; l.Heading != "Section" {
		t.Errorf("heading = %q, want Section", l.Heading)
	}
	if l := byTarget["Third"]; l.Alias != "display text" {
		t.Errorf("alias = %q", l.Alias)
	}
	if l := byTarget["Attachment Note"]; !l.Embed {
		t.Error("embed flag not set for ![[...]]")
	}
	if _, ok := byTarget["https://example.com"]; ok {
		t.Error("external URL treated as a note link")
	}
}

func TestExtractTags(t *testing.T) {
	body := "# Heading Is Not A Tag\n\n" +
		"Tagged with #project and #area/subarea and #with-dash.\n" +
		"Not a tag: C# or #1 or in code `#codetag`.\n" +
		"```\n#fencedtag\n```\n" +
		"Duplicate #project again."

	tags := ExtractTags(body)
	set := map[string]bool{}
	for _, tg := range tags {
		set[tg] = true
	}
	for _, want := range []string{"project", "area/subarea", "with-dash"} {
		if !set[want] {
			t.Errorf("missing tag %q (got %v)", want, tags)
		}
	}
	for _, unwanted := range []string{"codetag", "fencedtag", "1", "heading"} {
		if set[unwanted] {
			t.Errorf("unwanted tag %q extracted (got %v)", unwanted, tags)
		}
	}
	// Duplicates collapse.
	count := 0
	for _, tg := range tags {
		if tg == "project" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("tag 'project' appeared %d times, want 1", count)
	}
}

func TestDailyNoteDate(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2026-07-28.md", "2026-07-28", true},
		{"daily/2026-07-28.md", "2026-07-28", true},
		{"2026-07-28 Meeting notes.md", "2026-07-28", true},
		{"20260728.md", "2026-07-28", true},
		{"Some Note.md", "", false},
		{"v1.4.1 release.md", "", false},
	}
	for _, c := range cases {
		got, ok := DailyNoteDate(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("DailyNoteDate(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLinkResolver(t *testing.T) {
	r := NewLinkResolver()
	r.AddNote("Architecture.md", nil)
	r.AddNote("projects/Memory Plan.md", []string{"The Plan"})
	r.AddNote("a/Duplicate.md", nil)
	r.AddNote("b/Duplicate.md", nil)

	cases := []struct {
		target string
		want   string
		ok     bool
	}{
		{"Architecture", "Architecture.md", true},
		{"architecture", "Architecture.md", true},                 // case-insensitive
		{"Memory Plan", "projects/Memory Plan.md", true},          // shortest unique basename
		{"projects/Memory Plan", "projects/Memory Plan.md", true}, // full path
		{"The Plan", "projects/Memory Plan.md", true},             // alias
		{"Duplicate", "", false},                                  // ambiguous: must not guess
		{"a/Duplicate", "a/Duplicate.md", true},                   // disambiguated by path
		{"Nonexistent", "", false},
	}
	for _, c := range cases {
		got, ok := r.Resolve(c.target)
		if ok != c.ok || got != c.want {
			t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", c.target, got, ok, c.want, c.ok)
		}
	}
}

func TestExtractTerms(t *testing.T) {
	terms := ExtractTerms("The DuckDB store lives in tools_plan.go and store/store.go.")
	for _, want := range []string{"the", "duckdb", "store", "tools_plan.go", "tools", "plan", "go"} {
		if terms[want] == 0 {
			t.Errorf("missing term %q (got %v)", want, terms)
		}
	}
	// "store" appears as a word and inside store/store.go, so its count is > 1.
	if terms["store"] < 2 {
		t.Errorf("term 'store' count = %d, want >= 2", terms["store"])
	}
}

func TestExtractEntities(t *testing.T) {
	text := "Florida Hardware shipped on 2026-07-28. See conf/skills/foo.md for $19.99 pricing. " +
		"The system uses DuckDB."
	ents := ExtractEntities(text, []string{"project"}, []string{"2026-01-01"})

	byKind := map[string][]string{}
	for _, e := range ents {
		byKind[e.Kind] = append(byKind[e.Kind], e.ValueNorm)
	}

	if !containsStr(byKind["tag"], "project") {
		t.Errorf("tag entity missing: %v", byKind)
	}
	if !containsStr(byKind["date"], "2026-07-28") || !containsStr(byKind["date"], "2026-01-01") {
		t.Errorf("date entities missing: %v", byKind["date"])
	}
	if !containsStr(byKind["path"], "conf/skills/foo.md") {
		t.Errorf("path entity missing: %v", byKind["path"])
	}
	if !containsStr(byKind["proper"], "florida hardware") {
		t.Errorf("proper noun missing: %v", byKind["proper"])
	}
	// "The" begins a sentence and must not become an entity.
	if containsStr(byKind["proper"], "the") {
		t.Error("sentence-initial 'The' extracted as a proper noun")
	}
	// Extractor provenance is recorded so tiers can be weighted separately.
	for _, e := range ents {
		if e.Extractor == "" {
			t.Errorf("entity %q has no extractor", e.ValueNorm)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func TestDiceSimilarity(t *testing.T) {
	a := "The memory system ingests conversations and artifacts from directories."
	cases := []struct {
		name   string
		b      string
		minSim float64
		maxSim float64
	}{
		{"identical", a, 0.999, 1.0},
		{"whitespace_only_change", "The  memory   system ingests conversations and artifacts from directories.", 0.99, 1.0},
		{"case_only_change", strings.ToUpper(a), 0.99, 1.0},
		{"small_edit", "The memory system ingests conversations and artifacts from directories!", 0.95, 1.0},
		{"different", "Leiden community detection groups sentences into topical clusters.", 0.0, 0.35},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DiceSimilarity(a, c.b)
			if got < c.minSim || got > c.maxSim {
				t.Errorf("Dice = %.4f, want in [%.3f, %.3f]", got, c.minSim, c.maxSim)
			}
		})
	}
	if DiceSimilarity("", "") != 1 {
		t.Error("two empty strings should be identical")
	}
	if DiceSimilarity("abc", "") != 0 {
		t.Error("empty vs non-empty should be 0")
	}
}

func TestHeuristicTokenCounterIsConservative(t *testing.T) {
	tc := NewHeuristicCounter()
	// The heuristic must not *under*-count, or chunks built with it would
	// overflow the real model's window.
	for _, s := range []string{
		"hello world",
		"antidisestablishmentarianism",
		"tools_plan.go:42",
		"a b c d e f g h",
	} {
		if got := tc.Count(s); got < 1 {
			t.Errorf("Count(%q) = %d", s, got)
		}
	}
	if tc.Count("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
	// Truncate respects the budget.
	long := strings.Repeat("word ", 200)
	out := tc.Truncate(long, 10)
	if n := tc.Count(out); n > 10 {
		t.Errorf("Truncate produced %d tokens, want <= 10", n)
	}
}
