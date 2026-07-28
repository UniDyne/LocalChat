package memory

import (
	"strings"
	"testing"
)

func TestChunkHeadingsCarriesPathAndSplitsOnHeadings(t *testing.T) {
	tc := NewHeuristicCounter()
	md := `# Doc

Intro paragraph that is long enough to survive the small-chunk merge threshold on its own, ` +
		strings.Repeat("with additional filler text to push the token count up. ", 6) + `

## Section A

Section A prose, also padded out so it is not merged away. ` +
		strings.Repeat("more filler words here to add tokens. ", 6) + `

## Section B

Section B prose, likewise padded to stand alone as its own chunk. ` +
		strings.Repeat("further filler content for length. ", 6)

	chunks := ChunkHeadings(ParseBlocks(md), tc)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}

	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.HeadingPath] = true
		if c.TokenCount > MaxChunkTokens {
			t.Errorf("chunk exceeds MaxChunkTokens (%d > %d): %q", c.TokenCount, MaxChunkTokens, truncStr(c.Text, 60))
		}
	}
	for _, want := range []string{"Doc", "Doc › Section A", "Doc › Section B"} {
		if !paths[want] {
			t.Errorf("no chunk with heading path %q (got %v)", want, keys(paths))
		}
	}
}

// TestChunkNeverSplitsCodeOrTable is the guarantee that carries through from the
// block parser into the chunker.
func TestChunkNeverSplitsCodeOrTable(t *testing.T) {
	tc := NewHeuristicCounter()
	code := "```go\n" + strings.Repeat("// a line of code that is fairly long to push the token count\n", 40) + "```"
	md := "# Doc\n\nIntro.\n\n" + code + "\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"

	chunks := ChunkHeadings(ParseBlocks(md), tc)

	// The fence must appear intact in exactly one chunk: opening and closing
	// markers together, and no chunk containing only part of it.
	full := 0
	for _, c := range chunks {
		opens := strings.Count(c.Text, "```")
		if opens == 0 {
			continue
		}
		if opens != 2 {
			t.Errorf("a chunk contains an unbalanced code fence (%d markers): %q", opens, truncStr(c.Text, 80))
		} else {
			full++
		}
	}
	if full != 1 {
		t.Errorf("code fence appears whole in %d chunks, want 1", full)
	}

	// The table's rows must not be spread across chunks.
	tableChunks := 0
	for _, c := range chunks {
		if strings.Contains(c.Text, "| a | b |") {
			tableChunks++
			if !strings.Contains(c.Text, "| 1 | 2 |") {
				t.Error("table split: header and body landed in different chunks")
			}
		}
	}
	if tableChunks != 1 {
		t.Errorf("table appears in %d chunks, want 1", tableChunks)
	}
}

func TestChunkOversizedProseSplitsOnSentences(t *testing.T) {
	tc := NewHeuristicCounter()
	// One paragraph well over the budget, made of clean sentences.
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		sb.WriteString("This is sentence number ")
		sb.WriteString(strings.Repeat("x", 3))
		sb.WriteString(" with enough words to matter. ")
	}
	md := "# Doc\n\n" + sb.String()

	chunks := ChunkHeadings(ParseBlocks(md), tc)
	if len(chunks) < 2 {
		t.Fatalf("oversized paragraph was not split: %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if c.TokenCount > MaxChunkTokens {
			t.Errorf("chunk over budget: %d > %d", c.TokenCount, MaxChunkTokens)
		}
		// Cuts landed on sentence boundaries, so no chunk starts mid-sentence
		// with a lowercase fragment.
		trimmed := strings.TrimSpace(c.Text)
		if trimmed != "" && strings.HasPrefix(trimmed, "with enough") {
			t.Errorf("chunk starts mid-sentence: %q", truncStr(trimmed, 60))
		}
	}
}

func TestChunkMergesSmallNeighbours(t *testing.T) {
	tc := NewHeuristicCounter()
	// Several tiny sections under one heading path.
	md := "# Doc\n\ntiny one.\n\ntiny two.\n\ntiny three.\n"
	chunks := ChunkHeadings(ParseBlocks(md), tc)
	if len(chunks) != 1 {
		t.Errorf("expected tiny blocks to merge into 1 chunk, got %d: %+v", len(chunks), chunks)
	}
}

func TestChunkHeadingOnlyProducesNothing(t *testing.T) {
	tc := NewHeuristicCounter()
	chunks := ChunkHeadings(ParseBlocks("# Just A Heading\n\n## And Another\n"), tc)
	if len(chunks) != 0 {
		t.Errorf("headings with no content should produce no chunks, got %d: %+v", len(chunks), chunks)
	}
}

// TestBuildEmbeddedTextRespectsWindow is the §3.2 invariant: heading path, thread
// context and CoT all prepend to the embedded text, and their combined length
// must never exceed the model's hard 512-token window. ONNX Runtime truncates
// silently, so nothing else would catch a violation.
func TestBuildEmbeddedTextRespectsWindow(t *testing.T) {
	tc := NewHeuristicCounter()

	deepHeading := "Architecture › Data model › Storage schema › Deletion semantics › Cascade ordering › Even deeper"
	threadCtx := strings.Repeat("Session about cascade ordering in the storage layer. ", 20)
	longCoT := strings.Repeat("The user is asking about cascade ordering and the answer involves memory rows. ", 40)
	body := strings.Repeat("Deleting a session removes its derived memory rows. ", 200)

	out := BuildEmbeddedText(deepHeading, threadCtx, longCoT, body, tc)
	if n := tc.Count(out); n > 512 {
		t.Errorf("embedded text is %d tokens, exceeds the 512 window", n)
	}

	// The heading path is highest priority and must survive.
	if !strings.Contains(out, "Architecture") {
		t.Error("heading path was dropped from the prefix")
	}
	// Content must be present — prefix is clipped first, never the body.
	if !strings.Contains(out, "Deleting a session") {
		t.Error("body content was sacrificed to prefix")
	}
	t.Logf("embedded text = %d tokens", tc.Count(out))
}

func TestBuildEmbeddedTextClipsPrefixNotBody(t *testing.T) {
	tc := NewHeuristicCounter()
	body := "The important content that must be preserved in full because it is what retrieval returns."
	hugePrefix := strings.Repeat("prefix noise ", 500)

	out := BuildEmbeddedText(hugePrefix, "", "", body, tc)
	if !strings.Contains(out, "important content that must be preserved") {
		t.Error("body was clipped in favour of prefix — priority is inverted")
	}
	if n := tc.Count(out); n > 512 {
		t.Errorf("total = %d tokens, want <= 512", n)
	}
	// The prefix got its budget, not more.
	prefixPart := strings.TrimSuffix(out, body)
	if n := tc.Count(prefixPart); n > PrefixBudget+2 {
		t.Errorf("prefix used %d tokens, budget is %d", n, PrefixBudget)
	}
}

func TestBuildEmbeddedTextCoTIsCapped(t *testing.T) {
	tc := NewHeuristicCounter()
	cot := strings.Repeat("reasoning trace token ", 200)
	out := BuildEmbeddedText("Heading", "", cot, "body text", tc)

	// CoT must not consume the whole prefix budget.
	before, _, found := strings.Cut(out, "body text")
	if !found {
		t.Fatal("body missing")
	}
	if n := tc.Count(before); n > PrefixBudget+2 {
		t.Errorf("prefix (incl. CoT) = %d tokens, budget %d", n, PrefixBudget)
	}
	if !strings.Contains(out, "Heading") {
		t.Error("heading path lost to CoT — CoT should be capped so it cannot starve the others")
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
