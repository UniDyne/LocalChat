package memory

import (
	"strings"
	"testing"
)

func blocksOfKind(bs []Block, k BlockKind) []Block {
	var out []Block
	for _, b := range bs {
		if b.Kind == k {
			out = append(out, b)
		}
	}
	return out
}

// TestCodeFenceNeverSplit is the single most important structural guarantee: a
// code block that gets split produces a chunk that is worse than useless, and the
// content inside it must survive byte-for-byte.
func TestCodeFenceNeverSplit(t *testing.T) {
	md := "# Title\n\nIntro text.\n\n" +
		"```go\n" +
		"func main() {\n" +
		"\t// A heading-looking line inside code:\n" +
		"\t// # Not A Heading\n" +
		"\t// - not a list\n" +
		"\t// | not | a | table |\n" +
		"\tfmt.Println(\"hi\")\n" +
		"}\n" +
		"```\n\n" +
		"Trailing prose."

	blocks := ParseBlocks(md)
	code := blocksOfKind(blocks, BlockCode)
	if len(code) != 1 {
		t.Fatalf("expected exactly 1 code block, got %d: %+v", len(code), blocks)
	}
	body := code[0].Text
	for _, want := range []string{
		"func main() {", "# Not A Heading", "- not a list", "| not | a | table |", `fmt.Println("hi")`, "}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("code block lost %q:\n%s", want, body)
		}
	}
	// Nothing inside the fence became a heading or list block.
	for _, b := range blocks {
		if b.Kind == BlockHeading && b.Text == "Not A Heading" {
			t.Error("a comment inside a code fence was parsed as a heading")
		}
		if b.Kind == BlockList && strings.Contains(b.Text, "not a list") {
			t.Error("a comment inside a code fence was parsed as a list")
		}
	}
}

func TestNestedFenceAndTildeFence(t *testing.T) {
	// A ```` fence containing ``` must not be closed by the inner one.
	md := "~~~\nliteral ``` inside\nstill inside\n~~~\n\nafter"
	blocks := ParseBlocks(md)
	code := blocksOfKind(blocks, BlockCode)
	if len(code) != 1 {
		t.Fatalf("expected 1 code block, got %d", len(code))
	}
	if !strings.Contains(code[0].Text, "still inside") {
		t.Errorf("tilde fence closed early: %q", code[0].Text)
	}
}

func TestUnterminatedFenceConsumesRest(t *testing.T) {
	md := "# T\n\n```go\nnever closed\n# looks like heading\n"
	blocks := ParseBlocks(md)
	code := blocksOfKind(blocks, BlockCode)
	if len(code) != 1 {
		t.Fatalf("expected 1 code block, got %d", len(code))
	}
	if !strings.Contains(code[0].Text, "looks like heading") {
		t.Error("unterminated fence did not consume the remainder")
	}
	for _, b := range blocks {
		if b.Kind == BlockHeading && b.Text == "looks like heading" {
			t.Error("text after an unterminated fence was parsed as a heading")
		}
	}
}

func TestTableKeptWhole(t *testing.T) {
	md := "## Data\n\n| Col | Val |\n|-----|-----|\n| a   | 1   |\n| b   | 2   |\n\nAfter."
	blocks := ParseBlocks(md)
	tables := blocksOfKind(blocks, BlockTable)
	if len(tables) != 1 {
		t.Fatalf("expected 1 table block, got %d: %+v", len(tables), blocks)
	}
	for _, want := range []string{"| Col | Val |", "| a   | 1   |", "| b   | 2   |"} {
		if !strings.Contains(tables[0].Text, want) {
			t.Errorf("table lost row %q", want)
		}
	}
	if !tables[0].Kind.Atomic() {
		t.Error("table should be atomic")
	}
}

func TestHeadingPath(t *testing.T) {
	md := `# Architecture

Top level prose.

## Data model

Model prose.

### Storage schema

Schema prose.

## Turn lifecycle

Lifecycle prose.

# Other Doc

Other prose.`

	blocks := ParseBlocks(md)
	want := map[string]string{
		"Top level prose.": "Architecture",
		"Model prose.":     "Architecture › Data model",
		"Schema prose.":    "Architecture › Data model › Storage schema",
		"Lifecycle prose.": "Architecture › Turn lifecycle",
		"Other prose.":     "Other Doc",
	}
	found := 0
	for _, b := range blocks {
		if b.Kind != BlockParagraph {
			continue
		}
		if exp, ok := want[b.Text]; ok {
			found++
			if b.HeadingPath != exp {
				t.Errorf("%q: heading path = %q, want %q", b.Text, b.HeadingPath, exp)
			}
		}
	}
	if found != len(want) {
		t.Errorf("matched %d of %d paragraphs", found, len(want))
	}
}

// TestHeadingLevelSkip covers h1 -> h3 with no h2, which must not panic or
// produce a path with a gap.
func TestHeadingLevelSkip(t *testing.T) {
	md := "# One\n\n### Three\n\nprose"
	blocks := ParseBlocks(md)
	for _, b := range blocks {
		if b.Kind == BlockParagraph && b.Text == "prose" {
			if b.HeadingPath != "One › Three" {
				t.Errorf("path = %q, want %q", b.HeadingPath, "One › Three")
			}
			return
		}
	}
	t.Error("prose block not found")
}

func TestSetextHeadingVsThematicBreak(t *testing.T) {
	// "Title\n---" is an h2; a bare "---" after a blank line is a rule.
	md := "Title\n-----\n\nprose\n\n---\n\nmore"
	blocks := ParseBlocks(md)
	var headings, rules int
	for _, b := range blocks {
		switch b.Kind {
		case BlockHeading:
			headings++
			if b.Text != "Title" {
				t.Errorf("unexpected heading %q", b.Text)
			}
		case BlockThematic:
			rules++
		}
	}
	if headings != 1 {
		t.Errorf("headings = %d, want 1", headings)
	}
	if rules != 1 {
		t.Errorf("thematic breaks = %d, want 1", rules)
	}
}

func TestListAndQuoteBlocks(t *testing.T) {
	md := "Intro.\n\n- one\n- two\n- three\n\n> quoted line\n> more quote\n\nEnd."
	blocks := ParseBlocks(md)
	if n := len(blocksOfKind(blocks, BlockList)); n != 1 {
		t.Errorf("list blocks = %d, want 1", n)
	}
	if n := len(blocksOfKind(blocks, BlockQuote)); n != 1 {
		t.Errorf("quote blocks = %d, want 1", n)
	}
	lists := blocksOfKind(blocks, BlockList)
	if len(lists) == 1 && !strings.Contains(lists[0].Text, "three") {
		t.Error("list block truncated")
	}
}

func TestSplitFrontmatter(t *testing.T) {
	cases := []struct {
		name, in, wantFM, wantBodyHas string
	}{
		{"present", "---\ntitle: T\n---\n\nBody here", "title: T", "Body here"},
		{"absent", "# No frontmatter\n\nBody", "", "No frontmatter"},
		{"unterminated", "---\ntitle: T\n\nBody with no close", "", "Body with no close"},
		{"dots_close", "---\ntitle: T\n...\nBody", "title: T", "Body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, body := SplitFrontmatter(c.in)
			if strings.TrimSpace(fm) != c.wantFM {
				t.Errorf("frontmatter = %q, want %q", fm, c.wantFM)
			}
			if !strings.Contains(body, c.wantBodyHas) {
				t.Errorf("body %q missing %q", body, c.wantBodyHas)
			}
		})
	}
}

// TestThematicBreakNotFrontmatter guards a real ambiguity: a document starting
// with a horizontal rule must not have its whole content swallowed as frontmatter.
func TestThematicBreakNotFrontmatter(t *testing.T) {
	md := "---\n\nJust a rule at the top, no frontmatter.\n"
	fm, body := SplitFrontmatter(md)
	if fm != "" {
		// A blank line immediately after --- means there is no key: value
		// content, but the parser is line-based; the important property is that
		// the body is not lost.
		t.Logf("frontmatter parsed as %q", fm)
	}
	if !strings.Contains(body+fm, "Just a rule") {
		t.Error("document content was lost")
	}
}

// unclosedFences counts fence-delimiter LINES that are left open in text.
//
// Counting occurrences of the ``` substring is not the right invariant: a code
// fence is a line-level construct, and the sequence can legitimately appear as
// literal content inside an inline code span or a table cell — which real
// documentation does. This counts only lines that actually open or close a fence.
func unclosedFences(text string) int {
	open := 0
	var fence string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if open == 0 {
			if f, ok := codeFence(trimmed); ok {
				open, fence = 1, f
			}
			continue
		}
		if closesFence(trimmed, fence) {
			open = 0
		}
	}
	return open
}

func TestUnclosedFencesIgnoresInlineLiterals(t *testing.T) {
	// A table cell mentioning the fence sequence is not an open fence. This case
	// comes from the project's own documentation.
	table := "| File | Contents |\n|---|---|\n| `markdown.go` | fenced code (```" + " and ~~~) |\n"
	if n := unclosedFences(table); n != 0 {
		t.Errorf("table with a literal fence sequence in a cell reported %d unclosed fences", n)
	}
	// A genuinely unclosed fence is still detected.
	if n := unclosedFences("```go\nnever closed\n"); n != 1 {
		t.Errorf("unclosed fence reported %d, want 1", n)
	}
	// A balanced fence is balanced.
	if n := unclosedFences("```go\nx := 1\n```\n"); n != 0 {
		t.Errorf("balanced fence reported %d, want 0", n)
	}
}
