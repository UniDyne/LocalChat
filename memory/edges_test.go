package memory

import (
	"context"
	"sort"
	"strings"
	"testing"

	"simple-cot-chat/store"
)

// newEdgeSystem ingests the synthetic vault and returns the system plus the
// resolved links, ready for an edge pass.
func newEdgeSystem(t *testing.T, emb Embedder) (*System, []PendingLink) {
	t.Helper()
	s := openTestStore(t)
	sys := NewSystem(s, Config{Embedder: emb, ModelDir: t.TempDir()}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	root := writeVault(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if emb != nil {
		if _, err := sys.Backfill(context.Background(), 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	return sys, sys.Ingester.Links
}

// edgeSet reads every edge back, keyed for assertion.
func edgeSet(t *testing.T, s *store.Store) []store.MemoryEdge {
	t.Helper()
	ids, err := allChunkIDs(s)
	if err != nil {
		t.Fatal(err)
	}
	bySrc, err := s.GetEdgesFromMany(ids)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.MemoryEdge
	for _, id := range ids {
		out = append(out, bySrc[id]...)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].SrcChunkID != out[b].SrcChunkID {
			return out[a].SrcChunkID < out[b].SrcChunkID
		}
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].DstChunkID < out[b].DstChunkID
	})
	return out
}

func allChunkIDs(s *store.Store) ([]string, error) {
	srcs, err := s.ListSources()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, src := range srcs {
		chunks, err := s.GetChunksBySource(src.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range chunks {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// chunkIndex maps chunk id to its source_ref and heading path, so an assertion can
// talk about notes and sections rather than uuids.
type chunkIndex struct {
	ref     map[string]string
	heading map[string]string
	text    map[string]string
	byRef   map[string][]string
}

func indexChunks(t *testing.T, s *store.Store) chunkIndex {
	t.Helper()
	idx := chunkIndex{
		ref: map[string]string{}, heading: map[string]string{},
		text: map[string]string{}, byRef: map[string][]string{},
	}
	srcs, err := s.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		chunks, err := s.GetChunksBySource(src.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range chunks {
			idx.ref[c.ID] = src.SourceRef
			idx.heading[c.ID] = c.HeadingPath
			idx.text[c.ID] = c.Text
			idx.byRef[src.SourceRef] = append(idx.byRef[src.SourceRef], c.ID)
		}
	}
	return idx
}

// TestLinkEdgesFromVault is the Phase 6 first deliverable: the cheapest and
// highest-precision edge kind, built from wikilinks already resolved at ingest.
func TestLinkEdgesFromVault(t *testing.T) {
	sys, links := newEdgeSystem(t, nil)
	if len(links) == 0 {
		t.Fatal("ingestion resolved no links; the link pass has nothing to build from")
	}

	rep, err := sys.BuildLinkEdges(links, EdgeParams{})
	if err != nil {
		t.Fatalf("BuildLinkEdges: %v", err)
	}
	t.Logf("link pass: %d edges, %d no-chunk, %d heading-missed", rep.Link, rep.LinkNoChunk, rep.LinkHeadingMissed)
	if rep.Link == 0 {
		t.Fatal("no link edges written")
	}

	idx := indexChunks(t, sys.Store)
	edges := edgeSet(t, sys.Store)

	// Every edge is a link edge (no other pass has run) and connects two distinct
	// chunks in two distinct notes.
	type pair struct{ from, to string }
	seen := map[pair]bool{}
	for _, e := range edges {
		if e.Kind != EdgeLink {
			t.Fatalf("unexpected edge kind %q before other passes ran", e.Kind)
		}
		if e.SrcChunkID == e.DstChunkID {
			t.Error("self edge written")
		}
		seen[pair{idx.ref[e.SrcChunkID], idx.ref[e.DstChunkID]}] = true
	}

	// Architecture links Data Model and Retrieval; Data Model links back and embeds
	// Retrieval; the daily note links Architecture.
	for _, want := range []pair{
		{"Architecture.md", "Data Model.md"},
		{"Architecture.md", "Retrieval.md"},
		{"Data Model.md", "Architecture.md"},
		{"Data Model.md", "Retrieval.md"},
		{"daily/2026-07-28.md", "Architecture.md"},
	} {
		if !seen[want] {
			t.Errorf("missing link edge %s -> %s", want.from, want.to)
		}
	}

	// An unresolved target and an ambiguous basename must not produce edges. Broken
	// Links.md references only [[No Such Note]] and [[Duplicate]], so it should have
	// no outbound link edges at all.
	for _, e := range edges {
		if idx.ref[e.SrcChunkID] == "Broken Links.md" {
			t.Errorf("Broken Links.md produced an edge to %s; unresolved and ambiguous "+
				"targets must be dropped, not guessed", idx.ref[e.DstChunkID])
		}
	}

	// [[Retrieval#Fusion]] must aim at the Fusion section, not the note's first
	// chunk. Aiming at the wrong chunk is the failure this test exists for: it
	// would look like a working link pass while expanding to the wrong section.
	var sawFusion bool
	for _, e := range edges {
		if idx.ref[e.SrcChunkID] != "Architecture.md" || idx.ref[e.DstChunkID] != "Retrieval.md" {
			continue
		}
		if strings.Contains(idx.heading[e.DstChunkID], "Fusion") {
			sawFusion = true
			if e.Weight != DefaultEdgeParams().LinkWeight {
				t.Errorf("heading-qualified link weight = %v, want %v",
					e.Weight, DefaultEdgeParams().LinkWeight)
			}
		}
	}
	if !sawFusion {
		t.Error("[[Retrieval#Fusion]] did not resolve to the Fusion section")
	}

	// The edge must leave the chunk that contains the link, not the note's first
	// chunk by default.
	for _, e := range edges {
		if idx.ref[e.SrcChunkID] == "Architecture.md" && idx.ref[e.DstChunkID] == "Data Model.md" {
			if !strings.Contains(idx.text[e.SrcChunkID], "[[Data Model]]") {
				t.Errorf("link edge leaves a chunk that does not contain the link: %q",
					truncate(idx.text[e.SrcChunkID], 80))
			}
		}
	}

	// The alias link [[Arch]] resolved, which is what makes frontmatter aliases
	// worth reading in ingestion pass 1.
	var aliasEdges int
	for _, e := range edges {
		if idx.ref[e.SrcChunkID] == "Data Model.md" && idx.ref[e.DstChunkID] == "Architecture.md" {
			aliasEdges++
		}
	}
	if aliasEdges == 0 {
		t.Error("alias link [[Arch]] produced no edge")
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestSequentialEdges checks the cheapest pass: both directions, within a source
// only, and idempotent.
func TestSequentialEdges(t *testing.T) {
	sys, _ := newEdgeSystem(t, nil)

	n, err := sys.Store.BuildSequentialEdges("", 1.0)
	if err != nil {
		t.Fatalf("BuildSequentialEdges: %v", err)
	}
	if n == 0 {
		t.Fatal("no sequential edges written")
	}

	idx := indexChunks(t, sys.Store)
	edges := edgeSet(t, sys.Store)
	nextCount, prevCount := 0, 0
	for _, e := range edges {
		if idx.ref[e.SrcChunkID] != idx.ref[e.DstChunkID] {
			t.Errorf("sequential edge crosses sources: %s -> %s",
				idx.ref[e.SrcChunkID], idx.ref[e.DstChunkID])
		}
		switch e.Kind {
		case EdgeNext:
			nextCount++
		case EdgePrev:
			prevCount++
		default:
			t.Errorf("unexpected kind %q", e.Kind)
		}
	}
	if nextCount != prevCount {
		t.Errorf("next (%d) and prev (%d) counts differ; every adjacency should be "+
			"stored both ways", nextCount, prevCount)
	}

	// A second run must not duplicate: the pass runs on every ingest, so a
	// non-idempotent one would grow the graph without bound.
	before := len(edges)
	if _, err := sys.Store.BuildSequentialEdges("", 1.0); err != nil {
		t.Fatal(err)
	}
	if after := len(edgeSet(t, sys.Store)); after != before {
		t.Errorf("re-running the sequential pass changed the edge count %d -> %d", before, after)
	}
}

// TestEntityEdgesAreRareAndCapped covers the two things that make the entity pass
// usable rather than a graph-wide short circuit.
func TestEntityEdgesAreRareAndCapped(t *testing.T) {
	sys, _ := newEdgeSystem(t, nil)

	rep, err := sys.BuildEntityEdges(EdgeParams{})
	if err != nil {
		t.Fatalf("BuildEntityEdges: %v", err)
	}
	t.Logf("entity pass: %d edges, %d entities too common", rep.Entity, rep.EntitySkippedCommon)

	idx := indexChunks(t, sys.Store)
	edges := edgeSet(t, sys.Store)

	perChunk := map[string]int{}
	symmetric := map[[2]string]bool{}
	for _, e := range edges {
		if e.Kind != EdgeEntity {
			t.Fatalf("unexpected kind %q", e.Kind)
		}
		if idx.ref[e.SrcChunkID] == idx.ref[e.DstChunkID] {
			t.Errorf("entity edge within one source (%s); next/prev already covers that",
				idx.ref[e.SrcChunkID])
		}
		if e.Weight <= 0 || e.Weight > 1 {
			t.Errorf("entity weight %v outside (0,1]", e.Weight)
		}
		if e.Weight < DefaultEdgeParams().EntityMinWeight {
			t.Errorf("entity weight %v below EntityMinWeight; common entities should "+
				"be dropped, not written weakly", e.Weight)
		}
		perChunk[e.SrcChunkID]++
		symmetric[[2]string{e.SrcChunkID, e.DstChunkID}] = true
	}
	for id, n := range perChunk {
		if n > DefaultEdgeParams().EntityMaxPerChunk {
			t.Errorf("chunk %s has %d entity edges, over the cap of %d",
				id, n, DefaultEdgeParams().EntityMaxPerChunk)
		}
	}
	// Co-occurrence is symmetric, so both halves should be written — but only while
	// neither endpoint is at its per-chunk cap, which is a per-chunk budget rather
	// than a per-pair one. Assert symmetry only below the cap.
	for pair := range symmetric {
		if perChunk[pair[1]] >= DefaultEdgeParams().EntityMaxPerChunk {
			continue
		}
		if !symmetric[[2]string{pair[1], pair[0]}] {
			t.Errorf("entity edge %s -> %s has no reverse below the per-chunk cap; "+
				"co-occurrence is symmetric", pair[0], pair[1])
		}
	}

	// Rebuilding replaces rather than accumulates.
	before := len(edges)
	if _, err := sys.BuildEntityEdges(EdgeParams{}); err != nil {
		t.Fatal(err)
	}
	if after := len(edgeSet(t, sys.Store)); after != before {
		t.Errorf("rebuilding the entity pass changed the edge count %d -> %d", before, after)
	}
}

// TestSimilarEdgesAreMutual is the property that keeps the similarity graph sparse:
// a one-sided near-miss must not earn an edge.
func TestSimilarEdgesAreMutual(t *testing.T) {
	sys, _ := newEdgeSystem(t, NewFakeEmbedder())

	// A low threshold so the fake embedder's vectors produce candidates at all; the
	// property under test is mutuality, not the threshold.
	p := EdgeParams{SimilarTopK: 3, SimilarThreshold: 0.05}
	rep, err := sys.BuildSimilarEdges(context.Background(), nil, p)
	if err != nil {
		t.Fatalf("BuildSimilarEdges: %v", err)
	}
	t.Logf("similar pass: %d edges over %d chunks, %d dropped asymmetric",
		rep.Similar, rep.ChunksScanned, rep.SimilarDroppedAsymmetric)
	if rep.Similar == 0 {
		t.Skip("fake embedder produced no pairs above threshold")
	}

	idx := indexChunks(t, sys.Store)
	edges := edgeSet(t, sys.Store)
	have := map[[2]string]bool{}
	for _, e := range edges {
		if e.Kind != EdgeSimilar {
			t.Fatalf("unexpected kind %q", e.Kind)
		}
		if idx.ref[e.SrcChunkID] == idx.ref[e.DstChunkID] {
			t.Errorf("similar edge within one source (%s)", idx.ref[e.SrcChunkID])
		}
		if e.Weight < p.SimilarThreshold {
			t.Errorf("similar weight %v below threshold %v", e.Weight, p.SimilarThreshold)
		}
		have[[2]string{e.SrcChunkID, e.DstChunkID}] = true
	}
	for pair := range have {
		if !have[[2]string{pair[1], pair[0]}] {
			t.Errorf("similar edge %s -> %s has no reverse", pair[0], pair[1])
		}
	}

	// Asymmetric mode must admit at least as many edges — that is what the mutual
	// test is filtering out.
	loose := p
	loose.SimilarAllowAsymmetric = true
	looseRep, err := sys.BuildSimilarEdges(context.Background(), nil, loose)
	if err != nil {
		t.Fatal(err)
	}
	if looseRep.Similar < rep.Similar {
		t.Errorf("non-mutual pass produced fewer edges (%d) than the mutual one (%d)",
			looseRep.Similar, rep.Similar)
	}
	t.Logf("mutual %d edges vs non-mutual %d", rep.Similar, looseRep.Similar)
}

// TestBuildEdgesDeterministic is load-bearing for incremental re-ingest: if the
// same corpus yields a different graph each run, every re-ingest churns the edge
// table and nothing downstream can be asserted.
func TestBuildEdgesDeterministic(t *testing.T) {
	sys, links := newEdgeSystem(t, NewFakeEmbedder())

	p := EdgeParams{SimilarTopK: 3, SimilarThreshold: 0.05}
	first, err := sys.BuildEdges(context.Background(), links, p)
	if err != nil {
		t.Fatalf("BuildEdges: %v", err)
	}
	t.Log(first.String())
	a := edgeSet(t, sys.Store)

	second, err := sys.BuildEdges(context.Background(), links, p)
	if err != nil {
		t.Fatal(err)
	}
	b := edgeSet(t, sys.Store)

	if len(a) != len(b) {
		t.Fatalf("edge count changed on rebuild: %d -> %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("edge %d differs after rebuild:\n  %+v\n  %+v", i, a[i], b[i])
		}
	}
	if first.Total() != second.Total() {
		t.Errorf("reported totals differ: %d vs %d", first.Total(), second.Total())
	}

	byKind, err := sys.Store.EdgeCountsByKind()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("edges by kind: %v", byKind)
	for _, kind := range []string{EdgeNext, EdgePrev, EdgeLink} {
		if byKind[kind] == 0 {
			t.Errorf("no %s edges built", kind)
		}
	}
}

// TestEdgesDeletedWithChunks guards the cascade §3.4 warns about: a surviving
// chunk holding an edge into a deleted one would make the walk dereference a
// chunk that no longer exists.
func TestEdgesDeletedWithChunks(t *testing.T) {
	sys, links := newEdgeSystem(t, nil)
	if _, err := sys.BuildEdges(context.Background(), links, EdgeParams{}); err != nil {
		t.Fatal(err)
	}

	src, found, err := sys.Store.FindSource(store.SourceDirectory, "Retrieval.md")
	if err != nil || !found {
		t.Fatalf("Retrieval.md missing: %v", err)
	}
	doomed := map[string]bool{}
	chunks, err := sys.Store.GetChunksBySource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		doomed[c.ID] = true
	}
	// Retrieval.md is a link *target* from two other notes, so its deletion is
	// exactly the inbound-edge case.
	if err := sys.Store.DeleteSource(src.ID); err != nil {
		t.Fatal(err)
	}

	for _, e := range edgeSet(t, sys.Store) {
		if doomed[e.SrcChunkID] || doomed[e.DstChunkID] {
			t.Errorf("edge %+v survives a deleted chunk", e)
		}
	}
}
