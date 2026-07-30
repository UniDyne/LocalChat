package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"simple-cot-chat/store"
)

// turnMsgs builds a session's message rows the way SendChat persists them: user,
// optional cot, any tool rows, then assistant.
func turnMsgs(rows ...store.StoredMessage) []store.StoredMessage {
	for i := range rows {
		rows[i].Seq = i + 1
	}
	return rows
}

func msg(role, content string) store.StoredMessage {
	return store.StoredMessage{Role: role, Content: content}
}

func planMsg(content string) store.StoredMessage {
	return store.StoredMessage{Role: "user", Content: content, ToolName: "manage_plan"}
}

// TestExtractTurnPairs covers every exclusion rule in §3.3 at once, since they only
// matter in combination: a real session interleaves all of these.
func TestExtractTurnPairs(t *testing.T) {
	msgs := turnMsgs(
		msg("user", "How does the fusion scorer weight its signals?"),
		msg("cot", "The user is asking about retrieval weights; relevant context is the fusion layer."),
		msg("tool", "search_memory returned 3 excerpts about weights"),
		msg("assistant", "It uses a weighted sum of four min-max normalized signals."),

		// A plan-driven turn: synthetic user text, must be skipped.
		planMsg("Working on step 2: implement the walk"),
		msg("assistant", "Done, the walk is implemented."),

		// A normal terse follow-up.
		msg("user", "yes, do that one"),
		msg("assistant", "Applying the higher vector weight."),

		// An in-flight turn with no reply yet.
		msg("user", "what about RRF?"),
	)

	pairs, skips := ExtractTurnPairs("sess-1", "Fusion weights", msgs)

	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want 2: %+v", len(pairs), pairs)
	}
	if len(skips) != 2 {
		t.Errorf("got %d skips, want 2 (plan turn, unanswered turn): %+v", len(skips), skips)
	}

	first := pairs[0]
	if !strings.Contains(first.UserText, "fusion scorer") {
		t.Errorf("first pair user text = %q", first.UserText)
	}
	if !strings.Contains(first.Reply, "weighted sum") {
		t.Errorf("first pair reply = %q", first.Reply)
	}
	if !strings.Contains(first.CoT, "retrieval weights") {
		t.Errorf("first pair should carry the turn's cot row, got %q", first.CoT)
	}

	// The tool row must not appear anywhere in the ingestable text. Indexing it would
	// feed retrieval output back into the corpus (§3.3).
	text := TurnText(first)
	if strings.Contains(text, "search_memory returned") {
		t.Error("tool row content leaked into the turn text")
	}
	if strings.Contains(text, "retrieval weights") {
		t.Error("cot content leaked into the returnable turn text; it is indexed, not returned")
	}

	// The terse turn carries the previous turn's gist, which is what makes it
	// findable at all.
	second := pairs[1]
	if second.UserText != "yes, do that one" {
		t.Errorf("second pair user text = %q", second.UserText)
	}
	if second.PrevGist == "" {
		t.Error("a terse turn must carry the previous turn's gist")
	}
	if !strings.Contains(threadContext(second), "Fusion weights") {
		t.Errorf("thread context missing the session title: %q", threadContext(second))
	}

	// A plan-driven turn's reply must not be attached to the preceding real turn.
	for _, p := range pairs {
		if strings.Contains(p.Reply, "the walk is implemented") {
			t.Error("a plan-driven turn's reply was attached to a real turn")
		}
	}
}

// TestTurnRefRangeIsStable checks the source_ref shape, which the session cascade
// and incremental skip both depend on.
func TestTurnRefRangeIsStable(t *testing.T) {
	msgs := turnMsgs(
		msg("user", "first question here"),
		msg("cot", "some reasoning"),
		msg("assistant", "first answer here"),
	)
	pairs, _ := ExtractTurnPairs("sess", "Title", msgs)
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs", len(pairs))
	}
	want := TurnRef("sess", 1, 3)
	if got := TurnRef("sess", pairs[0].StartSeq, pairs[0].EndSeq); got != want {
		t.Errorf("ref = %q, want %q", got, want)
	}
}

func newConvSystem(t *testing.T) *System {
	t.Helper()
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })
	return sys
}

// TestIngestTurnsStoresAndSkips covers storage, the append-only property, and the
// unchanged skip.
func TestIngestTurnsStoresAndSkips(t *testing.T) {
	sys := newConvSystem(t)
	msgs := turnMsgs(
		msg("user", "Where does the ingestion queue live?"),
		msg("assistant", "In memory/queue.go, drained by exactly one worker."),
	)

	rep, err := sys.Ingester.IngestTurns(context.Background(), "sess-1", "Queue design", msgs)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())
	if rep.TurnsIngested != 1 || rep.ChunksWritten == 0 {
		t.Fatalf("expected one turn and some chunks, got %+v", rep)
	}

	// Re-running with the same messages must be a no-op.
	again, err := sys.Ingester.IngestTurns(context.Background(), "sess-1", "Queue design", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if again.TurnsIngested != 0 || again.TurnsUnchanged != 1 {
		t.Errorf("re-ingest was not a no-op: %+v", again)
	}

	// A second turn adds a source without re-chunking the first — the append-only
	// property that per-turn-pair chunking exists for.
	before := chunkIDsByRef(t, sys.Store)
	msgs = append(msgs, turnMsgs(
		msg("user", "Where does the ingestion queue live?"),
		msg("assistant", "In memory/queue.go, drained by exactly one worker."),
		msg("user", "and what drains it?"),
		msg("assistant", "A single background goroutine started by NewQueue."),
	)[2:]...)
	for i := range msgs {
		msgs[i].Seq = i + 1
	}
	third, err := sys.Ingester.IngestTurns(context.Background(), "sess-1", "Queue design", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if third.TurnsIngested != 1 {
		t.Errorf("second turn: TurnsIngested = %d, want 1 (%+v)", third.TurnsIngested, third)
	}
	if third.TurnsUnchanged != 1 {
		t.Errorf("the first turn should have been skipped as unchanged: %+v", third)
	}
	assertSameChunkIDs(t, before, chunkIDsByRef(t, sys.Store), nil)
}

// TestCotIsIndexedNotReturned is the invariant that keeps §3.3's architectural
// compromise honest: CoT makes a terse turn findable, but never comes back.
//
// ARCHITECTURE.md deliberately keeps the model's own past reasoning out of replayed
// history. Returning CoT through memory would route around that decision by a side
// door, so the rule is not a preference — it is upholding an existing design choice.
func TestCotIsIndexedNotReturned(t *testing.T) {
	s := openTestStore(t)
	// A real embedder is not needed: the assertion is about which text is *indexed*
	// (terms) and which is *returned* (stored text), and BM25 covers the first.
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	// The distinctive word appears only in the cot row.
	msgs := turnMsgs(
		msg("user", "yes, that one"),
		msg("cot", "The user is confirming the choice of zzyzxphase for the rollout window."),
		msg("assistant", "Confirmed, I'll proceed with that option."),
	)
	if _, err := sys.Ingester.IngestTurns(context.Background(), "sess-1", "Rollout", msgs); err != nil {
		t.Fatal(err)
	}

	results, _, err := sys.Search(context.Background(), "zzyzxphase rollout window",
		SearchOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("a term appearing only in the cot row did not make the turn findable; " +
			"CoT is not being indexed")
	}
	for _, r := range results {
		if strings.Contains(strings.ToLower(r.Text), "zzyzxphase") {
			t.Errorf("cot content was returned to the model: %q", truncate(r.Text, 120))
		}
		if !strings.Contains(r.Text, "yes, that one") {
			t.Errorf("the returned text should be the exchange itself, got %q", truncate(r.Text, 120))
		}
	}

	// And the stored row must carry it in the embed-only column, which is what the
	// backfill reads.
	pending, err := sys.Store.ChunksMissingEmbedding(10)
	if err != nil {
		t.Fatal(err)
	}
	var sawCot bool
	for _, c := range pending {
		if strings.Contains(c.CotContext, "zzyzxphase") {
			sawCot = true
			if strings.Contains(c.Text, "zzyzxphase") {
				t.Error("cot content is also in the returnable text column")
			}
		}
	}
	if !sawCot {
		t.Error("cot_context was not persisted; the backfill would embed without it")
	}
}

// TestConversationEmbeddedTextIncludesContexts checks that the prefixes actually
// reach the embedder, which is the only reason to store them.
func TestConversationEmbeddedTextIncludesContexts(t *testing.T) {
	sys := newConvSystem(t)
	msgs := turnMsgs(
		msg("user", "and the second one?"),
		msg("cot", "They mean the second candidate weighting from earlier."),
		msg("assistant", "The second uses 0.6 on the vector signal."),
	)
	if _, err := sys.Ingester.IngestTurns(context.Background(), "s", "Weight sweep", msgs); err != nil {
		t.Fatal(err)
	}
	pending, err := sys.Store.ChunksMissingEmbedding(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("no chunks stored")
	}

	tc := NewHeuristicCounter()
	c := pending[0]
	embedded := BuildEmbeddedText(c.HeadingPath, c.ThreadContext, c.CotContext, c.Text, tc)
	for _, want := range []string{"Weight sweep", "second candidate weighting", "0.6 on the vector"} {
		if !strings.Contains(embedded, want) {
			t.Errorf("embedded text missing %q:\n%s", want, embedded)
		}
	}
	// The 512-token invariant must hold with all three prefixes present.
	if n := tc.Count(embedded); n > 512 {
		t.Errorf("embedded text is %d tokens, over the model's hard 512 limit", n)
	}
}

// TestArtifactIngestion covers the third source type.
func TestArtifactIngestion(t *testing.T) {
	sys := newConvSystem(t)
	art := store.Artifact{
		ID: "art-1", SessionID: "sess-1", Title: "Migration plan", ContentType: "markdown",
		Content: "# Migration plan\n\n## Rollback\n\nRestore the snapshot and flip traffic back.\n",
	}
	n, err := sys.Ingester.IngestArtifact(context.Background(), art)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no chunks written for the artifact")
	}

	// Unchanged re-ingest is a no-op.
	again, err := sys.Ingester.IngestArtifact(context.Background(), art)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-ingesting an unchanged artifact wrote %d chunks", again)
	}

	// Edited content replaces rather than duplicates.
	art.Content += "\n## Verification\n\nCheck the quorum before draining.\n"
	if _, err := sys.Ingester.IngestArtifact(context.Background(), art); err != nil {
		t.Fatal(err)
	}
	src, found, err := sys.Store.FindSource(store.SourceArtifact, "art-1")
	if err != nil || !found {
		t.Fatalf("artifact source missing: %v", err)
	}
	if src.SessionID != "sess-1" {
		t.Errorf("SessionID = %q; without it the session cascade cannot reach this source", src.SessionID)
	}

	results, _, err := sys.Search(context.Background(), "rollback restore snapshot flip traffic",
		SearchOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var hit *Result
	for i := range results {
		if results[i].SourceType == store.SourceArtifact {
			hit = &results[i]
		}
	}
	if hit == nil {
		t.Fatal("the artifact is not searchable")
	}
	if !strings.Contains(hit.HeadingPath, "Migration plan") {
		t.Errorf("heading path should situate the excerpt in its artifact, got %q", hit.HeadingPath)
	}
}

// TestSourceTypeFilterSeparatesSources checks the tool's source_types filter now
// that all three kinds coexist — with conversations auto-ingesting, a query that
// wanted notes could otherwise be swamped by chat.
func TestSourceTypeFilterSeparatesSources(t *testing.T) {
	root := writeVault(t)
	sys := newConvSystem(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.Ingester.IngestTurns(context.Background(), "s", "Architecture chat", turnMsgs(
		msg("user", "Where are sessions and messages stored?"),
		msg("assistant", "In an embedded DuckDB database, see the data model."),
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
		ID: "a1", SessionID: "s", Title: "Schema notes",
		Content: "# Schema notes\n\nThe chunks table stores retrievable spans in DuckDB.\n",
	}); err != nil {
		t.Fatal(err)
	}

	const query = "duckdb chunks sessions storage"
	all, _, err := sys.Search(context.Background(), query, SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, r := range all {
		kinds[r.SourceType] = true
	}
	if len(kinds) < 2 {
		t.Errorf("expected results from several source types, got %v", kinds)
	}

	for _, want := range []string{store.SourceDirectory, store.SourceConversation, store.SourceArtifact} {
		got, _, err := sys.Search(context.Background(), query,
			SearchOptions{Limit: 10, SourceTypes: []string{want}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Errorf("no results for source type %q", want)
		}
		for _, r := range got {
			if r.SourceType != want {
				t.Errorf("filter %q returned a %q result", want, r.SourceType)
			}
		}
	}
}

// TestExcludeSessionFiltersOwnConversation covers the filter that keeps a search
// from returning the conversation it is running inside.
//
// Only a problem since Phase 7: with conversations ingesting automatically, the
// current session's turns are in memory as well as in the model's context, so a
// search would spend its token budget restating what the model can already see.
func TestExcludeSessionFiltersOwnConversation(t *testing.T) {
	sys := newConvSystem(t)

	// Deliberately different wording per session: two near-identical turns would be
	// collapsed by retrieval dedup, and the test would then be measuring that instead.
	turns := map[string][]store.StoredMessage{
		"current": turnMsgs(
			msg("user", "what is the retention horizon?"),
			msg("assistant", "Ninety days, after which the purge job removes cold rows.")),
		"other": turnMsgs(
			msg("user", "remind me how long we keep audit records"),
			msg("assistant", "Retention runs to a quarter, then archival tiering takes over.")),
	}
	for sess, msgs := range turns {
		if _, err := sys.Ingester.IngestTurns(context.Background(), sess, "Chat "+sess, msgs); err != nil {
			t.Fatal(err)
		}
	}

	const query = "retention horizon ninety days purge audit records"
	all, _, err := sys.Search(context.Background(), query, SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("expected both sessions to match, got %d results", len(all))
	}

	filtered, _, err := sys.Search(context.Background(), query,
		SearchOptions{Limit: 10, ExcludeSessionID: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) == 0 {
		t.Fatal("the filter removed everything, including the other session")
	}
	for _, r := range filtered {
		if strings.Contains(r.Text, "purge job removes cold rows") {
			t.Errorf("the excluded session's turn was returned: %q", truncate(r.Text, 100))
		}
	}
	if len(filtered) >= len(all) {
		t.Errorf("filtered result count %d is not below the unfiltered %d", len(filtered), len(all))
	}
}

// TestConversationMemoryDiesWithItsSession checks the cascade end to end, with the
// source_ref shape TurnRef actually produces.
//
// The store-level cascade test uses a hand-written ref. This one closes the gap
// between "the cascade works on the ref I typed" and "the cascade works on the ref
// ingestion generates" — the sort of mismatch that would leave a user's deleted
// conversations searchable.
func TestConversationMemoryDiesWithItsSession(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	sessionID, err := s.CreateSession("Doomed chat")
	if err != nil {
		t.Fatal(err)
	}
	keepID, err := s.CreateSession("Surviving chat")
	if err != nil {
		t.Fatal(err)
	}

	for id, word := range map[string]string{sessionID: "qwixotic", keepID: "flambard"} {
		if _, err := sys.Ingester.IngestTurns(context.Background(), id, "Chat", turnMsgs(
			msg("user", "what was the codename?"),
			msg("assistant", "The codename was "+word+" for that release."))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
		ID: "art-doomed", SessionID: sessionID, Title: "Doomed notes",
		Content: "# Doomed notes\n\nContains the string zephyrine for lookup.\n",
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(sessionID); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"qwixotic", "zephyrine"} {
		results, _, err := sys.Search(context.Background(), gone, SearchOptions{Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			if strings.Contains(r.Text, gone) {
				t.Errorf("memory from the deleted session is still searchable: %q", truncate(r.Text, 100))
			}
		}
	}
	// The other session must be untouched.
	results, _, err := sys.Search(context.Background(), "flambard codename", SearchOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if findText(results, "flambard") == nil {
		t.Error("deleting one session removed another session's memory")
	}
}

// TestReplyQuotingMemoryIsNotReturnedTwice covers the feedback loop §3.3 warns
// about, which only becomes live now that conversations ingest automatically.
//
// An assistant reply routinely quotes retrieved excerpts, so ingesting replies puts
// that text into the corpus a second time. Measured behaviour, and it is worth being
// exact about which guard fires: the **ingest-side** check at 0.9 does *not* catch
// this, because the stored turn is "User: <question>\n\nAssistant: <passage>" and
// the question's framing pulls the 3-gram Dice below the threshold. The chunk is
// stored. What stops the duplicate reaching the model is the **retrieval-side**
// check at 0.85.
//
// That is the design working as specified rather than a gap: §3.5 describes the two
// thresholds as complementary, with retrieval dedup covering pairs "legitimately
// worth storing separately but [which] shouldn't both be returned". This is exactly
// that case. The residual cost is real and accepted — the passage's terms are
// counted twice in the corpus statistics — and it is why the two layers exist rather
// than one.
func TestReplyQuotingMemoryIsNotReturnedTwice(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	passage := "The Leiden chunker builds a similarity graph over sentences, combining " +
		"mutual top-k semantic edges with a positional prior so communities come out " +
		"mostly contiguous, then splits each community into contiguous runs."
	if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
		ID: "src", SessionID: "s", Title: "Chunker notes",
		Content: "# Chunker notes\n\n" + passage + "\n",
	}); err != nil {
		t.Fatal(err)
	}

	before, err := sys.Store.MemoryStats()
	if err != nil {
		t.Fatal(err)
	}

	// A reply that quotes the passage back almost verbatim.
	rep, err := sys.Ingester.IngestTurns(context.Background(), "s", "Chunker chat", turnMsgs(
		msg("user", "how does the leiden chunker work?"),
		msg("assistant", passage)))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())

	after, err := sys.Store.MemoryStats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("chunks %d -> %d", before.Chunks, after.Chunks)

	// The turn is stored — asserted, so a future change that starts dropping it shows
	// up here rather than as quietly missing conversation memory.
	if after.Chunks <= before.Chunks {
		t.Errorf("chunk count did not grow (%d -> %d); the turn should be stored, with "+
			"retrieval dedup — not ingest dedup — preventing the duplicate being returned",
			before.Chunks, after.Chunks)
	}

	// But the passage must come back only once.
	results, rep2, err := sys.Search(context.Background(),
		"leiden chunker similarity graph positional prior contiguous runs",
		SearchOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, r := range results {
		if strings.Contains(r.Text, "mutual top-k semantic edges with a positional prior") {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("the quoted passage came back %d times, want exactly 1 (%d results "+
			"deduped)", copies, rep2.DedupedResults)
	}
}

// TestConcurrentMemoryWritesAndSearches is a regression test for a bug this phase
// created and then found.
//
// Automatic ingestion is the first thing that makes two memory writers routine: the
// queue worker ingests a turn while the chat goroutine searches, and a search
// recomputes and stores BM25 statistics. DuckDB fails the loser of a write-write
// conflict, so the two collided on memory_meta — `Duplicate key "key: stats_dirty"
// violates primary key constraint`, raised by a statement that is *already* an
// upsert, because the conflict is between transactions rather than within one.
//
// Before Phase 7 nothing wrote memory concurrently, so the store's mutex guarding
// only currentSession was sufficient by accident.
func TestConcurrentMemoryWritesAndSearches(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	// Seed something to search.
	root := writeVault(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}

	const workers = 4
	const rounds = 12
	errs := make(chan error, workers*rounds*2)

	// Every goroutine must finish before the test returns. An earlier version of this
	// test slept for a fixed window instead, on the reasoning that a WaitGroup was
	// ceremony for a test whose only assertion is "no error". It was not: the
	// goroutines outlived the test, kept using a store that t.Cleanup had closed, and
	// panicked with "send on closed channel" — but only when run alongside other
	// tests, so it passed in isolation.
	var wg sync.WaitGroup

	// Writers: ingest turns, which marks the stats dirty on every pass.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				msgs := turnMsgs(
					msg("user", fmt.Sprintf("worker %d question %d about retrieval", w, i)),
					msg("assistant", fmt.Sprintf("worker %d answer %d about the scorer", w, i)))
				if _, err := sys.Ingester.IngestTurns(context.Background(),
					fmt.Sprintf("sess-%d", w), "Concurrent", msgs); err != nil {
					errs <- fmt.Errorf("ingest: %w", err)
					return
				}
			}
		}(w)
	}

	// Readers: search, which recomputes the dirty statistics and writes them back.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, _, err := sys.Search(context.Background(), "retrieval scorer duckdb",
					SearchOptions{Limit: 5}); err != nil {
					errs <- fmt.Errorf("search: %w", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		for i, err := range failures {
			if i >= 5 {
				t.Errorf("... and %d more", len(failures)-i)
				break
			}
			t.Errorf("concurrent memory access failed: %v", err)
		}
	}
}

// TestTurnIngestDoesNotBlockTheCaller is the Phase 7 exit criterion for chat turns:
// turn latency must be unchanged — measured, not assumed.
//
// What SendChat pays is the enqueue, not the ingest. This measures the two
// separately: the enqueue must be orders of magnitude cheaper than the work it
// defers, because that difference *is* the guarantee. Asserting a wall-clock budget
// would be flaky on a loaded machine, so the assertion is the ratio plus a generous
// absolute ceiling.
func TestTurnIngestDoesNotBlockTheCaller(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	// A session with enough turns that ingesting it is real work.
	var msgs []store.StoredMessage
	for i := 0; i < 40; i++ {
		msgs = append(msgs,
			msg("user", fmt.Sprintf("Question %d about the retrieval pipeline and its scoring signals", i)),
			msg("cot", fmt.Sprintf("Reasoning note %d covering the fusion layer and candidate generation", i)),
			msg("assistant", fmt.Sprintf("Answer %d: the scorer fuses BM25, vector cosine, entity overlap and character n-grams", i)))
	}
	for i := range msgs {
		msgs[i].Seq = i + 1
	}

	load := func() ([]store.StoredMessage, error) { return msgs, nil }

	start := time.Now()
	queued, err := sys.EnqueueTurnIngest("sess-1", "Retrieval pipeline", load)
	enqueue := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !queued {
		t.Fatal("the job was not queued")
	}

	// The work itself, for comparison — run inline so it is the same work the queue
	// would do.
	start = time.Now()
	rep, err := sys.Ingester.IngestTurns(context.Background(), "sess-2", "Retrieval pipeline", msgs)
	if err != nil {
		t.Fatal(err)
	}
	ingest := time.Since(start)
	if rep.TurnsIngested != 40 {
		t.Fatalf("ingested %d of 40 turns", rep.TurnsIngested)
	}

	t.Logf("enqueue %v vs ingest of %d turns %v (%.0fx)",
		enqueue, rep.TurnsIngested, ingest, float64(ingest)/float64(enqueue))
	fmt.Printf("\n=== Phase 7: chat-turn cost of memory ingestion ===\n")
	fmt.Printf("enqueue: %v   inline ingest of %d turns: %v   ratio: %.0fx\n\n",
		enqueue.Round(time.Microsecond), rep.TurnsIngested,
		ingest.Round(time.Millisecond), float64(ingest)/float64(enqueue))

	if enqueue > 10*time.Millisecond {
		t.Errorf("enqueue took %v; it must be a cheap append, not work", enqueue)
	}
	if ingest < enqueue*20 {
		t.Errorf("ingest (%v) is not meaningfully more expensive than the enqueue (%v); "+
			"either the fixture is too small to prove anything or the enqueue is doing "+
			"work it should be deferring", ingest, enqueue)
	}
}

// TestConcurrentStatsRecomputeUnderLoad is the regression test for a write-write
// conflict that only appears under sustained concurrency.
//
// TestConcurrentMemoryWritesAndSearches (above) catches the coarse version — two
// writers colliding on memory_meta, fixed by the store's write mutex. This catches a
// subtler one the mutex cannot fix: `RecomputeBM25Stats` used to `DELETE FROM
// memory_terms` and then reinsert the same primary keys inside one transaction, and
// DuckDB rejects that at commit with `write-write conflict on key: "<term>"` whenever
// another transaction has touched those keys. Two concurrent searches do exactly that,
// since both recompute when the stats are marked dirty.
//
// It failed at roughly 1 in 2,000 recomputes — often enough to break a full-suite run
// now and then, rare enough to look like flakiness rather than a bug. That is the
// reason this test is deliberately heavy: a lighter one would pass while the bug was
// present, which is worse than having no test.
func TestConcurrentStatsRecomputeUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test; skipped with -short")
	}
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })
	root := writeVault(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}

	const workers, rounds = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*3)

	// Ingesters: each turn marks the corpus statistics dirty.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				msgs := turnMsgs(
					msg("user", fmt.Sprintf("w%d q%d scoring retrieval", w, i)),
					msg("assistant", fmt.Sprintf("w%d a%d scoring fusion", w, i)))
				if _, err := sys.Ingester.IngestTurns(context.Background(),
					fmt.Sprintf("s-%d", w), "T", msgs); err != nil {
					errs <- fmt.Errorf("ingest w%d: %w", w, err)
					return
				}
			}
		}(w)
	}
	// Searchers: each recomputes the dirty statistics and writes them back.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if _, _, err := sys.Search(context.Background(), "scoring retrieval fusion",
					SearchOptions{Limit: 5}); err != nil {
					errs <- fmt.Errorf("search: %w", err)
					return
				}
			}
		}()
	}
	// And direct contention on the recompute itself, which is where the conflict lands.
	for w := 0; w < workers/2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := s.MarkStatsDirty(); err != nil {
					errs <- fmt.Errorf("mark dirty: %w", err)
					return
				}
				if _, _, err := s.RecomputeBM25Stats(); err != nil {
					errs <- fmt.Errorf("recompute: %w", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n < 4 {
			t.Errorf("concurrent stats access failed: %v", err)
		}
		n++
	}
	if n > 0 {
		t.Errorf("total failures: %d", n)
	}
}
