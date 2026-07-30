package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"simple-cot-chat/store"
)

// Edge kinds. These are the values stored in memory_edges.kind and the keys of
// the walk's decay table.
const (
	EdgeNext         = "next"
	EdgePrev         = "prev"
	EdgeSimilar      = "similar"
	EdgeEntity       = "entity"
	EdgeLink         = "link"
	EdgeInferredLink = "inferred_link"
)

// EdgeParams tunes edge construction. The defaults come from DefaultEdgeParams;
// every cap exists to stop one pass from swamping the graph.
type EdgeParams struct {
	// SequentialWeight is the weight of next/prev edges. Adjacency is a certainty,
	// not an inference, so it is 1.0 and the walk's decay does the discounting.
	SequentialWeight float64

	// LinkWeight is the weight of a link edge aimed at a specific chunk (a
	// heading-qualified link, or an embed). LinkFallbackWeight applies when the
	// link names only the note and the edge is aimed at its first chunk, which is
	// a weaker claim about *which part* of the note is relevant.
	LinkWeight         float64
	LinkFallbackWeight float64

	// SimilarTopK is how many cross-source neighbours each chunk may propose.
	SimilarTopK int
	// SimilarThreshold is the minimum cosine for a similar edge.
	SimilarThreshold float64
	// SimilarAllowAsymmetric keeps a candidate edge even when only one endpoint
	// ranks the other in its top-k. Mutuality is the default, which is why this is
	// phrased as the opt-out: a bool field cannot distinguish "unset" from "false",
	// so the safer behaviour has to be the zero value.
	//
	// Without mutuality, a chunk of generic prose that is nobody's neighbour but has
	// hundreds of near-misses acquires edges purely by having been on the scanning
	// side, and becomes a hub the walk cannot avoid.
	SimilarAllowAsymmetric bool

	// EntityMaxChunks skips entities carried by more than this many chunks (see
	// store.RareEntityGroups for why the cap is a skip rather than a down-weight).
	EntityMaxChunks int
	// EntityMaxPerChunk caps how many entity edges one chunk may accumulate, keeping
	// the highest-weighted. Note that this can legitimately break symmetry: if b is
	// among a's strongest partners but a is not among b's, the pair survives only in
	// the a -> b direction. That is the intended outcome — the cap is per chunk's own
	// budget, not per pair.
	EntityMaxPerChunk int
	// EntityMinWeight drops entity edges whose inverse-frequency weight is below
	// this, i.e. entities common enough to be uninformative.
	EntityMinWeight float64
}

// DefaultEdgeParams are the plan's values (§3.5), with the caps set where the
// pair counts stay linear-ish in corpus size.
func DefaultEdgeParams() EdgeParams {
	return EdgeParams{
		SequentialWeight:   1.0,
		LinkWeight:         1.0,
		LinkFallbackWeight: 0.85,
		SimilarTopK:        8,
		SimilarThreshold:   0.7,
		EntityMaxChunks:    12,
		EntityMaxPerChunk:  12,
		EntityMinWeight:    0.15,
	}
}

func (p EdgeParams) withDefaults() EdgeParams {
	d := DefaultEdgeParams()
	if p.SequentialWeight == 0 {
		p.SequentialWeight = d.SequentialWeight
	}
	if p.LinkWeight == 0 {
		p.LinkWeight = d.LinkWeight
	}
	if p.LinkFallbackWeight == 0 {
		p.LinkFallbackWeight = d.LinkFallbackWeight
	}
	if p.SimilarTopK == 0 {
		p.SimilarTopK = d.SimilarTopK
	}
	if p.SimilarThreshold == 0 {
		p.SimilarThreshold = d.SimilarThreshold
	}
	if p.EntityMaxChunks == 0 {
		p.EntityMaxChunks = d.EntityMaxChunks
	}
	if p.EntityMaxPerChunk == 0 {
		p.EntityMaxPerChunk = d.EntityMaxPerChunk
	}
	return p
}

// EdgeReport counts what each pass wrote, and what it declined to write.
//
// The declined counts matter as much as the written ones: an edge build that
// silently drops most links looks identical to one with nothing to link, and the
// unresolved-link rate is the health metric for wikilink handling on a real vault.
type EdgeReport struct {
	Sequential int `json:"sequential"`
	Link       int `json:"link"`
	// LinkUnresolved counts links whose target is not in the corpus. On a vault this
	// is normal — links to not-yet-written notes are idiomatic Obsidian. Usually zero
	// here because IngestDirectory already filters unresolved links out of
	// Ingester.Links and reports them in IngestReport; this counts them for callers
	// that pass an unfiltered list.
	LinkUnresolved int `json:"linkUnresolved"`
	// LinkNoChunk counts resolved links whose target source has no chunks, which
	// happens when the target note is empty or was entirely deduped away.
	LinkNoChunk int `json:"linkNoChunk"`
	// LinkHeadingMissed counts heading-qualified links whose heading matched no
	// chunk, so the edge fell back to the note's first chunk.
	LinkHeadingMissed int `json:"linkHeadingMissed"`
	Similar           int `json:"similar"`
	// SimilarDroppedAsymmetric counts candidate similar edges rejected by the
	// mutual-top-k test.
	SimilarDroppedAsymmetric int `json:"similarDroppedAsymmetric"`
	Entity                   int `json:"entity"`
	// EntitySkippedCommon counts entities above EntityMaxChunks, which never
	// produce edges.
	EntitySkippedCommon int `json:"entitySkippedCommon"`
	// ChunksScanned is how many chunks the similarity pass was asked to score. On a
	// scoped pass that is the changed set, which may include chunks with no vector
	// yet — it is the work requested, not the work that found neighbours.
	ChunksScanned int           `json:"chunksScanned"`
	Duration      time.Duration `json:"duration"`
}

func (r EdgeReport) Total() int {
	return r.Sequential + r.Link + r.Similar + r.Entity
}

func (r EdgeReport) String() string {
	return fmt.Sprintf(
		"%d edges (%d sequential, %d link, %d similar, %d entity); "+
			"similarity pass scanned %d chunks; "+
			"links: %d unresolved, %d no-chunk, %d heading-missed; "+
			"similar: %d dropped asymmetric; entities: %d too common; in %s",
		r.Total(), r.Sequential, r.Link, r.Similar, r.Entity, r.ChunksScanned,
		r.LinkUnresolved, r.LinkNoChunk, r.LinkHeadingMissed,
		r.SimilarDroppedAsymmetric, r.EntitySkippedCommon,
		r.Duration.Round(time.Millisecond))
}

// BuildEdges runs every edge pass in the plan's order: sequential, link, entity,
// similar.
//
// The order is not arbitrary — it is cheapest-and-most-precise first, so a run
// interrupted partway through has built the edges that carry the most benefit per
// unit of work. Sequential is a SQL self-join, link is a human assertion needing
// no similarity computation, entity is an index scan, and similar is the O(n x
// corpus) vector pass.
//
// links may be nil, in which case the link pass is skipped rather than clearing
// existing link edges — a similarity rebuild must not discard authored links.
func (sys *System) BuildEdges(ctx context.Context, links []PendingLink, p EdgeParams) (EdgeReport, error) {
	started := time.Now()
	p = p.withDefaults()
	var rep EdgeReport

	n, err := sys.Store.BuildSequentialEdges("", p.SequentialWeight)
	if err != nil {
		return rep, err
	}
	rep.Sequential = n

	if err := ctx.Err(); err != nil {
		return rep, err
	}
	if len(links) > 0 {
		lr, err := sys.BuildLinkEdges(links, p)
		if err != nil {
			return rep, err
		}
		rep.Link = lr.Link
		rep.LinkUnresolved = lr.LinkUnresolved
		rep.LinkNoChunk = lr.LinkNoChunk
		rep.LinkHeadingMissed = lr.LinkHeadingMissed
	}

	if err := ctx.Err(); err != nil {
		return rep, err
	}
	er, err := sys.BuildEntityEdges(p)
	if err != nil {
		return rep, err
	}
	rep.Entity = er.Entity
	rep.EntitySkippedCommon = er.EntitySkippedCommon

	if err := ctx.Err(); err != nil {
		return rep, err
	}
	sr, err := sys.BuildSimilarEdges(ctx, nil, p)
	if err != nil {
		return rep, err
	}
	rep.Similar = sr.Similar
	rep.SimilarDroppedAsymmetric = sr.SimilarDroppedAsymmetric
	rep.ChunksScanned = sr.ChunksScanned

	rep.Duration = time.Since(started)
	return rep, nil
}

// BuildEdgesIncremental rebuilds only the edges an ingest run invalidated.
//
// Replacing a source's chunks deletes every edge touching them — including
// **inbound** ones. That is the part that makes a naive incremental rebuild wrong:
// a note linking *to* the changed note is not itself re-ingested, so its link edge
// is gone for good and the graph erodes silently with every edit. The fix is to
// rebuild from `memory_links`, which records links independently of chunk ids, and
// to include every link with either end in the changed set.
//
// What each pass needs:
//   - sequential — per changed source, cheap.
//   - link — every link touching a changed ref, in either direction.
//   - entity — a full rebuild; the per-chunk cap cannot be decided from a subset.
//   - similar — scoped to the new chunks. SimilarNeighbors writes both directions,
//     so scoring the new chunks restores the pairs an unchanged partner lost.
//
// Residual drift, stated rather than hidden: when a deleted chunk vacated a slot in
// an unchanged chunk's top-k, that slot is not refilled until the next full rebuild.
// The neighbour count only shrinks, never points anywhere wrong.
func (sys *System) BuildEdgesIncremental(ctx context.Context, rep IngestReport, p EdgeParams) (EdgeReport, error) {
	started := time.Now()
	p = p.withDefaults()
	var out EdgeReport

	if len(rep.ChangedRefs) == 0 && len(rep.ChangedSourceIDs) == 0 {
		out.Duration = time.Since(started)
		return out, nil
	}

	for _, id := range rep.ChangedSourceIDs {
		n, err := sys.Store.BuildSequentialEdges(id, p.SequentialWeight)
		if err != nil {
			return out, err
		}
		out.Sequential += n
	}

	if err := ctx.Err(); err != nil {
		return out, err
	}
	stored, err := sys.Store.LinksTouching(rep.ChangedRefs)
	if err != nil {
		return out, err
	}
	if len(stored) > 0 {
		links := make([]PendingLink, 0, len(stored))
		for _, l := range stored {
			links = append(links, PendingLink{
				FromSourceRef: l.FromRef, ToSourceRef: l.ToRef,
				Heading: l.Heading, Raw: l.Raw, Embed: l.Embed, Kind: l.Kind,
			})
		}
		lr, err := sys.BuildLinkEdges(links, p)
		if err != nil {
			return out, err
		}
		out.Link = lr.Link
		out.LinkNoChunk = lr.LinkNoChunk
		out.LinkHeadingMissed = lr.LinkHeadingMissed
	}

	if err := ctx.Err(); err != nil {
		return out, err
	}
	er, err := sys.BuildEntityEdges(p)
	if err != nil {
		return out, err
	}
	out.Entity = er.Entity
	out.EntitySkippedCommon = er.EntitySkippedCommon

	if err := ctx.Err(); err != nil {
		return out, err
	}
	ids, err := sys.Store.ChunkIDsForSources(rep.ChangedSourceIDs)
	if err != nil {
		return out, err
	}
	if len(ids) > 0 {
		sr, err := sys.BuildSimilarEdges(ctx, ids, p)
		if err != nil {
			return out, err
		}
		out.Similar = sr.Similar
		out.SimilarDroppedAsymmetric = sr.SimilarDroppedAsymmetric
		out.ChunksScanned = sr.ChunksScanned
	}

	out.Duration = time.Since(started)
	return out, nil
}

// BuildLinkEdges writes `link` edges for resolved wikilinks and Markdown links.
//
// This is the highest-precision edge kind in the system and the cheapest to
// build: a wikilink is a human assertion that two notes belong together, not a
// statistical inference. It needs no embeddings, so it works on a corpus whose
// model was never provisioned.
//
// Edges are directed — from the chunk containing the link to the chunk it names —
// and the walk traverses them both ways, since "notes that link here" is as
// meaningful as forward links.
//
// Two things a caller should know. Both ends are resolved as *directory* sources,
// which is where wikilinks come from; conversation and artifact sources have none.
// And this loads the chunk text of every note that participates in a link, because
// locating the linking chunk needs it — bounded by the linked subset of the vault,
// not the whole corpus, but not free either.
func (sys *System) BuildLinkEdges(links []PendingLink, p EdgeParams) (EdgeReport, error) {
	p = p.withDefaults()
	var rep EdgeReport
	if len(links) == 0 {
		return rep, nil
	}

	// One lookup for every source that appears on either end.
	refSet := map[string]bool{}
	for _, l := range links {
		if l.ToSourceRef == "" {
			rep.LinkUnresolved++
			continue
		}
		refSet[l.FromSourceRef] = true
		refSet[l.ToSourceRef] = true
	}
	if len(refSet) == 0 {
		return rep, nil
	}
	refs := make([]string, 0, len(refSet))
	for r := range refSet {
		refs = append(refs, r)
	}
	sort.Strings(refs)

	byRef, err := sys.Store.ChunkRefsBySourceRefs(store.SourceDirectory, refs)
	if err != nil {
		return rep, err
	}

	var edges []store.MemoryEdge
	seen := map[[2]string]bool{}
	for _, l := range links {
		if l.ToSourceRef == "" {
			continue
		}
		from, to := byRef[l.FromSourceRef], byRef[l.ToSourceRef]
		if len(from) == 0 || len(to) == 0 {
			rep.LinkNoChunk++
			continue
		}

		// Aim at the target chunk carrying the named heading; fall back to the
		// note's first chunk. A heading that matches nothing is counted, because
		// silently degrading to the first chunk would hide a real resolution
		// failure — a renamed heading, or a chunker that dropped it.
		dst := to[0]
		weight := p.LinkFallbackWeight
		if l.Heading != "" {
			if c, ok := chunkForHeading(to, l.Heading); ok {
				dst, weight = c, p.LinkWeight
			} else {
				rep.LinkHeadingMissed++
			}
		} else if l.Embed {
			// A transclusion pulls the whole note in, so the note-level target is
			// exactly what was meant rather than a fallback.
			weight = p.LinkWeight
		}

		// An inferred link is the model's guess, not the user's assertion, so it gets
		// its own kind and a lower weight — and the walk decays it below `similar`
		// besides (§3.5). Same machinery, honestly labelled.
		kind := EdgeLink
		if l.Kind == EdgeInferredLink {
			kind = EdgeInferredLink
			weight = InferredLinkWeight
		}

		// Leave from the chunk that actually contains the link. Multiple chunks may
		// contain it (a note repeating a reference), and each is a separate
		// assertion.
		srcs := chunksContaining(from, l.Raw)
		if len(srcs) == 0 {
			srcs = from[:1]
		}
		for _, src := range srcs {
			if src.ChunkID == dst.ChunkID {
				continue // a note linking itself adds nothing to traverse
			}
			key := [2]string{src.ChunkID, dst.ChunkID}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, store.MemoryEdge{
				SrcChunkID: src.ChunkID, DstChunkID: dst.ChunkID,
				Kind: kind, Weight: weight,
			})
		}
	}

	if err := sys.Store.InsertEdgesBatch(edges); err != nil {
		return rep, err
	}
	rep.Link = len(edges)
	return rep, nil
}

// chunkForHeading finds the chunk whose heading path ends with the named heading.
// Matching the last segment rather than the whole path is what makes
// "[[Note#Data model]]" resolve against a heading_path of "Architecture › Data
// model".
func chunkForHeading(chunks []store.ChunkRef, heading string) (store.ChunkRef, bool) {
	want := strings.ToLower(strings.TrimSpace(heading))
	if want == "" {
		return store.ChunkRef{}, false
	}
	for _, c := range chunks {
		path := strings.ToLower(c.HeadingPath)
		if path == want || strings.HasSuffix(path, headingSep+want) {
			return c, true
		}
	}
	// A chunk may sit under a deeper heading than the one linked, in which case the
	// linked heading appears mid-path. Take the first such chunk, which is the
	// section's opening.
	for _, c := range chunks {
		if strings.Contains(strings.ToLower(c.HeadingPath), want) {
			return c, true
		}
	}
	return store.ChunkRef{}, false
}

// chunksContaining returns the chunks whose text contains the raw link.
func chunksContaining(chunks []store.ChunkRef, raw string) []store.ChunkRef {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []store.ChunkRef
	for _, c := range chunks {
		if strings.Contains(c.Text, raw) {
			out = append(out, c)
		}
	}
	return out
}

// BuildEntityEdges connects chunks that share a rare entity, weighted by inverse
// entity frequency.
//
// IEF is what keeps this pass from being useless. Without it a ubiquitous project
// name wires the whole corpus together and the walk returns arbitrary chunks; with
// it, a shared rare proper noun is strong evidence and a shared common one is
// nearly nothing. Entities above EntityMaxChunks are skipped outright, since their
// pair count is quadratic and their weight would be near zero anyway.
//
// Edges are written in both directions: co-occurrence is symmetric, and storing
// both halves lets the walk read a chunk's neighbours with one outbound query.
//
// This pass is always a full rebuild — the per-chunk cap keeps the strongest edges,
// which cannot be decided from a subset. Cheap at current corpus sizes (one index
// scan plus a batched insert), but it is the pass to look at first if incremental
// re-ingest of a large vault turns out slow: unlike BuildSimilarEdges, there is no
// scoped form.
func (sys *System) BuildEntityEdges(p EdgeParams) (EdgeReport, error) {
	p = p.withDefaults()
	var rep EdgeReport

	st, err := sys.Store.MemoryStats()
	if err != nil {
		return rep, err
	}
	if st.Chunks < 2 {
		return rep, nil
	}
	groups, err := sys.Store.RareEntityGroups(p.EntityMaxChunks)
	if err != nil {
		return rep, err
	}
	if _, err := sys.Store.DeleteEdgesByKind(EdgeEntity); err != nil {
		return rep, err
	}

	// IEF, normalized to [0,1]: an entity in one chunk out of N scores ~1, one in
	// N/2 scores ~0.1. log(N) as the denominator keeps the scale independent of
	// corpus size, so EntityMinWeight means the same thing on a 100-chunk corpus
	// as on a 100,000-chunk one.
	n := float64(st.Chunks)
	logN := math.Log(n)
	if logN <= 0 {
		return rep, nil
	}

	// Per-chunk accumulation, so the per-chunk cap keeps the best edges rather than
	// whichever entity happened to be processed first.
	type pair struct {
		dst    string
		weight float64
	}
	byChunk := map[string][]pair{}
	for _, g := range groups {
		df := float64(len(g.Chunks))
		w := math.Log(n/df) / logN
		if w < p.EntityMinWeight {
			rep.EntitySkippedCommon++
			continue
		}
		if w > 1 {
			w = 1
		}
		for i := 0; i < len(g.Chunks); i++ {
			for j := i + 1; j < len(g.Chunks); j++ {
				a, b := g.Chunks[i], g.Chunks[j]
				// Same-source pairs already have next/prev edges; spending the
				// per-chunk budget on them would crowd out cross-document links.
				if a.SourceID == b.SourceID {
					continue
				}
				byChunk[a.ChunkID] = append(byChunk[a.ChunkID], pair{b.ChunkID, w})
				byChunk[b.ChunkID] = append(byChunk[b.ChunkID], pair{a.ChunkID, w})
			}
		}
	}

	var edges []store.MemoryEdge
	for _, src := range sortedKeys(byChunk) {
		ps := byChunk[src]
		// Highest weight first, then by id so a tie is resolved deterministically —
		// re-running the pass must produce the same graph.
		sort.Slice(ps, func(a, b int) bool {
			if ps[a].weight != ps[b].weight {
				return ps[a].weight > ps[b].weight
			}
			return ps[a].dst < ps[b].dst
		})
		kept := 0
		seen := map[string]bool{}
		for _, pr := range ps {
			if kept >= p.EntityMaxPerChunk {
				break
			}
			if seen[pr.dst] {
				continue // two shared entities: the stronger one already counted
			}
			seen[pr.dst] = true
			kept++
			edges = append(edges, store.MemoryEdge{
				SrcChunkID: src, DstChunkID: pr.dst, Kind: EdgeEntity, Weight: pr.weight,
			})
		}
	}

	if err := sys.Store.InsertEdgesBatch(edges); err != nil {
		return rep, err
	}
	rep.Entity = len(edges)
	return rep, nil
}

// BuildSimilarEdges writes `similar` edges from mutual top-k cosine similarity
// across sources.
//
// ids scopes the pass to chunks that changed; nil means every embedded chunk.
// This is the expensive pass — O(|ids| x corpus) — which is why the incremental
// form exists at all.
//
// Mutuality is what makes the result a sparse graph rather than a hub-and-spoke
// one. Without it, a chunk of generic prose is in nobody's top-k but has hundreds
// of near-misses, and one-sided edges would give it a high degree it has not
// earned. The cost is a second similarity query for the far endpoints, which is
// why SimilarAllowAsymmetric exists as an escape hatch rather than mutuality being
// hard-coded.
func (sys *System) BuildSimilarEdges(ctx context.Context, ids []string, p EdgeParams) (EdgeReport, error) {
	p = p.withDefaults()
	var rep EdgeReport

	full := len(ids) == 0
	if full {
		all, err := sys.Store.EmbeddedChunkIDs()
		if err != nil {
			return rep, err
		}
		ids = all
	}
	if len(ids) == 0 {
		return rep, nil
	}
	rep.ChunksScanned = len(ids)

	if full {
		// A from-scratch pass replaces the whole similarity graph. Scoped passes
		// must not, or re-ingesting one note would delete every other note's
		// similarity edges.
		if _, err := sys.Store.DeleteEdgesByKind(EdgeSimilar); err != nil {
			return rep, err
		}
	}

	cand, err := sys.Store.SimilarNeighbors(ids, p.SimilarTopK, p.SimilarThreshold)
	if err != nil {
		return rep, err
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}

	// topK[x] is the set x itself ranks highly, needed for the mutual test.
	topK := map[string]map[string]bool{}
	addAll := func(es []store.MemoryEdge) {
		for _, e := range es {
			if topK[e.SrcChunkID] == nil {
				topK[e.SrcChunkID] = map[string]bool{}
			}
			topK[e.SrcChunkID][e.DstChunkID] = true
		}
	}
	addAll(cand)

	if !p.SimilarAllowAsymmetric {
		// Resolve the far endpoints we do not already have a top-k for. On a full
		// pass every endpoint is already covered, so this costs nothing; on an
		// incremental one it is the price of mutuality.
		var missing []string
		for _, e := range cand {
			if _, ok := topK[e.DstChunkID]; !ok {
				topK[e.DstChunkID] = nil // mark as pending so it is requested once
				missing = append(missing, e.DstChunkID)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			far, err := sys.Store.SimilarNeighbors(missing, p.SimilarTopK, p.SimilarThreshold)
			if err != nil {
				return rep, err
			}
			addAll(far)
		}
	}

	// Deduped before insert rather than left to ON CONFLICT: a mutual pair appears
	// twice in cand (once from each side) and each proposal writes both directions,
	// so without this the reported count is up to double the rows actually written —
	// a report that overstates its own work is worse than no report.
	var edges []store.MemoryEdge
	written := map[[2]string]bool{}
	add := func(src, dst string, w float64) {
		key := [2]string{src, dst}
		if written[key] {
			return
		}
		written[key] = true
		edges = append(edges, store.MemoryEdge{
			SrcChunkID: src, DstChunkID: dst, Kind: EdgeSimilar, Weight: w,
		})
	}
	for _, e := range cand {
		if !p.SimilarAllowAsymmetric && !topK[e.DstChunkID][e.SrcChunkID] {
			rep.SimilarDroppedAsymmetric++
			continue
		}
		// Both directions, so the walk needs only outbound edges.
		add(e.SrcChunkID, e.DstChunkID, e.Weight)
		add(e.DstChunkID, e.SrcChunkID, e.Weight)
	}
	if err := sys.Store.InsertEdgesBatch(edges); err != nil {
		return rep, err
	}
	rep.Similar = len(edges)
	return rep, nil
}
