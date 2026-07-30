package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple-cot-chat/store"
)

// stubExtractor returns canned results, keyed by note title, so the whole pass can be
// exercised without a model. The guards, chunk attribution and link resolution are
// where the bugs are, and none of them need a network round trip.
type stubExtractor struct {
	byTitle map[string]ExtractResult
	// fail makes extraction fail for a given title, to exercise the failed-state path.
	fail map[string]bool
	// calls records the requests, so batching and prompt content can be asserted.
	calls []ExtractRequest
}

func newStubExtractor() *stubExtractor {
	return &stubExtractor{byTitle: map[string]ExtractResult{}, fail: map[string]bool{}}
}

func (s *stubExtractor) ModelName() string { return "stub-extractor" }

func (s *stubExtractor) Extract(_ context.Context, req ExtractRequest) (ExtractResult, error) {
	s.calls = append(s.calls, req)
	if s.fail[req.Title] {
		return ExtractResult{}, errors.New("simulated extraction failure")
	}
	return s.byTitle[req.Title], nil
}

// ---------- the guards ----------

// TestGuardRejectsHallucinations is the safety property that matters most: an
// invented entity is worse than a missed one, because it pollutes the entity signal
// and wires unrelated notes together.
//
// The guard is a pure function precisely so every failure shape can be enumerated
// here without a model or a database.
func TestGuardRejectsHallucinations(t *testing.T) {
	text := "The Acme Corp migration ran on 2026-07-14. Owner: Dana Whitfield. " +
		"Config lives at conf/deploy.yaml and the budget was 42000 units."

	resolver := NewLinkResolver()
	resolver.AddNote("Deployment.md", nil)

	res := ExtractResult{
		Entities: []ExtractedEntity{
			// Accepted: all present verbatim.
			{Kind: "org", Value: "Acme Corp"},
			{Kind: "person", Value: "Dana Whitfield"},
			{Kind: "date", Value: "2026-07-14"},
			{Kind: "path", Value: "conf/deploy.yaml"},
			{Kind: "number", Value: "42000"},
			// Case-insensitive match is fine — the same string, differently cased.
			{Kind: "org", Value: "ACME CORP"},

			// Rejected: not in the text at all. The classic confabulation.
			{Kind: "person", Value: "Robert Nakamura"},
			{Kind: "org", Value: "Globex"},
			// Rejected: a plausible-looking expansion of something that IS in the text.
			// This is the subtle case — the guard has to be literal, not charitable.
			{Kind: "org", Value: "Acme Corporation"},
			// Rejected: invented kind.
			{Kind: "vehicle", Value: "migration"},
			// Rejected: empty.
			{Kind: "org", Value: "   "},
			// Rejected: a sentence, not an entity.
			{Kind: "proper", Value: strings.Repeat("x", MaxEntityValueChars+1)},
		},
		Links: []ExtractedLink{
			// Accepted: target resolves, evidence present.
			{Target: "Deployment", Evidence: "the budget was 42000 units"},
			// Rejected: target is not a real note.
			{Target: "Nonexistent Note", Evidence: "Owner: Dana Whitfield"},
			// Rejected: evidence not in the text — the hallucination guard for links.
			{Target: "Deployment", Evidence: "the rollback was cancelled"},
			// Rejected: no evidence at all.
			{Target: "Deployment", Evidence: ""},
		},
	}

	entities, links, rep := GuardExtraction(text, resolver, "Other.md", res)

	// "ACME CORP" normalizes onto "Acme Corp", so six proposals become five entities.
	if rep.EntitiesAccepted != 5 {
		t.Errorf("EntitiesAccepted = %d, want 5: %+v", rep.EntitiesAccepted, entities)
	}
	if rep.RejectedNotVerbatim != 3 {
		t.Errorf("RejectedNotVerbatim = %d, want 3 (two invented, one over-expanded)",
			rep.RejectedNotVerbatim)
	}
	if rep.RejectedBadKind != 1 {
		t.Errorf("RejectedBadKind = %d, want 1", rep.RejectedBadKind)
	}
	if rep.RejectedEmpty != 1 {
		t.Errorf("RejectedEmpty = %d, want 1", rep.RejectedEmpty)
	}
	if rep.RejectedTooLong != 1 {
		t.Errorf("RejectedTooLong = %d, want 1", rep.RejectedTooLong)
	}

	for _, e := range entities {
		if e.Extractor != ExtractorLLM {
			t.Errorf("entity %q recorded extractor %q; provenance is what makes the tier "+
				"rollback-able", e.ValueNorm, e.Extractor)
		}
		if e.ValueNorm != strings.ToLower(e.ValueNorm) {
			t.Errorf("entity %q was not normalized", e.ValueNorm)
		}
		if strings.Contains(e.ValueNorm, "nakamura") || strings.Contains(e.ValueNorm, "globex") ||
			e.ValueNorm == "acme corporation" {
			t.Errorf("a hallucinated entity survived the guard: %q", e.ValueNorm)
		}
	}

	if len(links) != 1 || links[0].ToRef != "Deployment.md" {
		t.Errorf("links = %+v, want one to Deployment.md", links)
	}
	if rep.LinksUnresolved != 1 {
		t.Errorf("LinksUnresolved = %d, want 1", rep.LinksUnresolved)
	}
	if rep.LinksNoEvidence != 2 {
		t.Errorf("LinksNoEvidence = %d, want 2", rep.LinksNoEvidence)
	}
	t.Logf("guards: %+v", rep)
}

// TestGuardRejectsSelfLink checks that a note proposing itself is dropped: a self
// edge carries no information for a walk and would waste a hop.
func TestGuardRejectsSelfLink(t *testing.T) {
	resolver := NewLinkResolver()
	resolver.AddNote("Deployment.md", nil)
	_, links, rep := GuardExtraction("the deployment runs nightly", resolver, "Deployment.md",
		ExtractResult{Links: []ExtractedLink{
			{Target: "Deployment", Evidence: "the deployment runs nightly"},
		}})
	if len(links) != 0 {
		t.Errorf("a self-link was accepted: %+v", links)
	}
	if rep.LinksSelf != 1 {
		t.Errorf("LinksSelf = %d, want 1", rep.LinksSelf)
	}
}

// TestGuardCaps checks the volume caps, which stop a model that enumerates every
// capitalized word from swamping the entity table.
func TestGuardCaps(t *testing.T) {
	var b strings.Builder
	var res ExtractResult
	for i := 0; i < MaxEntitiesPerSource+20; i++ {
		v := fmt.Sprintf("Entity%03d", i)
		fmt.Fprintf(&b, "%s appears here. ", v)
		res.Entities = append(res.Entities, ExtractedEntity{Kind: "proper", Value: v})
	}
	entities, _, rep := GuardExtraction(b.String(), NewLinkResolver(), "x", res)
	if len(entities) != MaxEntitiesPerSource {
		t.Errorf("accepted %d entities, want the cap of %d", len(entities), MaxEntitiesPerSource)
	}
	if rep.RejectedOverCap != 20 {
		t.Errorf("RejectedOverCap = %d, want 20", rep.RejectedOverCap)
	}
}

// TestHallucinationRate checks the health metric that decides whether a given
// extraction model is good enough for this job.
func TestHallucinationRate(t *testing.T) {
	rep := EnrichReport{Guards: GuardReport{EntitiesProposed: 40, RejectedNotVerbatim: 6}}
	if got := rep.HallucinationRate(); got < 0.149 || got > 0.151 {
		t.Errorf("HallucinationRate = %v, want 0.15", got)
	}
	if (EnrichReport{}).HallucinationRate() != 0 {
		t.Error("an empty report should not divide by zero")
	}
}

// ---------- the pass ----------

func newEnrichSystem(t *testing.T, ext EntityExtractor) (*System, string) {
	t.Helper()
	s := openTestStore(t)
	sys := NewSystem(s, Config{
		ModelDir:  filepath.Join(t.TempDir(), "absent"),
		Extractor: ext,
	}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	root := writeVault(t)
	if _, err := sys.Ingester.IngestDirectory(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.BuildEdges(context.Background(), sys.Ingester.Links, EdgeParams{}); err != nil {
		t.Fatal(err)
	}
	return sys, root
}

// entityValuesByExtractor reads back which entities each tier produced.
func entityValuesByExtractor(t *testing.T, s *store.Store) map[string][]string {
	t.Helper()
	rows, err := s.DB().Query(`
		SELECT ce.extractor, e.value_norm
		FROM memory_chunk_entities ce
		JOIN memory_entities e ON e.id = ce.entity_id
		ORDER BY ce.extractor, e.value_norm`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var ex, v string
		if err := rows.Scan(&ex, &v); err != nil {
			t.Fatal(err)
		}
		out[ex] = append(out[ex], v)
	}
	return out
}

// TestEnrichAddsWithoutDisturbingOtherTiers is the core property of a second pass:
// it must add to what ingestion produced, never replace it.
func TestEnrichAddsWithoutDisturbingOtherTiers(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)

	// Values that genuinely appear in the vault fixture.
	ext.byTitle["Architecture"] = ExtractResult{
		Entities: []ExtractedEntity{
			{Kind: "code", Value: "DuckDB"},
			{Kind: "proper", Value: "heading path"},
			{Kind: "person", Value: "Someone Who Is Not There"}, // must be rejected
		},
	}

	before := entityValuesByExtractor(t, sys.Store)
	if len(before["heuristic"]) == 0 {
		t.Fatal("the fixture produced no heuristic entities; this test needs them")
	}

	rep, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())
	if rep.SourcesDone == 0 {
		t.Fatal("no sources enriched")
	}
	if rep.Guards.RejectedNotVerbatim == 0 {
		t.Error("the invented entity was not rejected")
	}

	after := entityValuesByExtractor(t, sys.Store)
	if len(after["heuristic"]) != len(before["heuristic"]) {
		t.Errorf("heuristic associations changed from %d to %d; the LLM pass must add, "+
			"not replace", len(before["heuristic"]), len(after["heuristic"]))
	}
	if len(after["tag"]) != len(before["tag"]) {
		t.Errorf("tag associations changed from %d to %d", len(before["tag"]), len(after["tag"]))
	}
	if len(after[ExtractorLLM]) == 0 {
		t.Fatal("no llm associations were written")
	}
	for _, v := range after[ExtractorLLM] {
		if strings.Contains(v, "not there") {
			t.Errorf("a rejected entity reached the database: %q", v)
		}
	}
}

// TestEnrichAttributesEntitiesToContainingChunks checks that an entity lands only on
// chunks that actually contain it.
//
// The entity signal answers "does this chunk mention X". A chunk that does not contain
// the string cannot honestly claim it, and attaching a note-level entity to every
// chunk of the note would make the signal meaningless on long notes.
func TestEnrichAttributesEntitiesToContainingChunks(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)

	// "Fusion" appears in one section of Retrieval.md, "Graph walk" in another.
	ext.byTitle["Retrieval"] = ExtractResult{
		Entities: []ExtractedEntity{
			{Kind: "proper", Value: "Fusion"},
			{Kind: "proper", Value: "Graph walk"},
		},
	}
	if _, err := sys.Enrich(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}

	src, found, err := sys.Store.FindSource(store.SourceDirectory, "Retrieval.md")
	if err != nil || !found {
		t.Fatal("Retrieval.md missing")
	}
	chunks, err := sys.Store.GetChunksBySource(src.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range chunks {
		rows, err := sys.Store.DB().Query(`
			SELECT e.value_norm FROM memory_chunk_entities ce
			JOIN memory_entities e ON e.id = ce.entity_id
			WHERE ce.chunk_id = ? AND ce.extractor = ?`, c.ID, ExtractorLLM)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToLower(c.Text), v) {
				t.Errorf("entity %q attached to a chunk that does not contain it: %q",
					v, truncate(c.Text, 80))
			}
		}
		rows.Close()
	}
}

// TestEnrichWritesInferredLinkEdges covers the pass's highest-value output for a
// vault without disciplined cross-referencing.
func TestEnrichWritesInferredLinkEdges(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)

	// Broken Links.md talks about Duplicate without a resolvable link. Propose a
	// cross-reference to a note that does exist, with evidence from the note itself.
	ext.byTitle["Broken Links"] = ExtractResult{
		Links: []ExtractedLink{
			{Target: "Retrieval", Evidence: "This references"},
			// An invented target must be dropped and counted.
			{Target: "No Such Target At All", Evidence: "This references"},
		},
	}

	rep, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())
	if rep.Guards.LinksAccepted != 1 {
		t.Errorf("LinksAccepted = %d, want 1", rep.Guards.LinksAccepted)
	}
	if rep.Guards.LinksUnresolved != 1 {
		t.Errorf("LinksUnresolved = %d, want 1 — an unresolvable target must be dropped "+
			"and counted, never guessed at", rep.Guards.LinksUnresolved)
	}
	if rep.EdgesWritten == 0 {
		t.Fatal("no inferred_link edges written")
	}

	idx := indexChunks(t, sys.Store)
	var found bool
	for _, e := range edgeSet(t, sys.Store) {
		if e.Kind != EdgeInferredLink {
			continue
		}
		found = true
		if idx.ref[e.SrcChunkID] != "Broken Links.md" || idx.ref[e.DstChunkID] != "Retrieval.md" {
			t.Errorf("inferred edge goes %s -> %s, want Broken Links.md -> Retrieval.md",
				idx.ref[e.SrcChunkID], idx.ref[e.DstChunkID])
		}
		if e.Weight != InferredLinkWeight {
			t.Errorf("inferred link weight = %v, want %v", e.Weight, InferredLinkWeight)
		}
	}
	if !found {
		t.Error("no edge of kind inferred_link exists")
	}

	// It must rank below an authored link at every stage: lower stored weight AND
	// lower decay in the walk. Ranking a guess above a human assertion would be
	// dishonest about which signal is better evidenced.
	if InferredLinkWeight >= DefaultEdgeParams().LinkWeight {
		t.Errorf("inferred weight %v is not below the authored link weight %v",
			InferredLinkWeight, DefaultEdgeParams().LinkWeight)
	}
	decay := DefaultDecay()
	if decay[EdgeInferredLink] >= decay[EdgeLink] || decay[EdgeInferredLink] >= decay[EdgeSimilar] {
		t.Errorf("inferred_link decay %v must sit below link (%v) and similar (%v)",
			decay[EdgeInferredLink], decay[EdgeLink], decay[EdgeSimilar])
	}
}

// TestEnrichIsResumable is what makes the pass survive a closed laptop: an
// interrupted run must continue rather than restart.
func TestEnrichIsResumable(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)

	total, err := sys.Store.SourcesPendingEnrichment(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(total) < 4 {
		t.Fatalf("need several pending sources, got %d", len(total))
	}

	first, err := sys.Enrich(context.Background(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourcesTried != 2 {
		t.Errorf("a limited run tried %d sources, want 2", first.SourcesTried)
	}

	states, err := sys.Store.CountSourcesByEntityPass()
	if err != nil {
		t.Fatal(err)
	}
	if states[store.EntityPassDone] != 2 {
		t.Errorf("done = %d, want 2 after a limited run: %v", states[store.EntityPassDone], states)
	}

	// The next run must pick up the rest, not redo the first two.
	calls := len(ext.calls)
	second, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.SourcesTried != len(total)-2 {
		t.Errorf("second run tried %d sources, want %d", second.SourcesTried, len(total)-2)
	}
	if got := len(ext.calls) - calls; got != len(total)-2 {
		t.Errorf("second run made %d extraction calls, want %d — already-done sources "+
			"must not be re-extracted", got, len(total)-2)
	}

	// A third run has nothing to do at all.
	third, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.SourcesTried != 0 {
		t.Errorf("a completed corpus still had %d sources to try", third.SourcesTried)
	}
}

// TestEnrichMarksFailuresAndContinues checks that one bad note does not stall the
// pass, and does not silently retry forever.
func TestEnrichMarksFailuresAndContinues(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)
	ext.fail["Architecture"] = true

	rep, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.String())
	if rep.SourcesFailed != 1 {
		t.Errorf("SourcesFailed = %d, want 1", rep.SourcesFailed)
	}
	if rep.SourcesDone != rep.SourcesTried-1 {
		t.Errorf("a single failure stopped the run: done=%d tried=%d",
			rep.SourcesDone, rep.SourcesTried)
	}

	states, err := sys.Store.CountSourcesByEntityPass()
	if err != nil {
		t.Fatal(err)
	}
	if states[store.EntityPassFailed] != 1 {
		t.Errorf("failed = %d, want 1: %v", states[store.EntityPassFailed], states)
	}
	// A deterministic failure must not be retried on the next run, or it blocks the
	// queue behind it forever. Retrying is an explicit ResetEntityPass.
	again, err := sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.SourcesTried != 0 {
		t.Errorf("a failed source was retried automatically (%d tried)", again.SourcesTried)
	}
}

// TestEnrichmentPriorityOrder checks that the most-linked note is enriched first, so
// a run that is 40% done has done the useful 40%.
func TestEnrichmentPriorityOrder(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)

	pending, err := sys.Store.SourcesPendingEnrichment(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) < 3 {
		t.Fatalf("need several sources, got %d", len(pending))
	}

	// Architecture.md carries the most outbound links in the fixture.
	links, err := sys.Store.LinksTouching([]string{"Architecture.md"})
	if err != nil {
		t.Fatal(err)
	}
	out := 0
	for _, l := range links {
		if l.FromRef == "Architecture.md" {
			out++
		}
	}
	if out == 0 {
		t.Fatal("the fixture has no outbound links from Architecture.md")
	}
	if pending[0].SourceRef != "Architecture.md" {
		t.Errorf("first pending source is %q; the most-linked note should come first "+
			"(Architecture.md has %d outbound links)", pending[0].SourceRef, out)
	}

	// And within the rest, larger before smaller.
	for i := 1; i < len(pending)-1; i++ {
		if pending[i].TokenCount < pending[i+1].TokenCount {
			// Only a violation when link counts are equal; log rather than fail, since
			// link count is the primary key of the ordering.
			t.Logf("note: %s (%d tokens) precedes %s (%d tokens)",
				pending[i].SourceRef, pending[i].TokenCount,
				pending[i+1].SourceRef, pending[i+1].TokenCount)
		}
	}
}

// TestRollbackRemovesOnlyTheLLMTier is what the `extractor` provenance column exists
// for: a model that extracts badly can be undone without disturbing the heuristic and
// tag entities search has used since ingestion.
func TestRollbackRemovesOnlyTheLLMTier(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)
	ext.byTitle["Architecture"] = ExtractResult{
		Entities: []ExtractedEntity{{Kind: "code", Value: "DuckDB"}},
	}
	if _, err := sys.Enrich(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}

	before := entityValuesByExtractor(t, sys.Store)
	if len(before[ExtractorLLM]) == 0 {
		t.Fatal("nothing to roll back")
	}

	n, err := sys.Store.DeleteChunkEntitiesByExtractor(ExtractorLLM)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("removed %d llm associations", n)

	after := entityValuesByExtractor(t, sys.Store)
	if len(after[ExtractorLLM]) != 0 {
		t.Errorf("%d llm associations survived the rollback", len(after[ExtractorLLM]))
	}
	if len(after["heuristic"]) != len(before["heuristic"]) {
		t.Errorf("the rollback took heuristic entities with it: %d -> %d",
			len(before["heuristic"]), len(after["heuristic"]))
	}
	if len(after["tag"]) != len(before["tag"]) {
		t.Errorf("the rollback took tag entities with it: %d -> %d",
			len(before["tag"]), len(after["tag"]))
	}

	// And the pass can be re-run afterwards.
	if err := sys.Store.ResetEntityPass(); err != nil {
		t.Fatal(err)
	}
	states, err := sys.Store.CountSourcesByEntityPass()
	if err != nil {
		t.Fatal(err)
	}
	if states[store.EntityPassDone] != 0 {
		t.Errorf("after a reset, %d sources are still marked done", states[store.EntityPassDone])
	}
}

// TestReingestReenrichesAndKeepsInferredLinks covers the interaction with Phase 7:
// re-ingesting a note must re-queue it for enrichment, and the inferred links of the
// notes on the other end must survive.
func TestReingestReenrichesAndKeepsInferredLinks(t *testing.T) {
	ext := newStubExtractor()
	sys, root := newEnrichSystem(t, ext)
	ext.byTitle["Broken Links"] = ExtractResult{
		Links: []ExtractedLink{{Target: "Retrieval", Evidence: "This references"}},
	}
	if _, err := sys.Enrich(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}

	countInferred := func() int {
		n := 0
		for _, e := range edgeSet(t, sys.Store) {
			if e.Kind == EdgeInferredLink {
				n++
			}
		}
		return n
	}
	before := countInferred()
	if before == 0 {
		t.Fatal("no inferred edges to begin with")
	}

	// Edit the link's *target*, whose chunks are replaced — deleting the inbound
	// inferred edge from a note that is not itself re-enriched.
	path := filepath.Join(root, "Retrieval.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte("\n## Added\n\nNew section.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := sys.Ingester.IngestDirectory(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FilesIngested != 1 {
		t.Fatalf("expected one re-ingested file, got %d", rep.FilesIngested)
	}
	if _, err := sys.BuildEdgesIncremental(context.Background(), rep, EdgeParams{}); err != nil {
		t.Fatal(err)
	}

	if got := countInferred(); got < before {
		t.Errorf("inferred edges after re-ingest: %d, want at least %d — persisting them "+
			"in memory_links is what stops the same erosion authored links had", got, before)
	}

	// The re-ingested note must be pending again: its content changed, so its old
	// extraction no longer describes it.
	src, found, err := sys.Store.FindSource(store.SourceDirectory, "Retrieval.md")
	if err != nil || !found {
		t.Fatal("Retrieval.md missing")
	}
	if src.EntityPass != store.EntityPassPending {
		t.Errorf("a re-ingested note is marked %q; it should be pending re-enrichment",
			src.EntityPass)
	}
}

// TestEnrichRequiresExtractor checks the degraded path: with no extraction model the
// pass reports why rather than doing something surprising.
func TestEnrichRequiresExtractor(t *testing.T) {
	s := openTestStore(t)
	sys := NewSystem(s, Config{ModelDir: filepath.Join(t.TempDir(), "absent")}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	if sys.Extractor() != nil {
		t.Fatal("an extractor was configured despite no model")
	}
	if _, err := sys.Enrich(context.Background(), 0, nil); err == nil {
		t.Error("Enrich should report that extraction is not configured")
	}
	if _, err := sys.EnqueueEnrichment(0, nil); err == nil {
		t.Error("EnqueueEnrichment should refuse without an extractor")
	}

	st, err := sys.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.ExtractModel != "" {
		t.Errorf("Status reports extraction model %q with none configured", st.ExtractModel)
	}
}

// TestExtractionPromptCarriesCandidatesOnlyForNotes checks that a conversation source
// is not offered cross-reference candidates it could never use.
func TestExtractionPromptCarriesCandidatesOnlyForNotes(t *testing.T) {
	ext := newStubExtractor()
	sys, _ := newEnrichSystem(t, ext)
	if _, err := sys.Ingester.IngestTurns(context.Background(), "s", "Chat", turnMsgs(
		msg("user", "where does the schema live?"),
		msg("assistant", "In store.Open, created idempotently."))); err != nil {
		t.Fatal(err)
	}

	if _, err := sys.Enrich(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}

	var sawNote, sawConversation bool
	for _, c := range ext.calls {
		if strings.HasSuffix(c.Ref, ".md") {
			sawNote = true
			if len(c.CandidateNotes) == 0 {
				t.Errorf("note %q was offered no link candidates", c.Ref)
			}
		} else {
			sawConversation = true
			if len(c.CandidateNotes) != 0 {
				t.Errorf("conversation source %q was offered %d link candidates it cannot "+
					"use", c.Ref, len(c.CandidateNotes))
			}
		}
	}
	if !sawNote || !sawConversation {
		t.Fatalf("expected both kinds of source to be extracted (note=%v conversation=%v)",
			sawNote, sawConversation)
	}
}

// ---------- query-side entity extraction ----------

// TestQueryNgrams checks the normalization that makes a dictionary lookup possible.
func TestQueryNgrams(t *testing.T) {
	got := queryNgrams("Where is store.Open, and conf/deploy.yaml?")
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, want := range []string{
		"store.open",           // punctuation trimmed, dots kept
		"conf/deploy.yaml",     // slashes kept
		"where is store.open",  // 3-gram
		"and conf/deploy.yaml", // trailing "?" trimmed
	} {
		if !set[want] {
			t.Errorf("missing n-gram %q from %v", want, got)
		}
	}
	// The cap must hold, or a long query explodes the lookup.
	for _, g := range got {
		if n := len(strings.Fields(g)); n > MaxQueryEntityWords {
			t.Errorf("n-gram %q has %d words, over the cap of %d", g, n, MaxQueryEntityWords)
		}
	}
}

// TestQueryEntityLookupFindsLowercaseMentions is the fix for what made the entity
// signal inert: a lowercase question mentioning a known entity must match it.
//
// Before this, query-side extraction ran the corpus-side heuristics, which recognize
// proper nouns by capitalization — so a typed question yielded nothing and no chunk
// could match on entities however well the corpus was enriched.
func TestQueryEntityLookupFindsLowercaseMentions(t *testing.T) {
	sys := newConvSystem(t)
	if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
		ID: "a1", SessionID: "s", Title: "Deployment",
		Content: "# Deployment\n\nThe Falkirk Wheel rollout is owned by Dana Whitfield " +
			"and configured in conf/deploy.yaml.\n",
	}); err != nil {
		t.Fatal(err)
	}

	// A lowercase question — exactly the shape a user types, and the shape the old
	// capitalization heuristic could not handle.
	got, err := sys.queryEntityValues("who owns the falkirk wheel rollout?")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	if !set["falkirk wheel"] {
		t.Errorf("the multi-word entity was not matched from a lowercase query: %v", got)
	}

	// The heuristic tier still contributes the punctuation-heavy shapes.
	got, err = sys.queryEntityValues("what is in conf/deploy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sawPath bool
	for _, v := range got {
		if strings.Contains(v, "deploy.yaml") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Errorf("path entity not matched: %v", got)
	}

	// A query mentioning nothing known must match nothing — the lookup has to stay a
	// lookup, not degrade into fuzzy matching.
	got, err = sys.queryEntityValues("completely unrelated zzyzx question")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range got {
		if v == "falkirk wheel" {
			t.Errorf("unrelated query matched %q", v)
		}
	}
}

// TestEnrichedEntitiesAreReachableFromQueries is the property that makes corpus-side
// enrichment worth doing at all: an entity the LLM pass adds must become something a
// query can match.
func TestEnrichedEntitiesAreReachableFromQueries(t *testing.T) {
	ext := newStubExtractor()
	s := openTestStore(t)
	sys := NewSystem(s, Config{
		ModelDir: filepath.Join(t.TempDir(), "absent"), Extractor: ext,
	}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	// A note containing a multi-word name the heuristics will not extract, because it
	// is written lowercase in the source.
	if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
		ID: "a1", SessionID: "s", Title: "Runbook",
		Content: "# Runbook\n\nthe quorum drain procedure must complete before failover.\n",
	}); err != nil {
		t.Fatal(err)
	}

	const phrase = "quorum drain procedure"
	before, err := sys.queryEntityValues("how does the " + phrase + " work?")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range before {
		if v == phrase {
			t.Fatal("the heuristic tier already extracted the phrase; this test cannot " +
				"show what enrichment adds")
		}
	}

	ext.byTitle["Runbook"] = ExtractResult{
		Entities: []ExtractedEntity{{Kind: "proper", Value: "quorum drain procedure"}},
	}
	if _, err := sys.Enrich(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}

	after, err := sys.queryEntityValues("how does the " + phrase + " work?")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range after {
		if v == phrase {
			found = true
		}
	}
	if !found {
		t.Errorf("an enriched entity is not reachable from a query mentioning it: %v", after)
	}

	// And it must actually drive retrieval, not merely exist.
	results, rep, err := sys.Search(context.Background(),
		"how does the quorum drain procedure work?",
		SearchOptions{Limit: 5, Weights: Weights{Entity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.QueryEntities) == 0 {
		t.Error("the search reported no query entities")
	}
	if findText(results, "quorum drain") == nil {
		t.Errorf("entity-only retrieval did not find the note; query entities were %v",
			rep.QueryEntities)
	}
}

// TestEnrichmentBatchesAndYields checks that a large enrichment does not monopolize
// the single queue worker.
//
// Enrichment shares that worker with ingestion — it must, since two concurrent writers
// hit DuckDB write-write conflicts — so an unbounded pass over a vault would hold it
// for hours and every conversation turn queued behind it would just wait. The pass
// therefore processes a bounded batch and re-enqueues itself, letting anything waiting
// run in between.
func TestEnrichmentBatchesAndYields(t *testing.T) {
	ext := newStubExtractor()
	s := openTestStore(t)
	sys := NewSystem(s, Config{
		ModelDir: filepath.Join(t.TempDir(), "absent"), Extractor: ext,
	}, nil)
	t.Cleanup(func() { sys.Queue.Stop() })

	// Enough sources that a batch of 2 cannot finish them.
	const notes = 7
	for i := 0; i < notes; i++ {
		if _, err := sys.Ingester.IngestArtifact(context.Background(), store.Artifact{
			ID:      fmt.Sprintf("a%d", i),
			Title:   fmt.Sprintf("Note %d", i),
			Content: fmt.Sprintf("# Note %d\n\nContent for note %d with Distinct%d inside.\n", i, i, i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := sys.Store.SourcesPendingEnrichment(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != notes {
		t.Fatalf("expected %d pending sources, got %d", notes, len(pending))
	}

	// A single batch must stop at the limit, not run away.
	rep, err := sys.Enrich(context.Background(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourcesTried != 2 {
		t.Errorf("a batch of 2 tried %d sources", rep.SourcesTried)
	}

	// And an unspecified limit must not silently mean "everything": that would take
	// SourcesPendingEnrichment's own default and quietly stop there on a large vault.
	rep, err = sys.Enrich(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourcesTried > EnrichBatch {
		t.Errorf("limit 0 tried %d sources, over the batch size of %d",
			rep.SourcesTried, EnrichBatch)
	}

	// Now the queued form: it must chain batches until nothing is pending, and an
	// ingestion job enqueued alongside must not be starved behind it.
	if err := sys.Store.ResetEntityPass(); err != nil {
		t.Fatal(err)
	}
	// The probe records how much work was still outstanding when it ran. That is the
	// assertion that actually distinguishes batching from a single fast job: with
	// batching it runs between batches and sees work remaining, whereas an unbounded
	// enrichment would finish everything first and the probe would see zero — which a
	// mere "did the probe run" check could never tell apart.
	var probeRan bool
	var pendingWhenProbeRan int
	if _, err := sys.EnqueueEnrichment(2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.Queue.Enqueue(Job{Kind: "probe", Key: "probe", Run: func(context.Context) error {
		states, err := sys.Store.CountSourcesByEntityPass()
		if err != nil {
			return err
		}
		pendingWhenProbeRan = states[store.EntityPassPending]
		probeRan = true
		return nil
	}}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		states, err := sys.Store.CountSourcesByEntityPass()
		if err != nil {
			t.Fatal(err)
		}
		if states[store.EntityPassPending] == 0 && probeRan {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	states, err := sys.Store.CountSourcesByEntityPass()
	if err != nil {
		t.Fatal(err)
	}
	if states[store.EntityPassPending] != 0 {
		t.Errorf("%d sources still pending; batches did not chain to completion", states[store.EntityPassPending])
	}
	if !probeRan {
		t.Fatal("the job queued alongside enrichment never ran — enrichment monopolized " +
			"the worker, which is the thing batching exists to prevent")
	}
	if pendingWhenProbeRan == 0 {
		t.Errorf("the probe ran only after enrichment had finished everything; batches "+
			"are not yielding the worker between them (%d sources, batch size 2)", notes)
	}
	t.Logf("the probe ran with %d of %d sources still pending", pendingWhenProbeRan, notes)
}
