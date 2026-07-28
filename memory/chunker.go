package memory

import (
	"strings"
)

// Token budget. The model's window is a hard 512 tokens and ONNX Runtime
// truncates silently, so the budget is split rather than spent twice: three
// separate mechanisms prepend context to the embedded text (heading_path, the
// conversation thread-context prefix, and CoT), and their combined length must
// never push past the window.
//
// Content is never sacrificed to make room for prefix — the prefix exists only to
// situate the content, so it gets clipped first.
const (
	// PrefixBudget is reserved for context prepended to the embedded text.
	PrefixBudget = 96
	// MaxChunkTokens is the ceiling on a chunk's own text: 512 - PrefixBudget.
	MaxChunkTokens = 512 - PrefixBudget
	// MinChunkTokens is the floor below which a chunk is merged into a neighbour.
	MinChunkTokens = 64
	// CoTSubCap limits how much of a CoT row may enter the prefix. Its value is
	// the topical restatement it opens with, not the full reasoning trace, and
	// letting it consume the budget would starve heading_path and thread context.
	CoTSubCap = 48
)

// Chunk is a chunker output: the text to store plus the context to embed with it.
type Chunk struct {
	// Text is what gets stored and returned to the model.
	Text string
	// HeadingPath situates the chunk in its document.
	HeadingPath string
	// TokenCount is the token length of Text alone.
	TokenCount int
}

// ChunkHeadings splits blocks into chunks along heading boundaries, packing
// consecutive blocks under the same heading until the budget is reached.
//
// This is the Phase 2 baseline chunker. Headings are boundaries the author chose
// deliberately, which makes this a genuinely strong baseline on structured notes
// and the thing Leiden has to beat in Phase 5.
//
// Atomic blocks (code fences, tables) are never split internally. An atomic block
// that alone exceeds the budget is emitted oversized rather than cut: a truncated
// code block is worse than a large one, and the embedder will clip the tail while
// the stored text stays intact and useful when returned.
func ChunkHeadings(blocks []Block, tc TokenCounter) []Chunk {
	var out []Chunk

	var cur []Block
	curTokens := 0
	curPath := ""

	flush := func() {
		if len(cur) == 0 {
			return
		}
		if c, ok := blocksToChunk(cur, curPath, tc); ok {
			out = append(out, c)
		}
		cur = nil
		curTokens = 0
	}

	for _, b := range blocks {
		if b.Kind == BlockThematic {
			continue // a horizontal rule carries no content
		}

		// A heading starts a new chunk and leads it, so the chunk's text says
		// what it is about even without the path metadata.
		if b.Kind == BlockHeading {
			flush()
			curPath = b.HeadingPath
			cur = append(cur, b)
			curTokens = tc.Count(b.Text)
			continue
		}

		if curPath == "" {
			curPath = b.HeadingPath
		}

		n := tc.Count(b.Text)

		// Oversized non-atomic block: split it on sentence boundaries.
		if n > MaxChunkTokens && !b.Kind.Atomic() {
			flush()
			curPath = b.HeadingPath
			for _, part := range splitBlockBySentences(b, tc) {
				out = append(out, part)
			}
			continue
		}

		if curTokens+n > MaxChunkTokens && curTokens > 0 {
			flush()
			curPath = b.HeadingPath
		}
		cur = append(cur, b)
		curTokens += n
	}
	flush()

	return mergeSmallChunks(out, tc)
}

func blocksToChunk(blocks []Block, path string, tc TokenCounter) (Chunk, bool) {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Kind == BlockHeading {
			// Re-add the marker so the stored text reads as Markdown.
			parts = append(parts, strings.Repeat("#", b.Level)+" "+b.Text)
			continue
		}
		parts = append(parts, b.Text)
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return Chunk{}, false
	}
	// A chunk containing only its heading carries no information.
	if len(blocks) == 1 && blocks[0].Kind == BlockHeading {
		return Chunk{}, false
	}
	return Chunk{Text: text, HeadingPath: path, TokenCount: tc.Count(text)}, true
}

// splitBlockBySentences divides an over-long prose block at sentence boundaries,
// which is the least-bad place to cut when a cut is unavoidable.
func splitBlockBySentences(b Block, tc TokenCounter) []Chunk {
	sentences := SplitSentences(b.Text)
	if len(sentences) <= 1 {
		// Nothing to split on — emit as-is rather than cutting mid-sentence.
		return []Chunk{{Text: b.Text, HeadingPath: b.HeadingPath, TokenCount: tc.Count(b.Text)}}
	}

	var out []Chunk
	var cur []string
	curTokens := 0
	for _, s := range sentences {
		n := tc.Count(s)
		if curTokens+n > MaxChunkTokens && curTokens > 0 {
			text := strings.Join(cur, " ")
			out = append(out, Chunk{Text: text, HeadingPath: b.HeadingPath, TokenCount: tc.Count(text)})
			cur, curTokens = nil, 0
		}
		cur = append(cur, s)
		curTokens += n
	}
	if len(cur) > 0 {
		text := strings.Join(cur, " ")
		out = append(out, Chunk{Text: text, HeadingPath: b.HeadingPath, TokenCount: tc.Count(text)})
	}
	return out
}

// mergeSmallChunks folds chunks below MinChunkTokens into an adjacent chunk under
// the same heading. A stub chunk ("## Notes" plus one line) retrieves poorly on
// its own and dilutes per-source diversity.
func mergeSmallChunks(in []Chunk, tc TokenCounter) []Chunk {
	if len(in) <= 1 {
		return in
	}
	var out []Chunk
	for _, c := range in {
		if len(out) == 0 {
			out = append(out, c)
			continue
		}
		prev := &out[len(out)-1]
		mergeable := c.TokenCount < MinChunkTokens &&
			prev.HeadingPath == c.HeadingPath &&
			prev.TokenCount+c.TokenCount <= MaxChunkTokens
		if mergeable {
			prev.Text = prev.Text + "\n\n" + c.Text
			prev.TokenCount = tc.Count(prev.Text)
			continue
		}
		out = append(out, c)
	}
	return out
}

// BuildEmbeddedText assembles the text that gets embedded, as distinct from the
// text that gets stored. Enforces the split budget: the prefix is clipped to
// PrefixBudget (heading path first, then thread context, then CoT within
// CoTSubCap) and the body to MaxChunkTokens, so the total cannot exceed 512.
//
// cot is included here but excluded from the stored text — see §3.3: indexing it
// makes terse turns findable, while returning it would hand the model its own past
// speculation as retrieved record, which ARCHITECTURE.md deliberately avoids.
func BuildEmbeddedText(headingPath, threadContext, cot, body string, tc TokenCounter) string {
	var parts []string
	remaining := PrefixBudget

	appendPrefix := func(s string, cap int) {
		if s == "" || remaining <= 0 {
			return
		}
		if cap > remaining {
			cap = remaining
		}
		clipped := tc.Truncate(strings.TrimSpace(s), cap)
		if clipped == "" {
			return
		}
		parts = append(parts, clipped)
		remaining -= tc.Count(clipped)
	}

	appendPrefix(headingPath, remaining)
	appendPrefix(threadContext, remaining)
	appendPrefix(cot, CoTSubCap)

	parts = append(parts, tc.Truncate(strings.TrimSpace(body), MaxChunkTokens))
	return strings.Join(parts, "\n")
}
