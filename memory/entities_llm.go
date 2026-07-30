package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"simple-cot-chat/store"
)

// LLM entity enrichment (§3.3).
//
// Runs strictly as a second pass: ingestion completes with heuristic entities only
// and memory is searchable immediately, then this enriches in the background. Four
// decisions keep it affordable on a vault:
//
//  1. **Per source, not per chunk.** One call per note rather than per chunk — a 10×
//     cut, and better quality, since the model sees the whole note instead of a
//     fragment.
//  2. **Never blocking.** Resumable via memory_sources.entity_pass, so an interrupted
//     run continues instead of restarting.
//  3. **Prioritized.** Most-linked, then largest, then most recent — so a run that is
//     40% done has done the useful 40%.
//  4. **A separate, smaller model.** Extraction is far easier than reasoning; paying
//     chat-model rates for it is waste.

// ExtractorLLM is the extractor value recorded on associations this pass produces,
// so the whole tier can be reweighted or rolled back without touching the others.
const ExtractorLLM = "llm"

// Enrichment limits.
const (
	// MaxSourceCharsForLLM bounds what one call sends. A note longer than this is
	// truncated rather than skipped: its opening carries most of its identity, and
	// skipping outright would leave the largest notes — the ones priority ordering
	// puts first — permanently unenriched.
	MaxSourceCharsForLLM = 24000
	// MaxEntitiesPerSource bounds what is accepted from one note, so a model that
	// enumerates every capitalized word cannot swamp the entity table.
	MaxEntitiesPerSource = 60
	// MaxInferredLinksPerSource bounds proposed cross-references per note.
	MaxInferredLinksPerSource = 12
	// EnrichBatch is how many sources one queued enrichment job processes before
	// re-enqueueing itself.
	//
	// The bound is what makes "never blocking" true rather than aspirational. Enrichment
	// shares the single ingestion worker — it must, since two concurrent writers hit
	// DuckDB write-write conflicts — so an unbounded run over a 3,000-note vault would
	// hold that worker for hours and every conversation and directory ingest queued
	// behind it would simply wait. Batching lets those interleave between batches, at
	// the cost of one extra queue round trip per batch.
	EnrichBatch = 25
	// MaxEntityValueChars rejects an "entity" that is really a sentence.
	MaxEntityValueChars = 96
	// InferredLinkWeight is the edge weight for a model-proposed cross-reference.
	// Below an authored link's 1.0 before the walk's per-kind decay is even applied,
	// because it is a guess about a connection rather than an assertion of one.
	InferredLinkWeight = 0.8
)

// llmEntityKinds are the kinds this pass may produce. A model that invents a kind
// gets the entity rejected rather than silently widening the taxonomy — the kinds are
// what the entity signal's weighting and the date-proximity logic key off.
var llmEntityKinds = map[string]bool{
	"person": true, "org": true, "date": true,
	"path": true, "code": true, "number": true, "proper": true,
}

// ExtractedEntity is one entity the model proposed.
type ExtractedEntity struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ExtractedLink is one cross-reference the model proposed: a note it believes this
// note is talking about without linking to it.
//
// Evidence is required and does double duty — it is the hallucination guard (the
// quote must appear verbatim in the note) and the means of aiming the edge at the
// chunk that actually makes the reference rather than at the note's first chunk.
type ExtractedLink struct {
	Target   string `json:"target"`
	Evidence string `json:"evidence"`
}

// ExtractResult is one call's typed output.
type ExtractResult struct {
	Entities []ExtractedEntity `json:"entities"`
	Links    []ExtractedLink   `json:"links"`
}

// ExtractRequest is one note presented for extraction.
type ExtractRequest struct {
	Title string
	Ref   string
	Text  string
	// CandidateNotes are the note names the model may propose as cross-reference
	// targets. Supplying them turns an open-ended guess into a choice from a list,
	// which is both easier for the model and far less likely to invent a target.
	CandidateNotes []string
}

// EntityExtractor is the LLM tier's transport.
//
// An interface so the pass is testable without a model: the guards, normalization,
// chunk attribution and link resolution are where the bugs live, and none of them
// need a network round trip to exercise.
type EntityExtractor interface {
	Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error)
	ModelName() string
}

// extractionSchema constrains the model to a typed result.
//
// Structured output is the difference between a reliable pipeline and one that fails
// on every tenth note. api.ChatRequest.Format takes a full JSON schema, so the shape
// is enforced by the server rather than by parsing prose and hoping.
var extractionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"entities": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "enum": ["person","org","date","path","code","number","proper"]},
					"value": {"type": "string"}
				},
				"required": ["kind","value"]
			}
		},
		"links": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"target": {"type": "string"},
					"evidence": {"type": "string"}
				},
				"required": ["target","evidence"]
			}
		}
	},
	"required": ["entities","links"]
}`)

const extractionSystemPrompt = `You extract structured index data from one note. Return JSON only.

entities: the significant named things in the note — people, organizations, projects,
file paths, identifiers, dates, quantities. Copy each value EXACTLY as it appears in
the note, character for character. Do not translate, expand, correct or reformat it.
Do not invent anything that is not written in the note. Prefer specific names over
generic words; skip ordinary nouns.

links: notes from the candidate list that THIS note is clearly talking about without
linking to. For each, quote the exact sentence fragment from this note that shows the
connection, copied character for character. If nothing in the candidate list is
clearly discussed, return an empty list. Do not guess.`

// OllamaExtractor drives extraction over the existing Ollama client.
type OllamaExtractor struct {
	cli   *api.Client
	model string
	// numCtx sizes the context window. Extraction sends a whole note, which is far
	// larger than a chat turn, and a too-small window silently truncates the note —
	// producing entities from its first paragraph and no error.
	numCtx int
}

// NewOllamaExtractor builds an extractor. model may differ from the chat model, and
// should: extraction is a much easier task than reasoning.
func NewOllamaExtractor(cli *api.Client, model string, numCtx int) (*OllamaExtractor, error) {
	if cli == nil {
		return nil, fmt.Errorf("ollama client is required for entity extraction")
	}
	if model == "" {
		return nil, fmt.Errorf("extraction model is required")
	}
	if numCtx <= 0 {
		numCtx = 8192
	}
	return &OllamaExtractor{cli: cli, model: model, numCtx: numCtx}, nil
}

func (e *OllamaExtractor) ModelName() string { return e.model }

func (e *OllamaExtractor) Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error) {
	var out ExtractResult

	var b strings.Builder
	fmt.Fprintf(&b, "Note: %s\n", req.Title)
	if len(req.CandidateNotes) > 0 {
		fmt.Fprintf(&b, "\nCandidate notes for links (use these names exactly, or none):\n")
		for _, n := range req.CandidateNotes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	fmt.Fprintf(&b, "\n---\n%s\n---\n", req.Text)

	stream := false
	var raw string
	err := e.cli.Chat(ctx, &api.ChatRequest{
		Model: e.model,
		Messages: []api.Message{
			{Role: "system", Content: extractionSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		Format: extractionSchema,
		Stream: &stream,
		// Deterministic: the same note must yield the same entities, or re-running the
		// pass churns the entity table and nothing downstream is reproducible.
		Options: map[string]any{"temperature": 0, "num_ctx": e.numCtx},
	}, func(resp api.ChatResponse) error {
		raw += resp.Message.Content
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("extraction call: %w", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err != nil {
		return out, fmt.Errorf("extraction returned unparseable JSON despite the schema: %w", err)
	}
	return out, nil
}

// GuardReport counts what the guards rejected. Reported rather than logged, because
// a pass that silently discards most of what the model returns looks identical to a
// model that returned little.
type GuardReport struct {
	// EntitiesProposed / Accepted bracket the guard's effect.
	EntitiesProposed int `json:"entitiesProposed"`
	EntitiesAccepted int `json:"entitiesAccepted"`
	// RejectedNotVerbatim is the hallucination count: the model named something that
	// does not appear in the note.
	RejectedNotVerbatim int `json:"rejectedNotVerbatim"`
	RejectedBadKind     int `json:"rejectedBadKind"`
	RejectedTooLong     int `json:"rejectedTooLong"`
	RejectedEmpty       int `json:"rejectedEmpty"`
	RejectedOverCap     int `json:"rejectedOverCap"`

	LinksProposed int `json:"linksProposed"`
	LinksAccepted int `json:"linksAccepted"`
	// LinksUnresolved is a proposed target that is not a real note.
	LinksUnresolved int `json:"linksUnresolved"`
	// LinksNoEvidence is a proposal whose quote does not appear in the note — the
	// same hallucination guard applied to links.
	LinksNoEvidence int `json:"linksNoEvidence"`
	LinksSelf       int `json:"linksSelf"`
}

func (r *GuardReport) add(o GuardReport) {
	r.EntitiesProposed += o.EntitiesProposed
	r.EntitiesAccepted += o.EntitiesAccepted
	r.RejectedNotVerbatim += o.RejectedNotVerbatim
	r.RejectedBadKind += o.RejectedBadKind
	r.RejectedTooLong += o.RejectedTooLong
	r.RejectedEmpty += o.RejectedEmpty
	r.RejectedOverCap += o.RejectedOverCap
	r.LinksProposed += o.LinksProposed
	r.LinksAccepted += o.LinksAccepted
	r.LinksUnresolved += o.LinksUnresolved
	r.LinksNoEvidence += o.LinksNoEvidence
	r.LinksSelf += o.LinksSelf
}

// AcceptedLink is a cross-reference that survived the guards.
type AcceptedLink struct {
	ToRef    string
	Evidence string
}

// GuardExtraction applies every hallucination guard and returns what survives.
//
// Pure, and deliberately so: this is the safety-critical part of the pass — an
// invented entity is worse than a missed one, because it pollutes the entity signal
// and wires unrelated notes together — and a pure function can be tested against
// every failure shape without a model or a database.
//
// The central rule is **verbatim appearance**: every accepted value must occur in the
// note, case-insensitively. Cheap, strict, and it costs almost no recall, because
// what is being asked for is entities that *are* in the text.
func GuardExtraction(text string, resolver *LinkResolver, selfRef string, res ExtractResult) ([]store.ChunkEntity, []AcceptedLink, GuardReport) {
	var rep GuardReport
	lower := strings.ToLower(text)

	seen := map[string]bool{}
	entities := make([]store.ChunkEntity, 0, len(res.Entities))
	for _, e := range res.Entities {
		rep.EntitiesProposed++
		value := strings.TrimSpace(e.Value)
		kind := strings.ToLower(strings.TrimSpace(e.Kind))

		switch {
		case value == "":
			rep.RejectedEmpty++
			continue
		case len(value) > MaxEntityValueChars:
			rep.RejectedTooLong++
			continue
		case !llmEntityKinds[kind]:
			rep.RejectedBadKind++
			continue
		case !strings.Contains(lower, strings.ToLower(value)):
			// The hallucination guard. Everything else here is hygiene; this is the
			// one that matters.
			rep.RejectedNotVerbatim++
			continue
		}
		if len(entities) >= MaxEntitiesPerSource {
			rep.RejectedOverCap++
			continue
		}
		// Normalize and merge rather than minting near-duplicates: "Florida Hardware"
		// and "florida hardware" are one entity.
		norm := strings.ToLower(value)
		key := kind + "\x00" + norm
		if seen[key] {
			continue
		}
		seen[key] = true
		entities = append(entities, store.ChunkEntity{
			Kind: kind, ValueNorm: norm, Count: 1, Extractor: ExtractorLLM,
		})
		rep.EntitiesAccepted++
	}

	links := make([]AcceptedLink, 0, len(res.Links))
	seenTarget := map[string]bool{}
	for _, l := range res.Links {
		rep.LinksProposed++
		target := strings.TrimSpace(l.Target)
		evidence := strings.TrimSpace(l.Evidence)
		if target == "" {
			rep.LinksUnresolved++
			continue
		}
		if evidence == "" || !strings.Contains(lower, strings.ToLower(evidence)) {
			rep.LinksNoEvidence++
			continue
		}
		ref, ok := resolveTarget(resolver, target)
		if !ok {
			// A proposed target that is not a real note is dropped and counted, never
			// guessed at — the same rule authored wikilinks follow.
			rep.LinksUnresolved++
			continue
		}
		if ref == selfRef {
			rep.LinksSelf++
			continue
		}
		if seenTarget[ref] || len(links) >= MaxInferredLinksPerSource {
			continue
		}
		seenTarget[ref] = true
		links = append(links, AcceptedLink{ToRef: ref, Evidence: evidence})
		rep.LinksAccepted++
	}
	return entities, links, rep
}

// resolveTarget maps a proposed target onto a real source_ref.
func resolveTarget(resolver *LinkResolver, target string) (string, bool) {
	if resolver == nil {
		return "", false
	}
	return resolver.Resolve(target)
}

// EnrichReport summarizes an enrichment run.
type EnrichReport struct {
	Model          string        `json:"model"`
	SourcesTried   int           `json:"sourcesTried"`
	SourcesDone    int           `json:"sourcesDone"`
	SourcesFailed  int           `json:"sourcesFailed"`
	SourcesSkipped int           `json:"sourcesSkipped"`
	Guards         GuardReport   `json:"guards"`
	EdgesWritten   int           `json:"edgesWritten"`
	Duration       time.Duration `json:"duration"`
}

func (r EnrichReport) String() string {
	rate := 0.0
	if r.Duration > 0 && r.SourcesDone > 0 {
		rate = r.Duration.Seconds() / float64(r.SourcesDone)
	}
	return fmt.Sprintf(
		"enriched %d/%d sources with %s (%d failed, %d skipped, %.1fs each); "+
			"entities %d/%d accepted (rejected: %d not verbatim, %d bad kind, %d too long, "+
			"%d empty, %d over cap); links %d/%d accepted (%d unresolved, %d no evidence, "+
			"%d self), %d inferred_link edges; in %s",
		r.SourcesDone, r.SourcesTried, r.Model, r.SourcesFailed, r.SourcesSkipped, rate,
		r.Guards.EntitiesAccepted, r.Guards.EntitiesProposed,
		r.Guards.RejectedNotVerbatim, r.Guards.RejectedBadKind, r.Guards.RejectedTooLong,
		r.Guards.RejectedEmpty, r.Guards.RejectedOverCap,
		r.Guards.LinksAccepted, r.Guards.LinksProposed, r.Guards.LinksUnresolved,
		r.Guards.LinksNoEvidence, r.Guards.LinksSelf, r.EdgesWritten,
		r.Duration.Round(time.Millisecond))
}

// HallucinationRate is the share of proposed entities that did not appear in the
// note — the health metric for the extraction model, and the number that decides
// whether a smaller model is good enough for this job.
func (r EnrichReport) HallucinationRate() float64 {
	if r.Guards.EntitiesProposed == 0 {
		return 0
	}
	return float64(r.Guards.RejectedNotVerbatim) / float64(r.Guards.EntitiesProposed)
}

// Enrich runs the LLM entity pass over pending sources, highest-priority first.
//
// limit bounds one run; it is required, not optional — passing 0 would silently take
// SourcesPendingEnrichment's own default and stop there, which on a vault means quietly
// enriching the first hundred notes and reporting success. Callers that want everything
// should loop, or use EnqueueEnrichment which does the looping through the queue.
//
// Each source is committed independently and marked done or failed as it completes, so
// an interrupted run resumes rather than restarting — the difference between a pass
// that survives a closed laptop and one that never finishes on a real vault.
func (sys *System) Enrich(ctx context.Context, limit int, onProgress func(done, total int)) (EnrichReport, error) {
	started := time.Now()
	rep := EnrichReport{}

	ext := sys.Extractor()
	if ext == nil {
		return rep, &ErrUnavailable{Reason: "entity extraction is not configured"}
	}
	rep.Model = ext.ModelName()

	if limit <= 0 {
		limit = EnrichBatch
	}
	pending, err := sys.Store.SourcesPendingEnrichment(limit)
	if err != nil {
		return rep, err
	}
	if len(pending) == 0 {
		rep.Duration = time.Since(started)
		return rep, nil
	}
	rep.SourcesTried = len(pending)

	// The candidate list for cross-references: every directory note. Built once per
	// run rather than per source, since it changes only when ingestion runs.
	refs, err := sys.Store.SourceRefsByType(store.SourceDirectory)
	if err != nil {
		return rep, err
	}
	resolver := NewLinkResolver()
	for _, ref := range refs {
		resolver.AddNote(ref, nil)
	}

	for i, src := range pending {
		if err := ctx.Err(); err != nil {
			rep.Duration = time.Since(started)
			return rep, err
		}
		if onProgress != nil {
			onProgress(i, len(pending))
		}

		guards, edges, err := sys.enrichSource(ctx, ext, src, resolver, refs)
		rep.Guards.add(guards)
		rep.EdgesWritten += edges
		switch {
		case err != nil:
			rep.SourcesFailed++
			slog.Warn("entity enrichment failed for a source",
				"ref", src.SourceRef, "error", err)
			// Marked failed rather than left pending, so a source that fails
			// deterministically does not block the queue behind it forever. A retry is
			// an explicit ResetEntityPass.
			if err := sys.Store.SetSourceEntityPass(src.ID, store.EntityPassFailed); err != nil {
				return rep, err
			}
		default:
			rep.SourcesDone++
			if err := sys.Store.SetSourceEntityPass(src.ID, store.EntityPassDone); err != nil {
				return rep, err
			}
		}
	}
	if onProgress != nil {
		onProgress(len(pending), len(pending))
	}

	rep.Duration = time.Since(started)
	return rep, nil
}

// enrichSource extracts for one source and writes what survives the guards.
func (sys *System) enrichSource(ctx context.Context, ext EntityExtractor,
	src store.MemorySource, resolver *LinkResolver, allRefs []string,
) (GuardReport, int, error) {
	var guards GuardReport

	chunks, err := sys.Store.GetChunksBySource(src.ID)
	if err != nil {
		return guards, 0, err
	}
	if len(chunks) == 0 {
		return guards, 0, nil
	}

	// The note text is reassembled from the *stored chunks*, not re-read from disk.
	// Two reasons, both correctness rather than convenience: the file may have changed
	// since ingestion, and dedup may have dropped passages — so the verbatim guard and
	// the chunk attribution below would disagree with the model's input if the source
	// of truth were the file.
	parts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		parts = append(parts, c.Text)
	}
	text := strings.Join(parts, "\n\n")
	truncated := false
	if len(text) > MaxSourceCharsForLLM {
		text = truncateRunes(text, MaxSourceCharsForLLM)
		truncated = true
	}

	title := src.Title
	if title == "" {
		title = src.SourceRef
	}
	// Cross-references are proposed only *from* notes. Entities are worth extracting
	// from every source type — a name in a conversation is as findable as one in a note
	// — but an inferred link needs both ends resolvable as vault notes, and offering
	// candidates to a conversation source would only produce proposals that the link
	// builder then drops. Better to not ask than to drop silently.
	var candidates []string
	if src.SourceType == store.SourceDirectory {
		candidates = candidateNotes(allRefs, src.SourceRef)
	}
	res, err := ext.Extract(ctx, ExtractRequest{
		Title: title, Ref: src.SourceRef, Text: text,
		CandidateNotes: candidates,
	})
	if err != nil {
		return guards, 0, err
	}

	// Guard against the text actually sent, so a truncated note cannot accept an
	// entity from the part the model never saw.
	entities, links, guards := GuardExtraction(text, resolver, src.SourceRef, res)
	if truncated {
		slog.Debug("note truncated for extraction", "ref", src.SourceRef, "chars", MaxSourceCharsForLLM)
	}

	// Attribute each entity to the chunks that actually contain it. An entity the
	// model found across a chunk boundary belongs to no single chunk and is dropped:
	// the entity signal answers "does this chunk mention X", and a chunk that does not
	// contain the string cannot honestly claim it.
	byChunk := map[string][]store.ChunkEntity{}
	for _, e := range entities {
		for _, c := range chunks {
			if strings.Contains(strings.ToLower(c.Text), e.ValueNorm) {
				byChunk[c.ID] = append(byChunk[c.ID], e)
			}
		}
	}
	if err := sys.Store.AddChunkEntities(byChunk); err != nil {
		return guards, 0, err
	}

	if src.SourceType != store.SourceDirectory {
		return guards, 0, nil
	}
	edges, err := sys.writeInferredLinks(src, links)
	if err != nil {
		return guards, 0, err
	}
	return guards, edges, nil
}

// writeInferredLinks persists accepted cross-references and builds their edges.
//
// Deliberately routed through `memory_links` and `BuildLinkEdges` rather than writing
// edges directly. Two things fall out of that: an inferred link survives a re-ingest
// of either endpoint exactly as an authored one does (the erosion bug of §4, Phase 7,
// which would otherwise reappear here in a new guise), and the evidence quote does the
// same job `Raw` does for a wikilink — locating the chunk that makes the reference, so
// an inferred edge is as specific as an authored one instead of hanging off the note's
// opening.
func (sys *System) writeInferredLinks(src store.MemorySource, links []AcceptedLink) (int, error) {
	stored := make([]store.StoredLink, 0, len(links))
	pending := make([]PendingLink, 0, len(links))
	for _, l := range links {
		stored = append(stored, store.StoredLink{
			ToRef: l.ToRef, Raw: l.Evidence, Kind: EdgeInferredLink,
		})
		pending = append(pending, PendingLink{
			FromSourceRef: src.SourceRef, ToSourceRef: l.ToRef,
			Raw: l.Evidence, Kind: EdgeInferredLink,
		})
	}
	// Replaces this note's previous inferred links only — its authored ones are a
	// different kind and belong to ingestion.
	if err := sys.Store.ReplaceLinksFrom(src.SourceRef, EdgeInferredLink, stored); err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}
	rep, err := sys.BuildLinkEdges(pending, EdgeParams{})
	if err != nil {
		return 0, err
	}
	return rep.Link, nil
}

// truncateRunes cuts a string to at most n bytes without splitting a rune.
//
// A plain byte slice would leave a partial multi-byte character at the end, which is
// invalid UTF-8 and would be sent as such in the JSON request body. Vault notes are
// full of non-ASCII — em dashes, accented names, the heading separator this codebase
// itself uses — so this is the common case, not an edge one.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// candidateNotes trims the note list offered to the model.
//
// Capped because the list goes into every prompt: on a 3,000-note vault the full list
// would dominate the context window and crowd out the note itself. The cap is a real
// limitation on large vaults — a target outside the window cannot be proposed — and
// the honest fix is to preselect candidates by similarity rather than to raise it.
func candidateNotes(refs []string, self string) []string {
	const maxCandidates = 200
	out := make([]string, 0, min(len(refs), maxCandidates))
	for _, r := range refs {
		if r == self {
			continue
		}
		out = append(out, noteName(r))
		if len(out) >= maxCandidates {
			break
		}
	}
	return out
}

// noteName reduces a source_ref to the name a model would naturally use.
func noteName(ref string) string {
	base := ref
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(base, ".markdown"), ".md")
}
