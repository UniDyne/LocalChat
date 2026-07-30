package memory

import (
	"strings"
)

// BlockKind classifies a Markdown block. The distinction that matters is
// "atomic" vs not: an atomic block is never split internally, no matter what the
// chunk budget says, because splitting a code fence or a table mid-way produces
// a chunk that is worse than useless.
type BlockKind string

const (
	BlockHeading   BlockKind = "heading"
	BlockParagraph BlockKind = "paragraph"
	BlockCode      BlockKind = "code"
	BlockTable     BlockKind = "table"
	BlockList      BlockKind = "list"
	BlockQuote     BlockKind = "quote"
	BlockThematic  BlockKind = "thematic"
)

// Atomic reports whether a block must never be split internally.
func (k BlockKind) Atomic() bool {
	switch k {
	case BlockCode, BlockTable:
		return true
	}
	return false
}

// Block is one structural unit of a Markdown document.
type Block struct {
	Kind BlockKind
	Text string
	// HeadingPath is the trail of enclosing headings, e.g.
	// "Architecture › Data model". Empty above the first heading.
	HeadingPath string
	// Level is the heading level for BlockHeading, 0 otherwise.
	Level int
	// StartLine is 1-based, for diagnostics.
	StartLine int
}

// headingSep joins heading path segments. A visually distinct separator avoids
// colliding with punctuation that appears inside headings.
const headingSep = " › "

// ParseBlocks splits Markdown into blocks, tracking the heading path.
//
// Deliberately not a full CommonMark parser: this needs to identify boundaries
// and never corrupt code, which is a much smaller problem than rendering. The
// repo already prefers small purpose-built parsers over heavy dependencies (see
// skill/skill.go's frontmatter reader).
//
// Frontmatter, if present, is not returned as a block — call SplitFrontmatter
// first if you want it.
func ParseBlocks(md string) []Block {
	lines := strings.Split(md, "\n")
	var blocks []Block
	var path []string

	// para accumulates consecutive lines of a non-atomic block.
	var para []string
	paraKind := BlockParagraph
	paraStart := 0

	flush := func() {
		if len(para) == 0 {
			return
		}
		text := strings.TrimRight(strings.Join(para, "\n"), " \t\n")
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, Block{
				Kind:        paraKind,
				Text:        text,
				HeadingPath: strings.Join(path, headingSep),
				StartLine:   paraStart,
			})
		}
		para = nil
		paraKind = BlockParagraph
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// --- fenced code: consume verbatim through the closing fence ---
		if fence, ok := codeFence(trimmed); ok {
			flush()
			start := i
			body := []string{line}
			i++
			for ; i < len(lines); i++ {
				body = append(body, lines[i])
				if closesFence(strings.TrimSpace(lines[i]), fence) {
					break
				}
			}
			// An unterminated fence runs to end of document, which is what a
			// renderer does too — better than re-interpreting the remainder as
			// prose and splitting inside it.
			blocks = append(blocks, Block{
				Kind:        BlockCode,
				Text:        strings.Join(body, "\n"),
				HeadingPath: strings.Join(path, headingSep),
				StartLine:   start + 1,
			})
			continue
		}

		// --- ATX heading ---
		if lvl, title, ok := atxHeading(trimmed); ok {
			flush()
			path = setHeadingPath(path, lvl, title)
			blocks = append(blocks, Block{
				Kind:        BlockHeading,
				Text:        title,
				HeadingPath: strings.Join(path, headingSep),
				Level:       lvl,
				StartLine:   i + 1,
			})
			continue
		}

		// --- setext heading: underlined by === or --- ---
		if len(para) == 1 && paraKind == BlockParagraph && isSetextUnderline(trimmed) {
			title := strings.TrimSpace(para[0])
			if title != "" {
				lvl := 1
				if trimmed[0] == '-' {
					lvl = 2
				}
				para = nil
				path = setHeadingPath(path, lvl, title)
				blocks = append(blocks, Block{
					Kind:        BlockHeading,
					Text:        title,
					HeadingPath: strings.Join(path, headingSep),
					Level:       lvl,
					StartLine:   i,
				})
				continue
			}
		}

		// --- blank line ends a block ---
		if trimmed == "" {
			flush()
			continue
		}

		// --- thematic break ---
		if isThematicBreak(trimmed) {
			flush()
			blocks = append(blocks, Block{
				Kind:        BlockThematic,
				Text:        trimmed,
				HeadingPath: strings.Join(path, headingSep),
				StartLine:   i + 1,
			})
			continue
		}

		// --- table: a pipe row followed by a delimiter row ---
		if isTableRow(trimmed) && i+1 < len(lines) && isTableDelimiter(strings.TrimSpace(lines[i+1])) {
			flush()
			start := i
			body := []string{line}
			i++
			for ; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if !isTableRow(t) && !isTableDelimiter(t) {
					i--
					break
				}
				body = append(body, lines[i])
			}
			blocks = append(blocks, Block{
				Kind:        BlockTable,
				Text:        strings.Join(body, "\n"),
				HeadingPath: strings.Join(path, headingSep),
				StartLine:   start + 1,
			})
			continue
		}

		// --- accumulate into the current block, classifying on first line ---
		if len(para) == 0 {
			paraStart = i + 1
			switch {
			case isListItem(trimmed):
				paraKind = BlockList
			case strings.HasPrefix(trimmed, ">"):
				paraKind = BlockQuote
			default:
				paraKind = BlockParagraph
			}
		} else {
			// A list or quote marker starting mid-paragraph begins a new block.
			if paraKind == BlockParagraph && (isListItem(trimmed) || strings.HasPrefix(trimmed, ">")) {
				flush()
				paraStart = i + 1
				if isListItem(trimmed) {
					paraKind = BlockList
				} else {
					paraKind = BlockQuote
				}
			}
		}
		para = append(para, line)
	}
	flush()
	return blocks
}

// setHeadingPath truncates the path to the new heading's depth and appends it.
// Levels may skip (h1 -> h3), so the path is padded rather than assumed dense.
func setHeadingPath(path []string, level int, title string) []string {
	if level < 1 {
		level = 1
	}
	depth := level - 1
	if depth > len(path) {
		depth = len(path)
	}
	out := append([]string{}, path[:depth]...)
	return append(out, title)
}

func codeFence(trimmed string) (string, bool) {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, f) {
			return f, true
		}
	}
	return "", false
}

// closesFence reports whether the line closes an open fence. A closing fence has
// no info string, which is what keeps "```go" inside a fence from closing it.
func closesFence(trimmed, fence string) bool {
	if !strings.HasPrefix(trimmed, fence) {
		return false
	}
	return strings.TrimSpace(strings.TrimLeft(trimmed, string(fence[0]))) == ""
}

func atxHeading(trimmed string) (level int, title string, ok bool) {
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	i := 0
	for i < len(trimmed) && trimmed[i] == '#' {
		i++
	}
	if i > 6 || i >= len(trimmed) || trimmed[i] != ' ' {
		return 0, "", false
	}
	title = strings.TrimSpace(strings.TrimRight(trimmed[i:], " #"))
	if title == "" {
		return 0, "", false
	}
	return i, title, true
}

func isSetextUnderline(trimmed string) bool {
	if len(trimmed) < 2 {
		return false
	}
	c := trimmed[0]
	if c != '=' && c != '-' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c {
			return false
		}
	}
	return true
}

// isThematicBreak matches ---, ***, ___ of length >= 3. Note the overlap with a
// setext h2 underline: the setext case is checked first, and only when a single
// paragraph line precedes it.
func isThematicBreak(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	c := trimmed[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c && trimmed[i] != ' ' {
			return false
		}
	}
	return true
}

func isListItem(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '-', '*', '+':
		return len(trimmed) > 1 && (trimmed[1] == ' ' || trimmed[1] == '\t')
	}
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(trimmed) {
		return false
	}
	return (trimmed[i] == '.' || trimmed[i] == ')') && i+1 < len(trimmed) && trimmed[i+1] == ' '
}

func isTableRow(trimmed string) bool {
	return strings.Contains(trimmed, "|") && strings.HasPrefix(trimmed, "|")
}

// isTableDelimiter matches the |---|:--:| row that turns pipe rows into a table.
func isTableDelimiter(trimmed string) bool {
	if !strings.Contains(trimmed, "-") || !strings.Contains(trimmed, "|") {
		return false
	}
	for _, r := range trimmed {
		switch r {
		case '|', '-', ':', ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// SplitFrontmatter separates a leading YAML frontmatter block from the body.
// Returns the raw frontmatter (without delimiters) and the remaining Markdown.
func SplitFrontmatter(md string) (frontmatter, body string) {
	// Tolerate a leading UTF-8 BOM before the opening delimiter.
	trimmed := strings.TrimPrefix(md, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return "", md
	}
	lines := strings.Split(trimmed, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", md
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	// No closing delimiter: treat the whole document as body rather than
	// swallowing it as frontmatter.
	return "", md
}
