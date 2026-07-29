package memory

import (
	"context"
	"fmt"
	"time"

	"simple-cot-chat/store"
)

// BackfillBatch is how many chunks are embedded and written per round.
const BackfillBatch = 64

// BackfillReport summarizes an embedding backfill.
type BackfillReport struct {
	Chunks    int           `json:"chunks"`
	Batches   int           `json:"batches"`
	Remaining int           `json:"remaining"`
	Duration  time.Duration `json:"duration"`
}

func (r BackfillReport) String() string {
	rate := 0.0
	if r.Duration > 0 {
		rate = float64(r.Chunks) / r.Duration.Seconds()
	}
	return fmt.Sprintf("embedded %d chunks in %d batches (%.1f/sec), %d remaining, in %s",
		r.Chunks, r.Batches, rate, r.Remaining, r.Duration.Round(time.Millisecond))
}

// Backfill embeds every chunk that has no vector yet.
//
// This is what makes ingestion independent of model provisioning: a chunk is
// stored with a NULL embedding when the model is unavailable, and this fills it in
// later. It is also the re-embedding path after a model change, since
// System.recordModel clears vectors when the model identity changes.
//
// The embedded text is not the stored text: heading_path is prepended so the
// vector carries document context, within the split token budget (see
// BuildEmbeddedText). Getting this wrong would produce vectors that do not match
// what retrieval expects.
func (sys *System) Backfill(ctx context.Context, batchSize int, onProgress func(done, total int)) (BackfillReport, error) {
	started := time.Now()
	var rep BackfillReport

	emb := sys.Embedder()
	if emb == nil {
		return rep, &ErrUnavailable{Reason: sys.UnavailableReason()}
	}
	if err := sys.CheckDimensions(); err != nil {
		return rep, err
	}
	if batchSize <= 0 {
		batchSize = BackfillBatch
	}

	// Total outstanding, for progress reporting. Recomputed only once: chunks are
	// not added concurrently, because ingestion and backfill share one queue
	// worker.
	total := 0
	if st, err := sys.Store.MemoryStats(); err == nil {
		total = st.Chunks - st.EmbeddedChunks
	}

	tc := sys.Ingester.tokens
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		pending, err := sys.Store.ChunksMissingEmbedding(batchSize)
		if err != nil {
			return rep, err
		}
		if len(pending) == 0 {
			break
		}

		texts := make([]string, len(pending))
		for i, c := range pending {
			// Passage side: no query prefix. The prefixes come from the stored row —
			// heading path for every chunk, plus thread context and CoT for
			// conversation chunks, which are embedded but never returned (§3.3).
			// BuildEmbeddedText applies the split budget and the CoT sub-cap.
			texts[i] = BuildEmbeddedText(c.HeadingPath, c.ThreadContext, c.CotContext, c.Text, tc)
		}

		vecs, err := emb.EmbedPassages(ctx, texts)
		if err != nil {
			return rep, fmt.Errorf("embed batch: %w", err)
		}
		if len(vecs) != len(pending) {
			return rep, fmt.Errorf("embedder returned %d vectors for %d chunks", len(vecs), len(pending))
		}

		byID := make(map[string][]float32, len(pending))
		for i, c := range pending {
			byID[c.ID] = vecs[i]
		}
		if err := sys.Store.SetChunkEmbeddings(emb.ModelName(), byID); err != nil {
			return rep, err
		}

		rep.Chunks += len(pending)
		rep.Batches++
		if onProgress != nil {
			onProgress(rep.Chunks, total)
		}

		// A short batch means the queue is drained.
		if len(pending) < batchSize {
			break
		}
	}

	if st, err := sys.Store.MemoryStats(); err == nil {
		rep.Remaining = st.Chunks - st.EmbeddedChunks
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// SearchVector returns the nearest chunks to a query by cosine distance, computed
// in SQL.
//
// Ranking happens in the database (array_cosine_distance over FLOAT[384]) rather
// than in Go: DuckDB's vectorized engine beats a hand-rolled loop, and it avoids
// holding the whole corpus's vectors in memory. This is the vector arm of §3.5's
// candidate generation; the full fused scorer lands in Phase 4.
func (sys *System) SearchVector(ctx context.Context, query string, limit int) ([]VectorHit, error) {
	emb := sys.Embedder()
	if emb == nil {
		return nil, &ErrUnavailable{Reason: sys.UnavailableReason()}
	}
	if limit <= 0 {
		limit = 10
	}
	qv, err := emb.EmbedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return sys.Store.NearestChunks(qv, limit)
}

// VectorHit is one result of a vector search.
type VectorHit = store.VectorHit
