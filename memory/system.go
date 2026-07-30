package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/ollama/ollama/api"
	"simple-cot-chat/store"
)

// Config configures the memory subsystem.
type Config struct {
	// ModelDir overrides model resolution (see FindModel).
	ModelDir string
	// IntraOpThreads caps ONNX Runtime parallelism; 0 picks a safe default.
	IntraOpThreads int
	// BatchSize for embedding; 0 uses the measured default.
	BatchSize int
	// OllamaClient enables the fallback embedder when the local path is
	// unavailable. Optional.
	OllamaClient *api.Client
	// OllamaEmbedModel names the fallback model, e.g. "bge-m3". Optional.
	OllamaEmbedModel string
	// OllamaEmbedDims is the fallback model's width.
	OllamaEmbedDims int
	// Embedder injects an embedder directly, bypassing resolution. Used by tests.
	Embedder Embedder

	// ExtractModel names the model for the LLM entity pass. Separate from the chat
	// model on purpose: extraction is a far easier task than reasoning, so paying
	// 27B rates for it is waste (§3.3). Empty disables the pass.
	ExtractModel string
	// ExtractNumCtx sizes the extraction context window; 0 picks a default. A whole
	// note is much larger than a chat turn, and a too-small window truncates it
	// silently.
	ExtractNumCtx int
	// Extractor injects an extractor directly, bypassing resolution. Used by tests —
	// the guards, attribution and link resolution are where the bugs are, and none of
	// them need a model.
	Extractor EntityExtractor
}

// System owns the memory subsystem: storage, the ingestion queue, the chunker and
// the embedder.
//
// NewSystem never fails. An unprovisioned model or a missing ONNX Runtime leaves
// the system usable — ingestion still runs and chunks are stored without vectors,
// retrieval falls back to the non-vector signals — and the reason is reported
// rather than crashing the app. Memory is opt-in; it must never take the app down.
type System struct {
	Store    *store.Store
	Queue    *Queue
	Ingester *Ingester

	mu          sync.RWMutex
	embedder    Embedder
	unavailable error
	extractor   EntityExtractor
}

// NewSystem wires the subsystem together, degrading gracefully.
func NewSystem(s *store.Store, cfg Config, onProgress func(Progress)) *System {
	sys := &System{
		Store: s,
		Queue: NewQueue(onProgress),
	}
	sys.Ingester = NewIngester(s, nil)

	emb, err := resolveEmbedder(cfg)
	if err != nil {
		sys.unavailable = err
		slog.Warn("memory embeddings unavailable", "reason", err.Error())
	} else {
		sys.embedder = emb
		if err := sys.recordModel(emb); err != nil {
			slog.Warn("could not record embedding model metadata", "error", err)
		}
	}

	// The entity pass is optional and independent: without it, search runs on
	// heuristic entities exactly as it did before Phase 8.
	if cfg.Extractor != nil {
		sys.extractor = cfg.Extractor
	} else if cfg.OllamaClient != nil && cfg.ExtractModel != "" {
		ext, err := NewOllamaExtractor(cfg.OllamaClient, cfg.ExtractModel, cfg.ExtractNumCtx)
		if err != nil {
			slog.Warn("entity extraction unavailable", "reason", err.Error())
		} else {
			sys.extractor = ext
			slog.Info("entity extraction configured", "model", cfg.ExtractModel)
		}
	}
	sys.Queue.Start()
	return sys
}

// resolveEmbedder picks the embedder: an injected one, then in-process ONNX, then
// the Ollama fallback if configured.
func resolveEmbedder(cfg Config) (Embedder, error) {
	if cfg.Embedder != nil {
		return cfg.Embedder, nil
	}

	onnx, onnxErr := NewONNXEmbedder(ONNXConfig{
		ModelDir:       cfg.ModelDir,
		IntraOpThreads: cfg.IntraOpThreads,
		BatchSize:      cfg.BatchSize,
	})
	if onnxErr == nil {
		return onnx, nil
	}

	if cfg.OllamaClient != nil && cfg.OllamaEmbedModel != "" {
		fallback, err := NewOllamaEmbedder(cfg.OllamaClient, cfg.OllamaEmbedModel, cfg.OllamaEmbedDims)
		if err == nil {
			slog.Info("using Ollama embedding fallback",
				"model", cfg.OllamaEmbedModel, "local_reason", onnxErr.Error())
			return fallback, nil
		}
	}
	return nil, onnxErr
}

// recordModel writes the model identity and detects a change. A different model or
// dimension means existing vectors are from another vector space and cannot be
// compared with new ones, so they are cleared and the corpus is queued for
// backfill rather than silently mixed.
func (sys *System) recordModel(emb Embedder) error {
	changed, err := sys.Store.InitMemoryMeta(emb.ModelName(), emb.Dims())
	if err != nil {
		return err
	}
	if err := sys.Store.SetMeta(store.MetaModelRevision, ModelRevision); err != nil {
		return err
	}
	if err := sys.Store.SetMeta(store.MetaModelDigest, ModelDigest); err != nil {
		return err
	}
	if !changed {
		return nil
	}

	st, err := sys.Store.MemoryStats()
	if err != nil {
		return err
	}
	slog.Warn("embedding model changed; clearing vectors and queueing re-embedding",
		"model", emb.ModelName(), "dims", emb.Dims(), "chunks_affected", st.EmbeddedChunks)
	if err := sys.Store.ClearEmbeddings(); err != nil {
		return err
	}
	return nil
}

// EmbeddingsAvailable reports whether vectors can currently be produced.
func (sys *System) EmbeddingsAvailable() bool {
	sys.mu.RLock()
	defer sys.mu.RUnlock()
	return sys.embedder != nil
}

// UnavailableReason explains why embeddings are unavailable, or "" if they are.
func (sys *System) UnavailableReason() string {
	sys.mu.RLock()
	defer sys.mu.RUnlock()
	if sys.embedder != nil {
		return ""
	}
	if sys.unavailable == nil {
		return "embeddings not configured"
	}
	return sys.unavailable.Error()
}

// Embedder returns the active embedder, or nil.
func (sys *System) Embedder() Embedder {
	sys.mu.RLock()
	defer sys.mu.RUnlock()
	return sys.embedder
}

// Extractor returns the active entity extractor, or nil when the LLM pass is not
// configured.
func (sys *System) Extractor() EntityExtractor {
	sys.mu.RLock()
	defer sys.mu.RUnlock()
	return sys.extractor
}

// SetExtractor installs an extractor after construction.
func (sys *System) SetExtractor(ext EntityExtractor) {
	sys.mu.Lock()
	sys.extractor = ext
	sys.mu.Unlock()
}

// EnqueueEnrichment queues the LLM entity pass, in batches.
//
// It runs on the same single-worker queue as ingestion — it must, since two concurrent
// writers hit DuckDB write-write conflicts — which creates a tension with the plan's
// "ingestion and search never wait on it": a full pass over a vault is one model call
// per note, so an unbounded job would hold the only worker for hours and every
// conversation turn and directory scan queued behind it would simply wait.
//
// Resolved by processing EnrichBatch sources and then **re-enqueueing itself** if more
// remain. Anything queued in the meantime runs between batches, so ingestion stays
// responsive while enrichment makes steady progress. This works because the pass is
// resumable: each batch picks up exactly where the last left off, and the queue's key
// dedup is released once a job starts running, so the re-enqueue is never dropped.
//
// limit caps a single batch (0 uses EnrichBatch).
func (sys *System) EnqueueEnrichment(limit int, onDone func(EnrichReport)) (bool, error) {
	if sys.Extractor() == nil {
		return false, &ErrUnavailable{Reason: "entity extraction is not configured"}
	}
	if limit <= 0 {
		limit = EnrichBatch
	}
	return sys.Queue.Enqueue(Job{
		Kind: "entities",
		Key:  "entities",
		Run: func(ctx context.Context) error {
			rep, err := sys.Enrich(ctx, limit, nil)
			// Report even on failure: a partial run's guard counts are the most useful
			// diagnostic there is, and discarding them on error would hide exactly the
			// case worth looking at.
			if onDone != nil {
				onDone(rep)
			}
			if err != nil {
				return err
			}
			if rep.SourcesTried > 0 {
				slog.Info("entity enrichment batch complete", "report", rep.String())
			}
			// More to do? Queue the next batch and let anything waiting go first.
			// Guarded on progress rather than on remaining count: a batch that tried
			// sources but completed none would otherwise re-enqueue forever.
			if rep.SourcesTried >= limit && rep.SourcesDone+rep.SourcesFailed > 0 {
				if _, err := sys.EnqueueEnrichment(limit, onDone); err != nil {
					slog.Warn("could not queue the next enrichment batch", "error", err)
				}
			}
			return nil
		},
	})
}

// EnqueueProvisionModel downloads and verifies the embedding model, then installs the
// embedder and backfills vectors for anything already ingested.
//
// On the queue for the usual reason — the backfill it triggers is a bulk write — but the
// download itself is the long part and is deliberately *not* automatic: §3.1a settled
// that an unannounced request to huggingface.co on first launch is the wrong behaviour
// for a local-first app.
//
// The backfill is what makes this useful rather than merely correct: a user who indexed
// their notes before provisioning has chunks with no vectors, and this fills them in
// without a re-ingest.
func (sys *System) EnqueueProvisionModel(cfg Config, onProgress func(ProvisionProgress)) (bool, error) {
	if sys.EmbeddingsAvailable() {
		return false, fmt.Errorf("embeddings are already available")
	}
	// The download runs on its own goroutine, NOT on the queue.
	//
	// The queue has exactly one worker, and a 133 MB fetch can take many minutes — so
	// running it there would park every conversation ingest and directory scan behind a
	// network operation. Phase 7 measured turn latency precisely to keep that worker
	// responsive, and Phase 8 batched enrichment to avoid monopolizing it; a long download
	// belongs even less there. Only the *short* tail — installing the embedder, embedding
	// what was already ingested, building edges — goes through the queue, because that
	// part is a bulk database write and must be serialized.
	go func() {
		ctx := context.Background()
		paths, err := ProvisionModel(ctx, onProgress)
		if err != nil {
			slog.Warn("model provisioning failed", "error", err)
			if onProgress != nil {
				onProgress(ProvisionProgress{File: "model.onnx", Err: err.Error()})
			}
			return
		}
		if _, err := sys.Queue.Enqueue(Job{
			Kind: "provision",
			Key:  "provision",
			Run: func(ctx context.Context) error {
				return sys.installEmbedder(ctx, cfg, paths, onProgress)
			},
		}); err != nil {
			slog.Warn("could not queue post-download embedding", "error", err)
		}
	}()
	return true, nil
}

// installEmbedder is provisioning's tail: bring the embedder up, then embed and link
// whatever was ingested before the model existed.
//
// Separate from the download so the two can live on different goroutines — this half is a
// bulk database write and belongs on the queue; the download half does not.
func (sys *System) installEmbedder(ctx context.Context, cfg Config, paths ModelPaths,
	onProgress func(ProvisionProgress),
) error {
	{
		emb, err := NewONNXEmbedder(ONNXConfig{
			ModelDir:       paths.Dir,
			IntraOpThreads: cfg.IntraOpThreads,
			BatchSize:      cfg.BatchSize,
		})
		if err != nil {
			// The model is on disk but unusable — almost always the ONNX Runtime, not the
			// model, so the reason has to reach the user rather than becoming a generic
			// "provisioning failed".
			if onProgress != nil {
				onProgress(ProvisionProgress{File: "model.onnx", Err: err.Error()})
			}
			return err
		}
		if err := sys.SetEmbedder(emb); err != nil {
			return err
		}
		slog.Info("embedder installed after provisioning", "model", emb.ModelName())
	}

	rep, err := sys.Backfill(ctx, 0, nil)
	if err != nil {
		return err
	}
	if rep.Chunks > 0 {
		slog.Info("embedded previously-ingested chunks", "report", rep.String())
		// Similarity edges need those vectors, and nothing else will build them.
		er, err := sys.BuildEdges(ctx, nil, EdgeParams{})
		if err != nil {
			return err
		}
		slog.Info("edge build after provisioning", "report", er.String())
	}
	if onProgress != nil {
		onProgress(ProvisionProgress{File: "model.onnx", Complete: true, Percent: 100})
	}
	return nil
}

// SetEmbedder installs an embedder after construction — the path taken when the
// user provisions the model from the UI without restarting the app.
func (sys *System) SetEmbedder(emb Embedder) error {
	sys.mu.Lock()
	sys.embedder = emb
	sys.unavailable = nil
	sys.mu.Unlock()
	return sys.recordModel(emb)
}

// Close stops the queue and releases the embedder.
func (sys *System) Close() error {
	sys.Queue.Stop()
	sys.mu.Lock()
	defer sys.mu.Unlock()
	if sys.embedder != nil {
		err := sys.embedder.Close()
		sys.embedder = nil
		return err
	}
	return nil
}

// Status is a UI-facing snapshot.
type Status struct {
	EmbeddingsAvailable bool              `json:"embeddingsAvailable"`
	UnavailableReason   string            `json:"unavailableReason"`
	TokenCounter        string            `json:"tokenCounter"`
	Queue               Progress          `json:"queue"`
	Corpus              store.MemoryStats `json:"corpus"`
	// ExtractModel names the entity-extraction model, or "" when the pass is off.
	ExtractModel string `json:"extractModel"`
	// EntityPass counts sources per enrichment state, so progress is visible rather
	// than inferred from a spinner.
	EntityPass map[string]int `json:"entityPass"`
	// EdgesByKind breaks the graph down per edge kind. The total alone hides the
	// thing worth knowing: a corpus with 40k similarity edges and no link edges is
	// a very different graph from the reverse.
	EdgesByKind map[string]int `json:"edgesByKind"`
}

// Status reports subsystem state, including why embeddings are off when they are.
func (sys *System) Status() (Status, error) {
	st, err := sys.Store.MemoryStats()
	if err != nil {
		return Status{}, err
	}
	byKind, err := sys.Store.EdgeCountsByKind()
	if err != nil {
		return Status{}, err
	}
	passes, err := sys.Store.CountSourcesByEntityPass()
	if err != nil {
		return Status{}, err
	}
	extModel := ""
	if ext := sys.Extractor(); ext != nil {
		extModel = ext.ModelName()
	}
	return Status{
		EmbeddingsAvailable: sys.EmbeddingsAvailable(),
		UnavailableReason:   sys.UnavailableReason(),
		TokenCounter:        sys.Ingester.TokenCounterName(),
		Queue:               sys.Queue.Progress(),
		Corpus:              st,
		EdgesByKind:         byKind,
		ExtractModel:        extModel,
		EntityPass:          passes,
	}, nil
}

// EnqueueDirectoryIngest queues a directory scan followed by an embedding
// backfill, so a freshly ingested vault becomes searchable by vector without a
// second user action.
func (sys *System) EnqueueDirectoryIngest(root string, onIngest func(IngestReport)) (bool, error) {
	return sys.Queue.Enqueue(Job{
		Kind: "directory",
		Key:  root,
		Run: func(ctx context.Context) error {
			rep, err := sys.Ingester.IngestDirectory(ctx, root, nil)
			if err != nil {
				return err
			}
			slog.Info("directory ingest complete", "root", root, "report", rep.String())
			if onIngest != nil {
				onIngest(rep)
			}
			if sys.EmbeddingsAvailable() {
				br, err := sys.Backfill(ctx, 0, nil)
				if err != nil {
					return err
				}
				slog.Info("embedding backfill complete", "report", br.String())
			}
			// Edges come last because the similarity pass needs the vectors the
			// backfill just wrote. The link and sequential passes do not, so an
			// unprovisioned model still yields a traversable graph — just without
			// its similarity edges.
			//
			// Incremental when the run changed only part of the vault, full when it
			// is a first ingest. The distinction matters: a full pass is O(chunks ×
			// corpus) in the similarity stage, which is minutes on a vault and
			// entirely wasted after a two-file edit.
			switch {
			case rep.FilesIngested == 0 && rep.FilesDeleted == 0:
				// Nothing changed. Rebuilding would burn the whole similarity pass to
				// arrive at the graph already stored, which is precisely the no-op a
				// re-scan is supposed to be.
				slog.Info("no changes; edges left as they are", "root", root)
				return nil
			case rep.Incremental() || rep.FilesDeleted > 0:
				er, err := sys.BuildEdgesIncremental(ctx, rep, EdgeParams{})
				if err != nil {
					return err
				}
				slog.Info("incremental edge build complete", "report", er.String())
				return nil
			}
			er, err := sys.BuildEdges(ctx, sys.Ingester.Links, EdgeParams{})
			if err != nil {
				return err
			}
			slog.Info("edge build complete", "report", er.String())
			return nil
		},
	})
}

// EnqueueTurnIngest queues one session's conversation turns for ingestion, then
// embeds and links what it wrote.
//
// Keyed by session id, so a rapid series of turns coalesces into one job instead of
// piling up: `Queue.Enqueue` is a no-op when the key is already queued, and
// IngestTurns skips turns already stored unchanged, so the coalesced job simply
// picks up whatever has accumulated.
//
// This must never run on the chat turn's goroutine: ingesting inline would put
// chunking and embedding in the path of the reply the user is waiting for, and behind
// the store's write mutex, in the path of the next SaveMessage too. Measured at
// 7 µs to enqueue against 1.79 s to ingest 40 turns.
func (sys *System) EnqueueTurnIngest(sessionID, title string, load func() ([]store.StoredMessage, error)) (bool, error) {
	if sessionID == "" {
		return false, fmt.Errorf("session id is required")
	}
	return sys.Queue.Enqueue(Job{
		Kind: "conversation",
		Key:  "conversation:" + sessionID,
		Run: func(ctx context.Context) error {
			msgs, err := load()
			if err != nil {
				return err
			}
			rep, err := sys.Ingester.IngestTurns(ctx, sessionID, title, msgs)
			if err != nil {
				return err
			}
			if rep.TurnsIngested == 0 {
				return nil
			}
			slog.Info("conversation ingest complete", "report", rep.String())
			return sys.finishIngest(ctx, IngestReport{ChangedSourceIDs: rep.SourceIDs})
		},
	})
}

// EnqueueArtifactIngest queues one artifact for ingestion.
func (sys *System) EnqueueArtifactIngest(load func() (store.Artifact, error), artifactID string) (bool, error) {
	if artifactID == "" {
		return false, fmt.Errorf("artifact id is required")
	}
	return sys.Queue.Enqueue(Job{
		Kind: "artifact",
		Key:  "artifact:" + artifactID,
		Run: func(ctx context.Context) error {
			art, err := load()
			if err != nil {
				return err
			}
			n, err := sys.Ingester.IngestArtifact(ctx, art)
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			slog.Info("artifact ingested", "id", art.ID, "title", art.Title, "chunks", n)
			src, found, err := sys.Store.FindSource(store.SourceArtifact, art.ID)
			if err != nil || !found {
				return err
			}
			return sys.finishIngest(ctx, IngestReport{
				ChangedRefs: []string{art.ID}, ChangedSourceIDs: []string{src.ID},
			})
		},
	})
}

// finishIngest embeds and re-links whatever an ingest wrote.
//
// Shared by the conversation and artifact paths, and it runs the *incremental* edge
// rebuild: a single new turn must not trigger a full-corpus similarity pass, which
// on a vault-scale corpus would be minutes of work per chat message.
func (sys *System) finishIngest(ctx context.Context, rep IngestReport) error {
	if sys.EmbeddingsAvailable() {
		br, err := sys.Backfill(ctx, 0, nil)
		if err != nil {
			return err
		}
		if br.Chunks > 0 {
			slog.Info("embedding backfill complete", "report", br.String())
		}
	}
	er, err := sys.BuildEdgesIncremental(ctx, rep, EdgeParams{})
	if err != nil {
		return err
	}
	if er.Total() > 0 {
		slog.Info("incremental edge build complete", "report", er.String())
	}
	return nil
}

// EnqueueEdgeBuild queues an edge rebuild on its own — the path taken after the
// model is provisioned for a corpus ingested without it, where the sequential and
// link edges exist but the similarity graph does not.
//
// links may be nil; the link pass is then skipped rather than clearing the link
// edges a previous ingest resolved.
func (sys *System) EnqueueEdgeBuild(links []PendingLink) (bool, error) {
	return sys.Queue.Enqueue(Job{
		Kind: "edges",
		Key:  "edges",
		Run: func(ctx context.Context) error {
			rep, err := sys.BuildEdges(ctx, links, EdgeParams{})
			if err != nil {
				return err
			}
			slog.Info("edge build complete", "report", rep.String())
			return nil
		},
	})
}

// EnqueueBackfill queues an embedding backfill on its own — the path taken after
// the model is provisioned for a corpus that was ingested without it.
func (sys *System) EnqueueBackfill() (bool, error) {
	if !sys.EmbeddingsAvailable() {
		return false, &ErrUnavailable{Reason: sys.UnavailableReason()}
	}
	return sys.Queue.Enqueue(Job{
		Kind: "backfill",
		Key:  "backfill",
		Run: func(ctx context.Context) error {
			rep, err := sys.Backfill(ctx, 0, nil)
			if err != nil {
				return err
			}
			slog.Info("embedding backfill complete", "report", rep.String())
			return nil
		},
	})
}

// storedDims reads the dimension recorded in memory_meta, or 0.
func (sys *System) storedDims() int {
	v, err := sys.Store.GetMeta(store.MetaEmbedDims)
	if err != nil || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// CheckDimensions verifies the active embedder matches the schema and the recorded
// metadata. A width mismatch is a schema migration, not a re-embed, so it is
// reported rather than worked around.
func (sys *System) CheckDimensions() error {
	emb := sys.Embedder()
	if emb == nil {
		return nil
	}
	if emb.Dims() != store.EmbedDim {
		return fmt.Errorf("embedder produces %d dims but the schema stores FLOAT[%d]; "+
			"changing width requires a schema migration", emb.Dims(), store.EmbedDim)
	}
	if d := sys.storedDims(); d != 0 && d != emb.Dims() {
		return fmt.Errorf("stored vectors are %d dims but the embedder produces %d", d, emb.Dims())
	}
	return nil
}
