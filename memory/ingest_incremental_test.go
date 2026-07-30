package memory

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"simple-cot-chat/store"
)

// chunkIDsByRef maps source_ref to its chunk ids, for asserting that an unchanged
// file's chunks were not churned.
//
// Chunk-id stability is the load-bearing property of incremental re-ingest, not a
// nicety: re-chunking assigns fresh ids, and every edge touching the old ones is
// deleted with them. A re-scan that quietly re-chunked unchanged notes would erode
// the graph on every run while all the counters still looked right.
func chunkIDsByRef(t *testing.T, s *store.Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	srcs, err := s.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		chunks, err := s.GetChunksBySource(src.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(chunks))
		for _, c := range chunks {
			ids = append(ids, c.ID)
		}
		sort.Strings(ids)
		out[src.SourceRef] = ids
	}
	return out
}

func newIncrementalSystem(t *testing.T, root string) *System {
	t.Helper()
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })
	return sys
}

// TestReingestUnchangedIsNoOp is the Phase 7 exit criterion for directory sources:
// re-scanning an unchanged vault must be cheap.
//
// "Cheap" is asserted concretely rather than by timing, which would be flaky: not a
// single byte of note content may be read, and no chunk may be rewritten.
func TestReingestUnchangedIsNoOp(t *testing.T) {
	root := writeVault(t)
	sys := newIncrementalSystem(t, root)

	first, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("first:  " + first.String())
	if first.FilesIngested == 0 {
		t.Fatal("first run ingested nothing")
	}
	if first.BytesRead == 0 {
		t.Fatal("first run reported reading no bytes")
	}
	before := chunkIDsByRef(t, sys.Store)

	second, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("second: " + second.String())

	if second.FilesIngested != 0 {
		t.Errorf("re-scan ingested %d files; nothing changed", second.FilesIngested)
	}
	if second.FilesSkippedByStat != second.FilesSeen {
		t.Errorf("skipped %d of %d files by stat; every file should take the fast path "+
			"on an unchanged re-scan", second.FilesSkippedByStat, second.FilesSeen)
	}
	if second.FilesSkippedByHash != 0 {
		t.Errorf("%d files fell through to hashing; their (mtime, size) should have "+
			"matched", second.FilesSkippedByHash)
	}
	if second.BytesRead != 0 {
		t.Errorf("re-scan read %d bytes of note content; an unchanged file must not be "+
			"opened at all", second.BytesRead)
	}
	if len(second.ChangedRefs) != 0 {
		t.Errorf("re-scan reported changed refs %v", second.ChangedRefs)
	}
	if second.FilesDeleted != 0 {
		t.Errorf("re-scan deleted %d sources", second.FilesDeleted)
	}

	after := chunkIDsByRef(t, sys.Store)
	assertSameChunkIDs(t, before, after, nil)
}

// assertSameChunkIDs compares two chunk-id snapshots, permitting change only for
// refs listed in mayChange.
func assertSameChunkIDs(t *testing.T, before, after map[string][]string, mayChange map[string]bool) {
	t.Helper()
	for ref, ids := range before {
		if mayChange[ref] {
			continue
		}
		got, ok := after[ref]
		if !ok {
			t.Errorf("%s: source disappeared", ref)
			continue
		}
		if len(got) != len(ids) {
			t.Errorf("%s: chunk count changed %d -> %d", ref, len(ids), len(got))
			continue
		}
		for i := range ids {
			if ids[i] != got[i] {
				t.Errorf("%s: chunk ids were rewritten; every edge touching them was "+
					"deleted with them", ref)
				break
			}
		}
	}
}

// TestReingestTouchedButUnchanged covers the middle case: the file's stamps moved
// but its bytes did not, which is what a sync tool or a save-with-no-edit produces.
func TestReingestTouchedButUnchanged(t *testing.T) {
	root := writeVault(t)
	sys := newIncrementalSystem(t, root)

	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	before := chunkIDsByRef(t, sys.Store)

	touched := filepath.Join(root, "Retrieval.md")
	future := time.Now().Add(2 * time.Minute)
	if err := os.Chtimes(touched, future, future); err != nil {
		t.Fatal(err)
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())

	if rep.FilesSkippedByHash != 1 {
		t.Errorf("FilesSkippedByHash = %d, want 1 — a touched file must be read once "+
			"and then recognised by its hash", rep.FilesSkippedByHash)
	}
	if rep.FilesIngested != 0 {
		t.Errorf("re-chunked %d files whose content did not change", rep.FilesIngested)
	}
	assertSameChunkIDs(t, before, chunkIDsByRef(t, sys.Store), nil)

	// The stamps must have been refreshed, or every future scan pays the read again.
	third, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.BytesRead != 0 || third.FilesSkippedByStat != third.FilesSeen {
		t.Errorf("the touched file was not stamp-refreshed: third scan read %d bytes and "+
			"skipped %d/%d by stat", third.BytesRead, third.FilesSkippedByStat, third.FilesSeen)
	}
}

// TestReingestEditedFileOnly checks that an edit re-ingests exactly one file.
func TestReingestEditedFileOnly(t *testing.T) {
	root := writeVault(t)
	sys := newIncrementalSystem(t, root)

	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	before := chunkIDsByRef(t, sys.Store)

	edited := filepath.Join(root, "Retrieval.md")
	body, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	added := string(body) + "\n## Expansion\n\nThe walk now reaches linked notes.\n"
	if err := os.WriteFile(edited, []byte(added), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())

	if rep.FilesIngested != 1 {
		t.Errorf("FilesIngested = %d, want 1", rep.FilesIngested)
	}
	if rep.BytesRead != int64(len(added)) {
		t.Errorf("read %d bytes, want %d — only the edited file should be opened",
			rep.BytesRead, len(added))
	}
	if len(rep.ChangedRefs) != 1 || rep.ChangedRefs[0] != "Retrieval.md" {
		t.Errorf("ChangedRefs = %v, want [Retrieval.md]", rep.ChangedRefs)
	}
	if len(rep.ChangedSourceIDs) != 1 {
		t.Errorf("ChangedSourceIDs = %v, want one id", rep.ChangedSourceIDs)
	}
	if !rep.Incremental() {
		t.Error("Incremental() is false on a one-file edit")
	}
	assertSameChunkIDs(t, before, chunkIDsByRef(t, sys.Store),
		map[string]bool{"Retrieval.md": true})
}

// TestReingestDeletesRemovedNotes covers the half of incrementality that is easy to
// forget: a note deleted from the vault must not stay searchable.
func TestReingestDeletesRemovedNotes(t *testing.T) {
	root := writeVault(t)
	sys := newIncrementalSystem(t, root)

	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{}); err != nil {
		t.Fatal(err)
	}

	// Retrieval.md is a link target from two other notes, so its removal exercises
	// inbound-edge cleanup as well.
	gone := "Retrieval.md"
	src, found, err := sys.Store.FindSource(store.SourceDirectory, gone)
	if err != nil || !found {
		t.Fatalf("%s not ingested: %v", gone, err)
	}
	doomed := map[string]bool{}
	chunks, _ := sys.Store.GetChunksBySource(src.ID)
	for _, c := range chunks {
		doomed[c.ID] = true
	}
	if err := os.Remove(filepath.Join(root, gone)); err != nil {
		t.Fatal(err)
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())

	if rep.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", rep.FilesDeleted)
	}
	if _, found, _ := sys.Store.FindSource(store.SourceDirectory, gone); found {
		t.Error("the deleted note's source survived the sweep")
	}
	for _, e := range edgeSet(t, sys.Store) {
		if doomed[e.SrcChunkID] || doomed[e.DstChunkID] {
			t.Errorf("edge %+v references a chunk from the deleted note", e)
		}
	}
	// Its own links must go too, or an edge rebuild would resurrect edges from a
	// note that no longer exists.
	links, err := sys.Store.LinksTouching([]string{gone})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.FromRef == gone {
			t.Errorf("link from the deleted note survived: %+v", l)
		}
	}
}

// TestSweepIgnoresOtherRoots guards a data-loss bug rather than a staleness one: a
// scan of one vault must never delete another vault's memory just because those
// notes are absent from this walk.
func TestSweepIgnoresOtherRoots(t *testing.T) {
	vaultA := writeVault(t)
	vaultB := t.TempDir()
	if err := os.WriteFile(filepath.Join(vaultB, "Other.md"),
		[]byte("# Other vault\n\nEntirely unrelated notes.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sys := newIncrementalSystem(t, vaultA)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), vaultA, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.Ingester.IngestDirectory(context.Background(), vaultB, nil); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := sys.Store.FindSource(store.SourceDirectory, "Other.md"); !found {
		t.Fatal("vault B was not ingested")
	}

	// Re-scanning vault A must leave vault B alone.
	rep, err := sys.Ingester.IngestDirectory(context.Background(), vaultA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesDeleted != 0 {
		t.Errorf("re-scanning vault A deleted %d sources; it must only sweep its own "+
			"root", rep.FilesDeleted)
	}
	if _, found, _ := sys.Store.FindSource(store.SourceDirectory, "Other.md"); !found {
		t.Error("re-scanning vault A deleted vault B's memory")
	}
}

// TestIncrementalRebuildRestoresInboundLinkEdges is the correctness trap this whole
// mechanism exists for.
//
// Re-ingesting a note replaces its chunks, which deletes every edge touching them —
// including the link edges *pointing at* it from notes that were not re-ingested.
// Without the persisted link table those edges are gone for good, and the graph
// erodes a little with every edit while every counter still reads as healthy.
func TestIncrementalRebuildRestoresInboundLinkEdges(t *testing.T) {
	root := writeVault(t)
	sys := newIncrementalSystem(t, root)

	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{}); err != nil {
		t.Fatal(err)
	}

	// Count the link edges pointing at Retrieval.md before the edit.
	countInbound := func(target string) int {
		idx := indexChunks(t, sys.Store)
		n := 0
		for _, e := range edgeSet(t, sys.Store) {
			if e.Kind == EdgeLink && idx.ref[e.DstChunkID] == target {
				n++
			}
		}
		return n
	}
	target := "Retrieval.md"
	beforeInbound := countInbound(target)
	if beforeInbound == 0 {
		t.Fatalf("no link edges point at %s; the fixture cannot exercise this", target)
	}

	// Edit the *target*. The notes linking to it are untouched, so a naive rebuild
	// would never revisit them.
	path := filepath.Join(root, target)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body,
		[]byte("\n## Expansion\n\nAdded in an edit.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesIngested != 1 {
		t.Fatalf("expected exactly the target to be re-ingested, got %d", rep.FilesIngested)
	}

	// The edit deleted the inbound edges; confirm that, so the test proves the
	// rebuild is doing the work rather than the edges never having been at risk.
	if n := countInbound(target); n != 0 {
		t.Errorf("inbound link edges survived the chunk replacement (%d); this test no "+
			"longer exercises the case it exists for", n)
	}

	er, err := sys.BuildEdgesIncremental(context.Background(), rep, EdgeParams{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(er.String())

	if n := countInbound(target); n < beforeInbound {
		t.Errorf("inbound link edges after the incremental rebuild: %d, want at least %d "+
			"— backlinks from notes that were not re-ingested were lost", n, beforeInbound)
	}
}

// TestSchemaMigrationIsIdempotent covers the ALTER-based migration: reopening a
// database must not fail or duplicate columns, since it runs on every Open.
func TestSchemaMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	for i := 0; i < 3; i++ {
		s, err := store.OpenAt(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		// Write through the new columns each time, so a missing column would surface
		// as an error rather than as a silently ignored field.
		if _, err := s.ReplaceSource(store.MemorySource{
			SourceType: store.SourceConversation, SourceRef: "s#1-2",
			SessionID: "s", ContentHash: "h", FileSize: 123,
		}, []store.MemoryChunk{{
			Text: "hello", ThreadContext: "thread", CotContext: "cot", TokenCount: 1,
		}}); err != nil {
			t.Fatalf("write on open %d: %v", i, err)
		}
		got, err := s.ChunksMissingEmbedding(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("open %d: got %d chunks, want 1", i, len(got))
		}
		if got[0].ThreadContext != "thread" || got[0].CotContext != "cot" {
			t.Errorf("open %d: contexts did not round-trip: %+v", i, got[0])
		}
		src, found, err := s.FindSource(store.SourceConversation, "s#1-2")
		if err != nil || !found {
			t.Fatalf("open %d: source missing: %v", i, err)
		}
		if src.FileSize != 123 {
			t.Errorf("open %d: FileSize = %d, want 123", i, src.FileSize)
		}
		s.Close()
	}
}
