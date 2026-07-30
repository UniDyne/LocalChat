package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"simple-cot-chat/store"
)

// Conversation ingestion (§3.3).
//
// The unit is one **turn pair**: a user message plus its assistant reply. Not
// per-message (a question without its answer has no content, an answer without its
// question has no topic) and not per-session (every new turn would re-chunk and
// re-embed the whole conversation, and per-source argmax would cap a 200-message
// session at one returned chunk). Per-turn-pair is append-only — a new turn adds a
// source and nothing existing re-chunks.

const (
	// PrevGistWords caps the previous turn's gist inside the thread-context prefix.
	// Small on purpose: its job is to situate the turn, not to restate it.
	PrevGistWords = 12
	// CoTStoreWords bounds how much of the reasoning note is stored. The real limit
	// is BuildEmbeddedText's CoTSubCap in tokens; this just avoids keeping a
	// multi-kilobyte trace that will be truncated to a couple of sentences anyway.
	CoTStoreWords = 64
	// MaxTurnChars guards against a pathological turn — a pasted file, a huge tool
	// dump folded into a reply — becoming one enormous source.
	MaxTurnChars = 200_000
)

// TurnRef builds the source_ref for a turn pair: the session id plus the seq range
// it covers. Stable and unique, and it keeps §3.4's session cascade a single
// indexed predicate via the separate session_id column.
func TurnRef(sessionID string, startSeq, endSeq int) string {
	return fmt.Sprintf("%s#%d-%d", sessionID, startSeq, endSeq)
}

// TurnPair is one ingestable exchange, assembled from a session's message rows.
type TurnPair struct {
	SessionID string
	// Title is the session title, used in the thread-context prefix.
	Title    string
	StartSeq int
	EndSeq   int
	UserText string
	Reply    string
	// CoT is the turn's hidden reasoning note, if the mode produced one. Indexed,
	// never returned.
	CoT string
	// PrevGist is a few words from the preceding turn, so a terse exchange ("yes, do
	// that one") still carries some topic into its vector.
	PrevGist string
}

// TurnSkip explains why a turn was not ingested. Returned rather than logged so a
// caller can report it: silently dropping turns would make conversation memory look
// simply unreliable.
type TurnSkip struct {
	StartSeq int
	Reason   string
}

// ConversationReport summarizes one session's ingestion.
type ConversationReport struct {
	SessionID      string        `json:"sessionId"`
	Turns          int           `json:"turns"`
	TurnsIngested  int           `json:"turnsIngested"`
	TurnsSkipped   []TurnSkip    `json:"turnsSkipped"`
	TurnsUnchanged int           `json:"turnsUnchanged"`
	ChunksWritten  int           `json:"chunksWritten"`
	SourceIDs      []string      `json:"sourceIds"`
	Duration       time.Duration `json:"duration"`
}

func (r ConversationReport) String() string {
	return fmt.Sprintf("session %s: %d/%d turns ingested (%d unchanged, %d skipped), %d chunks, in %s",
		r.SessionID, r.TurnsIngested, r.Turns, r.TurnsUnchanged, len(r.TurnsSkipped),
		r.ChunksWritten, r.Duration.Round(time.Millisecond))
}

// ExtractTurnPairs assembles turn pairs from a session's messages in seq order.
//
// Three exclusions, each deliberate:
//
//   - **`tool` rows are dropped outright.** They carry retrieved text, including
//     memory's own search results, so indexing them would feed retrieval back into
//     the corpus — duplicate inflation, IDF distortion and recency skew, all silent
//     (§3.3 "Retrieval feedback loops").
//   - **Plan-driven turns are skipped.** Their user text is a synthetic "working on
//     step N" label injected by the frontend, not something the user wrote, and the
//     reply is meaningful only for that step.
//   - **A user message with no reply is skipped.** An in-flight or failed turn has no
//     answer yet; it becomes ingestable when the reply lands.
func ExtractTurnPairs(sessionID, title string, msgs []store.StoredMessage) ([]TurnPair, []TurnSkip) {
	var pairs []TurnPair
	var skips []TurnSkip

	// prevGist carries forward from the previous *ingested* turn.
	prevGist := ""
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "user" {
			continue
		}
		if m.ToolName == "manage_plan" {
			skips = append(skips, TurnSkip{m.Seq, "plan-driven turn: synthetic user text"})
			continue
		}

		// Walk forward to the reply, collecting the turn's cot row and ignoring tool
		// rows. Stop at the next user message: that starts a new turn.
		var reply, cot string
		endSeq := m.Seq
		for j := i + 1; j < len(msgs); j++ {
			n := msgs[j]
			if n.Role == "user" {
				break
			}
			switch n.Role {
			case "cot":
				cot = n.Content
				endSeq = n.Seq
			case "assistant":
				reply = n.Content
				endSeq = n.Seq
			}
			if reply != "" {
				break
			}
		}

		if strings.TrimSpace(m.Content) == "" || strings.TrimSpace(reply) == "" {
			skips = append(skips, TurnSkip{m.Seq, "no reply yet, or empty text"})
			continue
		}
		if len(m.Content)+len(reply) > MaxTurnChars {
			skips = append(skips, TurnSkip{m.Seq, "turn exceeds the size cap"})
			continue
		}

		pairs = append(pairs, TurnPair{
			SessionID: sessionID, Title: title,
			StartSeq: m.Seq, EndSeq: endSeq,
			UserText: m.Content, Reply: reply, CoT: cot, PrevGist: prevGist,
		})
		prevGist = firstWords(m.Content, PrevGistWords)
	}
	return pairs, skips
}

// firstWords returns the leading n whitespace-separated words, collapsed.
func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// TurnText renders the stored, returnable text of a turn pair.
//
// This is what the tool hands back, so it holds the exchange and nothing else — no
// CoT. Roles are labelled because an unlabelled Q&A blob reads as one voice.
func TurnText(p TurnPair) string {
	var b strings.Builder
	b.WriteString("User: ")
	b.WriteString(strings.TrimSpace(p.UserText))
	b.WriteString("\n\nAssistant: ")
	b.WriteString(strings.TrimSpace(p.Reply))
	return b.String()
}

// turnHeading is the returnable provenance line for a turn chunk, standing in for
// a Markdown heading path.
func turnHeading(p TurnPair) string {
	if p.Title == "" {
		return fmt.Sprintf("Conversation › turn %d", p.StartSeq)
	}
	return fmt.Sprintf("%s › turn %d", p.Title, p.StartSeq)
}

// threadContext is the always-available baseline prefix: session title plus the
// preceding turn's gist.
//
// It exists because CoT cannot be relied on — `cot` rows only exist when a
// file-based cot mode is active, and plan-driven turns force the mode to none. So
// the prefix that makes terse turns findable has to work without it, with CoT
// enriching it when present rather than replacing it.
func threadContext(p TurnPair) string {
	parts := make([]string, 0, 2)
	if p.Title != "" {
		parts = append(parts, "Conversation: "+p.Title)
	}
	if p.PrevGist != "" {
		parts = append(parts, "Previously: "+p.PrevGist)
	}
	return strings.Join(parts, " — ")
}

// IngestTurns stores a session's turn pairs, skipping those already stored
// unchanged.
//
// Chunking uses the same heading chunker as Markdown, so a long exchange is split
// rather than truncated at the token budget. The thread-context and CoT prefixes are
// stored per chunk in fields the read paths never return, which is how §3.3's
// indexed-but-not-returned rule survives the fact that embedding happens later, in
// the backfill, from the stored row.
func (ing *Ingester) IngestTurns(ctx context.Context, sessionID, title string, msgs []store.StoredMessage) (ConversationReport, error) {
	started := time.Now()
	rep := ConversationReport{SessionID: sessionID}

	pairs, skips := ExtractTurnPairs(sessionID, title, msgs)
	rep.Turns = len(pairs) + len(skips)
	rep.TurnsSkipped = skips

	for _, p := range pairs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ref := TurnRef(sessionID, p.StartSeq, p.EndSeq)
		text := TurnText(p)
		// The hash covers the CoT too: a turn whose reasoning note changed has a
		// different vector even though its returnable text is identical.
		hash := hashContent(text + "\x00" + p.CoT)

		if existing, found, err := ing.store.FindSource(store.SourceConversation, ref); err != nil {
			return rep, err
		} else if found && existing.ContentHash == hash {
			rep.TurnsUnchanged++
			continue
		}

		heading := turnHeading(p)
		thread := threadContext(p)
		cot := firstWords(p.CoT, CoTStoreWords)

		blocks := ParseBlocks(text)
		chunks, err := ing.chunkDocument(ctx, blocks)
		if err != nil {
			return rep, err
		}

		storeChunks := make([]store.MemoryChunk, 0, len(chunks))
		for _, c := range chunks {
			// A conversation has no author-written headings, so the turn's own
			// provenance line is used instead of whatever the chunker inferred from
			// text the user happened to write with a "#" in front.
			hp := heading
			if c.HeadingPath != "" {
				hp = heading + headingSep + c.HeadingPath
			}
			storeChunks = append(storeChunks, store.MemoryChunk{
				Text:          c.Text,
				HeadingPath:   hp,
				TokenCount:    c.TokenCount,
				CharLen:       len(c.Text),
				ThreadContext: thread,
				CotContext:    cot,
				// Terms cover the prefixes as well as the text. §3.3 calls for CoT to
				// be *embedded and term-indexed*, and embedding it alone would leave a
				// terse turn findable by vector but invisible to BM25 — which is the
				// stronger signal for the distinctive words a CoT note tends to carry.
				//
				// This does inflate tf without inflating the chunk's token_count, so
				// BM25 length normalization treats such a chunk as slightly denser than
				// it is. The prefixes are ~96 tokens against a chunk of up to 416, and
				// the alternative — a term index that disagrees with the vector about
				// what the chunk contains — is worse.
				Terms: ExtractTerms(strings.Join([]string{c.Text, thread, cot}, "\n")),
				// Entities come from the returnable text only. An entity is a claim
				// that something appears in this chunk; CoT can be wrong, and letting
				// it mint entities would put unverifiable names into a signal whose
				// whole value is exactness.
				Entities: ExtractEntities(c.Text, nil, nil),
			})
		}
		if len(storeChunks) == 0 {
			continue
		}

		sourceID, err := ing.store.ReplaceSource(store.MemorySource{
			SourceType:  store.SourceConversation,
			SourceRef:   ref,
			SessionID:   sessionID,
			Title:       title,
			ContentHash: hash,
		}, storeChunks)
		if err != nil {
			return rep, err
		}
		rep.TurnsIngested++
		rep.ChunksWritten += len(storeChunks)
		rep.SourceIDs = append(rep.SourceIDs, sourceID)
	}

	if rep.TurnsIngested > 0 {
		if err := ing.store.MarkStatsDirty(); err != nil {
			return rep, err
		}
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// IngestArtifact stores one artifact, replacing any prior content for the same id.
//
// Artifacts are the app's own long-form output — exactly the material a user asks
// about later ("what did that migration plan say?"), and unlike a conversation turn
// it is a single coherent document, so the Markdown chunker applies directly.
func (ing *Ingester) IngestArtifact(ctx context.Context, art store.Artifact) (int, error) {
	text := strings.TrimSpace(art.Content)
	if text == "" {
		return 0, nil
	}
	hash := hashContent(text)
	ref := art.ID

	if existing, found, err := ing.store.FindSource(store.SourceArtifact, ref); err != nil {
		return 0, err
	} else if found && existing.ContentHash == hash {
		return 0, nil
	}

	blocks := ParseBlocks(text)
	chunks, err := ing.chunkDocument(ctx, blocks)
	if err != nil {
		return 0, err
	}

	title := art.Title
	if title == "" {
		title = "Untitled artifact"
	}
	// The artifact's title and type situate its chunks the way a heading path does
	// for a note: "Migration plan (markdown) › Rollback" is enough to attribute an
	// excerpt without a second lookup.
	prefix := title
	if art.ContentType != "" && art.ContentType != "text" {
		prefix = fmt.Sprintf("%s (%s)", title, art.ContentType)
	}

	storeChunks := make([]store.MemoryChunk, 0, len(chunks))
	for _, c := range chunks {
		hp := prefix
		if c.HeadingPath != "" {
			hp = prefix + headingSep + c.HeadingPath
		}
		storeChunks = append(storeChunks, store.MemoryChunk{
			Text:        c.Text,
			HeadingPath: hp,
			TokenCount:  c.TokenCount,
			CharLen:     len(c.Text),
			Terms:       ExtractTerms(c.Text),
			Entities:    ExtractEntities(c.Text, nil, nil),
		})
	}
	if len(storeChunks) == 0 {
		return 0, nil
	}

	if _, err := ing.store.ReplaceSource(store.MemorySource{
		SourceType:  store.SourceArtifact,
		SourceRef:   ref,
		SessionID:   art.SessionID,
		Title:       title,
		ContentHash: hash,
	}, storeChunks); err != nil {
		return 0, err
	}
	if err := ing.store.MarkStatsDirty(); err != nil {
		return 0, err
	}
	return len(storeChunks), nil
}
