package memory

import (
	"context"
	"hash/fnv"
	"sort"
	"strings"
)

// ChunkerKind selects a chunking strategy.
type ChunkerKind string

const (
	// ChunkerHeadings is the Phase 2 baseline: split on author-chosen headings.
	ChunkerHeadings ChunkerKind = "headings"
	// ChunkerLeidenLexical runs Leiden over a lexically-weighted sentence graph.
	//
	// MEASURED AND NOT RECOMMENDED. It costs nothing beyond the baseline, which was
	// the appeal, but its similarity signal is not discriminative at sentence
	// length: on a fixture with three planted topics, the AUC for separating
	// same-topic from cross-topic sentence pairs was 0.513 — a coin flip — against
	// 0.875 for bge-small embeddings. Sentences are short, so two sentences on one
	// topic frequently share no exact terms, and character n-grams mostly measure
	// English morphology. Retained because it satisfies the SentenceSimilarity
	// interface and may be usable at paragraph granularity, but it should not be
	// selected expecting topical chunking.
	ChunkerLeidenLexical ChunkerKind = "leiden-lexical"
	// ChunkerLeidenSemantic runs Leiden over embedding similarity. Best topical
	// coherence in principle, at roughly 5-6x the baseline's ingestion time.
	ChunkerLeidenSemantic ChunkerKind = "leiden-semantic"
)

// Valid reports whether k is a known chunker.
func (k ChunkerKind) Valid() bool {
	switch k {
	case ChunkerHeadings, ChunkerLeidenLexical, ChunkerLeidenSemantic:
		return true
	}
	return false
}

// DefaultResolution is CPM's γ.
//
// Measured, not guessed: swept against a fixture with three planted topics using
// the real bge-small embeddings and the relative similarity threshold, γ in
// [0.15, 0.20] recovered the topics exactly (pair agreement 1.000, three
// communities, all internally connected). Below that range communities merge
// (0.709 at γ=0.10); above it they fragment (0.836 at γ=0.80). 0.175 is the
// middle of the working band.
const DefaultResolution = 0.175

// LeidenChunkerConfig configures the Leiden chunker.
type LeidenChunkerConfig struct {
	Graph      GraphParams
	Resolution float64
	// Similarity supplies the pairwise scores. Required.
	Similarity SentenceSimilarity
}

// ChunkLeiden chunks a document by running Leiden over its sentence graph.
//
// Structure still wins where it exists: the document is first split into regions by
// the block parser, atomic blocks (code, tables) are emitted whole, and Leiden runs
// only over the prose within a heading's region. A heading is a boundary the author
// chose deliberately, and there is no reason to let a statistical method cross one.
func ChunkLeiden(ctx context.Context, blocks []Block, tc TokenCounter, cfg LeidenChunkerConfig) ([]Chunk, error) {
	if cfg.Similarity == nil {
		return nil, errNoSimilarity
	}
	gp := cfg.Graph
	if gp.TopK == 0 {
		gp = DefaultGraphParams()
	}

	var out []Chunk
	for _, region := range groupByHeading(blocks) {
		chunks, err := chunkRegion(ctx, region, tc, cfg, gp)
		if err != nil {
			return nil, err
		}
		out = append(out, chunks...)
	}
	return mergeSmallChunks(out, tc), nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errNoSimilarity = errString("leiden chunker: no similarity function configured")

// region is a heading and the blocks beneath it.
type region struct {
	headingPath string
	heading     *Block
	blocks      []Block
}

func groupByHeading(blocks []Block) []region {
	var out []region
	var cur *region
	for i := range blocks {
		b := blocks[i]
		if b.Kind == BlockThematic {
			continue
		}
		if b.Kind == BlockHeading {
			out = append(out, region{headingPath: b.HeadingPath, heading: &blocks[i]})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			out = append(out, region{headingPath: b.HeadingPath})
			cur = &out[len(out)-1]
		}
		cur.blocks = append(cur.blocks, b)
	}
	return out
}

// chunkRegion chunks one heading's content: atomic blocks pass through whole, prose
// goes through Leiden.
func chunkRegion(ctx context.Context, r region, tc TokenCounter, cfg LeidenChunkerConfig, gp GraphParams) ([]Chunk, error) {
	var out []Chunk

	// Atomic blocks are emitted as their own chunks — never split, never merged
	// into a topical community whose boundaries would cut them.
	var prose []Block
	flushProse := func() error {
		if len(prose) == 0 {
			return nil
		}
		cs, err := leidenChunksFromBlocks(ctx, prose, r, tc, cfg, gp)
		if err != nil {
			return err
		}
		out = append(out, cs...)
		prose = nil
		return nil
	}

	for _, b := range r.blocks {
		if b.Kind.Atomic() {
			if err := flushProse(); err != nil {
				return nil, err
			}
			text := b.Text
			if r.heading != nil {
				text = headingLine(r.heading) + "\n\n" + text
			}
			out = append(out, Chunk{
				Text: strings.TrimSpace(text), HeadingPath: r.headingPath,
				TokenCount: tc.Count(text),
			})
			continue
		}
		prose = append(prose, b)
	}
	if err := flushProse(); err != nil {
		return nil, err
	}
	return out, nil
}

func headingLine(b *Block) string {
	return strings.Repeat("#", b.Level) + " " + b.Text
}

// sentenceRef locates a sentence within a region's prose.
type sentenceRef struct {
	text  string
	block int
}

func leidenChunksFromBlocks(ctx context.Context, blocks []Block, r region, tc TokenCounter,
	cfg LeidenChunkerConfig, gp GraphParams) ([]Chunk, error) {

	var refs []sentenceRef
	for bi, b := range blocks {
		for _, s := range SplitSentences(b.Text) {
			refs = append(refs, sentenceRef{text: s, block: bi})
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	// Too few sentences to cluster meaningfully: keep them together and let the
	// budget split them if needed.
	if len(refs) < 4 {
		return packSentences(refs, 0, len(refs), 0, r, tc), nil
	}

	sentences := make([]string, len(refs))
	for i, rf := range refs {
		sentences[i] = rf.text
	}

	// Seed from the content so an unchanged document always chunks identically.
	seed := contentSeed(sentences)

	var runs [][2]int
	for _, w := range SentenceWindows(len(sentences), gp) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		win := sentences[w[0]:w[1]]
		sim, err := cfg.Similarity.Similarities(ctx, win)
		if err != nil {
			return nil, err
		}
		g := BuildSentenceGraph(sim, gp)
		part := Leiden(g, LeidenOptions{Resolution: cfg.Resolution, Seed: seed + int64(w[0])})

		// Communities become contiguous runs of sentence indices.
		for _, run := range contiguousRuns(part) {
			runs = append(runs, [2]int{w[0] + run[0], w[0] + run[1]})
		}
	}

	runs = dedupeOverlappingRuns(runs, len(sentences))

	var out []Chunk
	for ri, run := range runs {
		// Each run is a distinct community, numbered from 1 so it never collides
		// with the 0 used by chunkers that have no notion of community.
		out = append(out, packSentences(refs, run[0], run[1], ri+1, r, tc)...)
	}
	return out, nil
}

// contiguousRuns converts a community assignment into contiguous index ranges.
//
// This is where most of the remaining quality lives. A community whose members are
// scattered — sentences 1, 2, 7, 8 — becomes two runs rather than one incoherent
// chunk: a chunk must be a contiguous span of the document to be worth returning.
func contiguousRuns(part []int) [][2]int {
	if len(part) == 0 {
		return nil
	}
	var out [][2]int
	start := 0
	for i := 1; i <= len(part); i++ {
		if i == len(part) || part[i] != part[start] {
			out = append(out, [2]int{start, i})
			start = i
		}
	}
	return out
}

// dedupeOverlappingRuns resolves the overlap between adjacent windows: each
// sentence must land in exactly one run, or the chunk texts would duplicate content
// and the ingest-side dedup would then reject one of them.
func dedupeOverlappingRuns(runs [][2]int, n int) [][2]int {
	if len(runs) == 0 {
		return nil
	}
	sort.Slice(runs, func(a, b int) bool {
		if runs[a][0] != runs[b][0] {
			return runs[a][0] < runs[b][0]
		}
		return runs[a][1] > runs[b][1]
	})
	var out [][2]int
	covered := 0
	for _, r := range runs {
		start, end := r[0], r[1]
		if start < covered {
			start = covered
		}
		if start >= end {
			continue
		}
		out = append(out, [2]int{start, end})
		covered = end
	}
	// Anything left uncovered at the tail becomes its own run.
	if covered < n {
		out = append(out, [2]int{covered, n})
	}
	return out
}

// packSentences turns a sentence range into one or more chunks respecting the token
// budget, prefixing the region's heading so the stored text says what it is about.
func packSentences(refs []sentenceRef, from, to int, community int, r region, tc TokenCounter) []Chunk {
	if from >= to {
		return nil
	}
	prefix := ""
	if r.heading != nil {
		prefix = headingLine(r.heading) + "\n\n"
	}

	var out []Chunk
	var cur []string
	curFrom := from
	curTokens := tc.Count(prefix)
	flush := func(end int) {
		if len(cur) == 0 {
			return
		}
		text := strings.TrimSpace(prefix + strings.Join(cur, " "))
		if text != "" {
			out = append(out, Chunk{
				Text: text, HeadingPath: r.headingPath, TokenCount: tc.Count(text),
				SentFrom: curFrom, SentTo: end, Community: community,
			})
		}
		cur = nil
		curFrom = end
		curTokens = tc.Count(prefix)
	}

	for i := from; i < to; i++ {
		n := tc.Count(refs[i].text)
		if curTokens+n > MaxChunkTokens && len(cur) > 0 {
			flush(i)
		}
		cur = append(cur, refs[i].text)
		curTokens += n
	}
	flush(to)
	return out
}

// contentSeed derives a deterministic seed from the text, so the same document
// always produces the same partition and re-ingesting it does not churn the index.
func contentSeed(sentences []string) int64 {
	h := fnv.New64a()
	for _, s := range sentences {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
