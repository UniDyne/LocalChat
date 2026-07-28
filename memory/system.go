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
}

// Status reports subsystem state, including why embeddings are off when they are.
func (sys *System) Status() (Status, error) {
	st, err := sys.Store.MemoryStats()
	if err != nil {
		return Status{}, err
	}
	return Status{
		EmbeddingsAvailable: sys.EmbeddingsAvailable(),
		UnavailableReason:   sys.UnavailableReason(),
		TokenCounter:        sys.Ingester.TokenCounterName(),
		Queue:               sys.Queue.Progress(),
		Corpus:              st,
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
