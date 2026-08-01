package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/ollama/ollama/api"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"simple-cot-chat/memory"
	"simple-cot-chat/store"
)

// Reserved chain-of-thought mode names. All other modes are discovered from
// markdown files in the cot directory.
const (
	CotModeNone    = "none"
	CotModeBuiltIn = "built-in"
)

// config holds the Ollama endpoint and preferred model loaded from config.json.
type config struct {
	OllamaEndpoint string `json:"ollama_endpoint"`
	Model          string `json:"model"`
	// ExtractModel names the model for memory's LLM entity pass. Separate from Model
	// on purpose: extraction is a far easier task than chat, so it should run on a
	// smaller model. Empty leaves the pass disabled, and memory works without it.
	ExtractModel string `json:"extract_model"`
	// DBPath overrides the default sessions.db location. Empty means use the
	// default (next to the executable, or in the working directory).
	DBPath string `json:"db_path"`
}

func loadConfig() *config {
	cfg := &config{
		OllamaEndpoint: "http://localhost:11434",
		Model:          "qwen3.5:9b",
	}

	execPath, err := os.Executable()
	if err == nil {
		// confDir() is searched first so config.json can live alongside
		// SYSTEM.md and the other runtime files in conf/. The executable
		// directory and working directory remain as fallbacks so existing
		// installations keep working without moving their config.json.
		for _, dir := range []string{confDir(), filepath.Dir(execPath), "."} {
			path := filepath.Join(dir, "config.json")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				slog.Debug("config not found", "path", path, "error", readErr)
				continue
			}
			var loaded config
			if parseErr := json.Unmarshal(data, &loaded); parseErr != nil {
				slog.Warn("failed to parse config.json", "error", parseErr)
				continue
			}
			cfg = &loaded
			break
		}
	}

	return cfg
}

// Timeouts for talking to Ollama.
//
// The distinction matters: a *connect* can be bounded tightly, a *generation* cannot.
const (
	// ollamaDialTimeout bounds establishing the TCP connection. This is the one that
	// matters for a misconfigured or unreachable endpoint: a host that drops SYNs rather
	// than refusing them otherwise hangs for the OS retry budget — roughly two minutes on
	// Linux — with the calling goroutine parked in net/http's transport.
	ollamaDialTimeout = 5 * time.Second
	// ollamaShortCallTimeout bounds the management calls (listing models, health checks).
	// They are metadata requests; if one is slow, something is wrong.
	ollamaShortCallTimeout = 10 * time.Second
)

// newOllamaHTTPClient builds the HTTP client for Ollama.
//
// Note what is deliberately *not* set: `http.Client.Timeout`. That would be the obvious
// choice and it is wrong here, because it bounds the entire request *including reading
// the response body* — which for a chat completion is the whole generation. A 60-second
// client timeout would silently truncate any reply that took longer to produce.
//
// So the bound goes on the connection instead. A dial timeout fixes the case that
// actually hangs the UI (an endpoint that is not there) while leaving a slow generation
// free to take as long as it takes.
//
// ResponseHeaderTimeout is left unset for the same class of reason: Ollama may not send
// response headers until it has loaded the model into memory, which for a large model on
// a cold start is legitimately minutes.
func newOllamaHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   ollamaDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// shortOllamaCall derives a bounded context for a metadata request.
//
// Chat completions deliberately do not use this: they run on the application context,
// because a generation has no sensible upper bound. Only the calls that should be fast
// are made to fail fast.
func (a *App) shortOllamaCall() (context.Context, context.CancelFunc) {
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, ollamaShortCallTimeout)
}

// App holds the Ollama client, active configuration, and DuckDB session store.
type App struct {
	ctx    context.Context
	cli    *api.Client
	addr   string
	model  string
	mode   string
	dbPath string // empty → use store.Open default
	sess   *store.Store

	// workDirMu guards workDir, which is read from the tool-calling loop
	// (toolRegistry, on every chat turn) and written from the frontend's
	// directory picker — two different goroutines under Wails' bindings.
	workDirMu sync.RWMutex
	// workDir is the directory the file tools (list/read/write/update) are
	// sandboxed to. Empty means no directory is selected, which disables the
	// file tools entirely — see hasWorkDir/toolRegistry.
	workDir string

	// mem is the memory subsystem. Non-nil once startup has run; embeddings may
	// still be unavailable inside it, which is a reported state rather than an
	// error — see memory.System.
	//
	// memMu guards it, because the frontend can call a memory RPC before startup has
	// finished building it: Wails serves the UI while OnStartup is still running, and
	// NewSystem loads a 133 MB model. Without the lock that is a data race; without
	// memInitErr the UI cannot tell "still starting" from "failed", and it latched the
	// first error forever.
	memMu      sync.RWMutex
	mem        *memory.System
	memInitErr error

	// extractModel is the model memory's entity pass uses, from config.json. Empty
	// leaves that pass disabled.
	extractModel string
}

// NewApp creates a new App with defaults.
func NewApp() *App {
	cfg := loadConfig()
	return &App{
		addr: cfg.OllamaEndpoint, model: cfg.Model, mode: CotModeNone,
		extractModel: cfg.ExtractModel, dbPath: cfg.DBPath,
	}
}

// startup is called when the app starts — saves context and initializes Ollama client + DB.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// OnStartup runs after the GUI toolkit is up, so a handler lost between main() entry
	// and here was clobbered by GTK/WebKit or something they pull in.
	probeSignals("startup: GUI is up")

	addr := os.Getenv("OLLAMA_HOST")
	if addr == "" {
		addr = a.addr
	}

	u, err := url.Parse(addr)
	if err != nil {
		slog.Error("invalid Ollama address", "addr", addr, "error", err)
		a.setMemInitErr(fmt.Errorf("invalid Ollama address %q: %w", addr, err))
		return
	}

	a.cli = api.NewClient(u, newOllamaHTTPClient())
	slog.Info("Ollama client ready", "addr", addr)

	probeSignals("startup: before store.Open")
	var ds *store.Store
	if a.dbPath != "" {
		ds, err = store.OpenAt(a.dbPath)
	} else {
		ds, err = store.Open()
	}
	if err != nil {
		slog.Error("db init failed", "error", err)
		a.setMemInitErr(fmt.Errorf("database could not be opened: %w", err))
		return
	}
	a.sess = ds
	probeSignals("startup: after store.Open")
	slog.Info("session store initialized")

	// Memory never blocks startup. NewSystem reports an unprovisioned model or a
	// missing ONNX Runtime as a state rather than an error, so the app is fully
	// usable either way and retrieval simply falls back to its non-vector signals.
	mem := memory.NewSystem(ds, memory.Config{
		OllamaClient: a.cli,
		ExtractModel: a.extractModel,
	}, func(p memory.Progress) {
		wailsruntime.EventsEmit(a.ctx, "memory:progress", p)
	})
	a.memMu.Lock()
	a.mem = mem
	a.memMu.Unlock()

	probeSignals("startup: after memory.NewSystem")
	if mem.EmbeddingsAvailable() {
		slog.Info("memory ready", "chunker", mem.Ingester.ChunkerName(),
			"tokenizer", mem.Ingester.TokenCounterName())
	} else {
		slog.Warn("memory ready without embeddings", "reason", mem.UnavailableReason())
	}
	// Tells a UI that rendered during startup to try again. Without it the Memory tab
	// shows whatever it found on its single attempt, which during a slow model load is
	// "not initialized" — permanently.
	wailsruntime.EventsEmit(a.ctx, "memory:ready", true)
}

// setMemInitErr records why startup could not build memory, so the UI can show the
// real cause. Both early returns in startup are silent to the frontend otherwise.
func (a *App) setMemInitErr(err error) {
	a.memMu.Lock()
	a.memInitErr = err
	a.memMu.Unlock()
	if a.ctx != nil {
		wailsruntime.EventsEmit(a.ctx, "memory:ready", false)
	}
}

// memory returns the subsystem, or an error explaining why it is not available yet.
//
// Every memory RPC goes through this. The distinction it draws matters to the UI: a
// subsystem that is *still starting* warrants a retry, while one that failed to build
// warrants showing the reason and stopping. Returning the same "not initialized" for
// both is what made a startup race look like a permanent breakage.
func (a *App) memory() (*memory.System, error) {
	a.memMu.RLock()
	defer a.memMu.RUnlock()
	if a.mem != nil {
		return a.mem, nil
	}
	if a.memInitErr != nil {
		return nil, fmt.Errorf("memory could not start: %w", a.memInitErr)
	}
	return nil, errMemoryStarting
}

// errMemoryStarting is the retryable case: startup has not reached memory yet.
var errMemoryStarting = errors.New("memory is still starting")

// MemoryReady reports whether the subsystem is up, so the UI can poll cheaply while
// startup finishes rather than guessing from a failed call.
func (a *App) MemoryReady() bool {
	a.memMu.RLock()
	defer a.memMu.RUnlock()
	return a.mem != nil
}

// shutdown releases the memory subsystem. Wired from main.go's OnShutdown.
func (a *App) shutdown(context.Context) {
	if a.mem != nil {
		if err := a.mem.Close(); err != nil {
			slog.Warn("memory shutdown", "error", err)
		}
	}
	if a.sess != nil {
		if err := a.sess.Close(); err != nil {
			slog.Warn("store shutdown", "error", err)
		}
	}
}

// hasMemory reports whether memory holds anything worth searching. Gates the
// search_memory tool: an empty corpus can only return nothing.
func (a *App) hasMemory() bool {
	if a.mem == nil {
		return false
	}
	st, err := a.mem.Store.MemoryStats()
	if err != nil {
		return false
	}
	return st.Chunks > 0
}

// MemoryStatus reports subsystem state for the UI, including why embeddings are off
// when they are.
func (a *App) MemoryStatus() (memory.Status, error) {
	mem, err := a.memory()
	if err != nil {
		return memory.Status{}, err
	}
	return mem.Status()
}

// SelectMemoryDirectory opens a directory picker and queues the chosen folder for
// ingestion into memory.
//
// Deliberately separate from SelectDirectory, which sets the *file tools* sandbox.
// They are different concepts that happen to both pick a folder: the file-tool root is
// where the model may read and write, while this is a corpus to index. Reusing one
// picker would mean indexing a vault silently granted write access to it, which is not
// a trade a user should make by accident.
//
// Returns the chosen directory, or "" if the dialog was cancelled.
func (a *App) SelectMemoryDirectory() (string, error) {
	// Checked before opening the dialog, so a user is not asked to pick a folder that
	// then cannot be indexed.
	if _, err := a.memory(); err != nil {
		return "", err
	}
	dir, err := wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Select a folder of Markdown notes to index",
	})
	if err != nil {
		return "", fmt.Errorf("open directory dialog: %w", err)
	}
	if dir == "" {
		return "", nil
	}
	if _, err := a.IngestDirectory(dir); err != nil {
		return dir, err
	}
	return dir, nil
}

// ProvisionMemoryModel downloads and verifies the embedding model, then turns semantic
// search on without a restart.
//
// User-initiated, per §3.1a: memory is opt-in and an unannounced 133 MB request to
// huggingface.co on first launch would be surprising in a local-first app. Progress
// arrives on the `memory:provision` event channel, including the failure reason — which
// matters because the most likely failure is not the download but the ONNX Runtime being
// missing or too old, and that needs a different fix.
func (a *App) ProvisionMemoryModel() (bool, error) {
	mem, err := a.memory()
	if err != nil {
		return false, err
	}
	return mem.EnqueueProvisionModel(memory.Config{}, func(p memory.ProvisionProgress) {
		wailsruntime.EventsEmit(a.ctx, "memory:provision", p)
	})
}

// MemoryModelInfo describes what provisioning would fetch and where it would go, so the
// UI can state the size and destination before asking for a 133 MB download rather than
// after starting one.
type MemoryModelInfo struct {
	Name      string `json:"name"`
	Revision  string `json:"revision"`
	Bytes     int64  `json:"bytes"`
	TargetDir string `json:"targetDir"`
	Present   bool   `json:"present"`
}

// MemoryModelInfo reports the model's identity and whether it is already present.
func (a *App) GetMemoryModelInfo() (MemoryModelInfo, error) {
	dir, err := memory.ModelTargetDir()
	if err != nil {
		return MemoryModelInfo{}, err
	}
	info := MemoryModelInfo{
		Name: memory.ModelDirName, Revision: memory.ModelRevision,
		Bytes: memory.ModelBytes, TargetDir: dir,
	}
	if _, err := memory.FindModel(); err == nil {
		info.Present = true
	}
	return info, nil
}

// MemorySources lists what has been indexed, newest first, so the corpus is
// inspectable without opening the database.
func (a *App) MemorySources() ([]store.MemorySource, error) {
	mem, err := a.memory()
	if err != nil {
		return nil, err
	}
	return mem.Store.ListSources()
}

// ForgetMemorySource deletes one source and everything derived from it.
func (a *App) ForgetMemorySource(sourceID string) error {
	mem, err := a.memory()
	if err != nil {
		return err
	}
	if sourceID == "" {
		return fmt.Errorf("source id is required")
	}
	return mem.Store.DeleteSource(sourceID)
}

// ForgetMemoryFolder deletes every indexed note under a folder.
//
// The counterpart to indexing the wrong folder, and the unit the UI works in — a vault
// is thousands of sources, so per-source deletion is not an undo a person can use.
// Without this the only way to reverse an ingest is to edit the database by hand, which
// is exactly what Phase 9 exists to make unnecessary.
//
// Matching is by the stored absolute path, so a folder that has since been renamed on
// disk is still forgettable: the rows remember where they came from.
func (a *App) ForgetMemoryFolder(path string) (int, error) {
	mem, err := a.memory()
	if err != nil {
		return 0, err
	}
	if path == "" {
		return 0, fmt.Errorf("folder path is required")
	}
	sources, err := mem.Store.ListSources()
	if err != nil {
		return 0, err
	}
	prefix := strings.TrimRight(path, `/\`)
	removed := 0
	for _, s := range sources {
		if s.SourceType != store.SourceDirectory || s.Path == "" {
			continue
		}
		// Prefix match on a path boundary, so "/vault" cannot swallow "/vault-archive".
		rest := strings.TrimPrefix(s.Path, prefix)
		if rest == s.Path || (rest != "" && rest[0] != '/' && rest[0] != '\\') {
			continue
		}
		if err := mem.Store.DeleteSource(s.ID); err != nil {
			return removed, err
		}
		removed++
	}
	slog.Info("forgot indexed folder", "path", prefix, "sources_removed", removed)
	return removed, nil
}

// IngestDirectory queues a directory scan. Returns immediately: the work runs on the
// memory queue's single worker so it cannot stall a chat turn.
func (a *App) IngestDirectory(path string) (bool, error) {
	mem, err := a.memory()
	if err != nil {
		return false, err
	}
	if path == "" {
		return false, fmt.Errorf("no directory given")
	}
	return mem.EnqueueDirectoryIngest(path, func(rep memory.IngestReport) {
		wailsruntime.EventsEmit(a.ctx, "memory:ingested", rep)
	})
}

// SearchMemoryManual runs a search for the UI. Separate from the tool so retrieval
// quality can be inspected directly, which is also the control for measuring how
// often the model actually invokes the tool.
func (a *App) SearchMemoryManual(query string, limit int) ([]memory.Result, error) {
	res, err := a.SearchMemoryTuned(query, limit, MemorySearchTuning{})
	if err != nil {
		return nil, err
	}
	return res.Results, nil
}

// MemorySearchTuning exposes the retrieval knobs to the UI.
//
// Zero fields mean "use the defaults", so the common case sends nothing. The point is
// that the weights are *not settled* — they were tuned in Phase 4 against an entity
// signal that has since changed character — and §3.5 is explicit that without a visible
// score breakdown, tuning them is guesswork. Making them adjustable next to the
// breakdown is what turns that guesswork into an experiment a user can run.
type MemorySearchTuning struct {
	BM25   float64 `json:"bm25"`
	Vector float64 `json:"vector"`
	Entity float64 `json:"entity"`
	Ngram  float64 `json:"ngram"`
	// Mode is "weighted" or "rrf"; empty uses the default.
	Mode string `json:"mode"`
	// Expand toggles the graph walk. Defaults on, matching the tool.
	Expand *bool `json:"expand"`
	// SourceTypes restricts to directory|conversation|artifact; empty means all.
	SourceTypes []string `json:"sourceTypes"`
}

// MemorySearchResponse pairs results with the report that explains them.
//
// The report is the interesting half for a user testing retrieval: it says how many
// candidates each arm produced, which query entities were recognized, what the graph
// walk did, and why the vector arm was skipped when it was. A bare result list cannot
// distinguish "nothing matched" from "embeddings are unavailable".
type MemorySearchResponse struct {
	Results []memory.Result     `json:"results"`
	Report  memory.SearchReport `json:"report"`
}

// SearchMemoryTuned runs a search with explicit knobs and returns the full report.
func (a *App) SearchMemoryTuned(query string, limit int, t MemorySearchTuning) (MemorySearchResponse, error) {
	mem, err := a.memory()
	if err != nil {
		return MemorySearchResponse{}, err
	}
	if limit <= 0 {
		limit = 8
	}
	opts := memory.SearchOptions{
		Limit: limit, Explain: true, Expand: true,
		Mode: memory.FusionMode(t.Mode), SourceTypes: t.SourceTypes,
	}
	if t.Expand != nil {
		opts.Expand = *t.Expand
	}
	// All four at zero is the zero value, which withDefaults reads as "unset". Any
	// nonzero weight means the caller is driving.
	if t.BM25 != 0 || t.Vector != 0 || t.Entity != 0 || t.Ngram != 0 {
		opts.Weights = memory.Weights{
			BM25: t.BM25, Vector: t.Vector, Entity: t.Entity, Ngram: t.Ngram,
		}
	}
	results, rep, err := mem.Search(context.Background(), query, opts)
	if err != nil {
		return MemorySearchResponse{}, err
	}
	return MemorySearchResponse{Results: results, Report: rep}, nil
}

// DefaultMemoryWeights reports the current defaults, so the UI's tuning controls start
// from what the system actually uses rather than from hardcoded copies that drift.
func (a *App) DefaultMemoryWeights() MemorySearchTuning {
	w := memory.DefaultWeights()
	return MemorySearchTuning{
		BM25: w.BM25, Vector: w.Vector, Entity: w.Entity, Ngram: w.Ngram,
		Mode: string(memory.FusionWeighted),
	}
}

// IndexExistingHistory indexes the sessions and artifacts that already exist.
//
// Phase 7 wired ingestion to *events* — a turn completing, an artifact being created —
// which means a user who had history before memory existed has none of it indexed, and
// never will: nothing replays the past. That reads as "memory doesn't work for my
// existing chats", which is exactly right, and it is the first thing anyone with an
// established database hits.
//
// Idempotent by construction, so running it repeatedly is safe and cheap: every turn
// and artifact is content-hashed, and an unchanged one is skipped. Queued as a single
// job on the same single worker, so it cannot race an ongoing ingest.
func (a *App) IndexExistingHistory() (bool, error) {
	mem, err := a.memory()
	if err != nil {
		return false, err
	}
	sessions, err := a.sess.GetSessions()
	if err != nil {
		return false, err
	}
	return mem.Queue.Enqueue(memory.Job{
		Kind: "backfill-history",
		Key:  "backfill-history",
		Run: func(ctx context.Context) error {
			var turns, artifacts int
			for _, sess := range sessions {
				if err := ctx.Err(); err != nil {
					return err
				}
				msgs, err := a.sess.GetMessages(sess.ID)
				if err != nil {
					slog.Warn("skipping session during history backfill",
						"session", sess.ID, "error", err)
					continue
				}
				rep, err := mem.Ingester.IngestTurns(ctx, sess.ID, sess.Title, msgs)
				if err != nil {
					slog.Warn("history backfill failed for a session",
						"session", sess.ID, "error", err)
					continue
				}
				turns += rep.TurnsIngested

				metas, err := a.sess.GetArtifactsForSession(sess.ID)
				if err != nil {
					continue
				}
				for _, m := range metas {
					art, err := a.sess.GetArtifact(m.ID)
					if err != nil {
						continue
					}
					if n, err := mem.Ingester.IngestArtifact(ctx, art); err != nil {
						slog.Warn("history backfill failed for an artifact",
							"artifact", m.ID, "error", err)
					} else if n > 0 {
						artifacts++
					}
				}
			}
			slog.Info("history backfill complete",
				"sessions", len(sessions), "turns", turns, "artifacts", artifacts)
			wailsruntime.EventsEmit(a.ctx, "memory:backfilled", map[string]any{
				"sessions": len(sessions), "turns": turns, "artifacts": artifacts,
			})
			// Embed and link what was written, the same way a directory ingest does.
			if mem.EmbeddingsAvailable() {
				if _, err := mem.Backfill(ctx, 0, nil); err != nil {
					return err
				}
			}
			_, err := mem.BuildEdges(ctx, nil, memory.EdgeParams{})
			return err
		},
	})
}

// UnindexedHistoryCount reports how many sessions and artifacts exist but have no
// memory source, so the UI can offer the backfill only when there is something to do.
func (a *App) UnindexedHistoryCount() (int, error) {
	mem, err := a.memory()
	if err != nil {
		return 0, err
	}
	sessions, err := a.sess.GetSessions()
	if err != nil {
		return 0, err
	}
	indexed := map[string]bool{}
	sources, err := mem.Store.ListSources()
	if err != nil {
		return 0, err
	}
	for _, s := range sources {
		if s.SessionID != "" {
			indexed[s.SessionID] = true
		}
	}
	n := 0
	for _, sess := range sessions {
		// MessageCount 0 means an empty session, which has nothing to index.
		if sess.MessageCount > 0 && !indexed[sess.ID] {
			n++
		}
	}
	return n, nil
}

// EnrichMemoryEntities queues the LLM entity pass over sources that have not had it.
//
// User-initiated rather than automatic, unlike the per-turn ingestion of Phase 7: the
// pass makes one model call per note, so on a real vault it is minutes-to-hours of
// background work — not something to start unasked.
//
// Returns as soon as the first batch is queued. The pass then chains batches through
// the queue until nothing is pending, yielding to ingestion between them, so this is a
// "start it" call rather than a "run it all" call. limit caps one batch; 0 uses the
// default.
func (a *App) EnrichMemoryEntities(limit int) (bool, error) {
	mem, err := a.memory()
	if err != nil {
		return false, err
	}
	return mem.EnqueueEnrichment(limit, func(rep memory.EnrichReport) {
		wailsruntime.EventsEmit(a.ctx, "memory:enriched", rep)
	})
}

// RollbackMemoryEntityEnrichment removes every association the LLM tier produced and
// marks each source pending again.
//
// Exposed because the `extractor` provenance column exists precisely so this is
// possible: if a model turns out to extract badly, the whole tier can go without
// disturbing the heuristic and tag entities search has always used.
func (a *App) RollbackMemoryEntityEnrichment() (int64, error) {
	mem, err := a.memory()
	if err != nil {
		return 0, err
	}
	n, err := mem.Store.DeleteChunkEntitiesByExtractor(memory.ExtractorLLM)
	if err != nil {
		return 0, err
	}
	if err := mem.Store.ResetEntityPass(); err != nil {
		return n, err
	}
	slog.Info("rolled back the LLM entity tier", "associations_removed", n)
	return n, nil
}

// RebuildMemoryEdges queues an edge rebuild. Exposed because the similarity pass
// needs vectors: a corpus ingested before the model was provisioned has its
// sequential and link edges but no similarity graph, and this is what fills it in
// without re-ingesting.
//
// Link edges are not rebuilt here — resolving them needs the vault index that only
// exists during a directory walk. Re-run the ingest to refresh those.
func (a *App) RebuildMemoryEdges() (bool, error) {
	mem, err := a.memory()
	if err != nil {
		return false, err
	}
	return mem.EnqueueEdgeBuild(nil)
}

// GetModel returns the currently selected model name.
func (a *App) GetModel() string {
	return a.model
}

// SetModel changes the active model for chat completions.
func (a *App) SetModel(name string) error {
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	a.model = name
	slog.Info("model changed", "model", a.model)
	return nil
}

// ListModels returns all locally available model names from Ollama.
func (a *App) ListModels() ([]string, error) {
	if a.cli == nil {
		return nil, fmt.Errorf("Ollama client not initialized")
	}

	ctx, cancel := a.shortOllamaCall()
	defer cancel()

	resp, err := a.cli.List(ctx)
	if err != nil {
		// Reported rather than retried, and phrased so the endpoint is visible: the
		// overwhelmingly common cause is a wrong or unreachable ollama_endpoint, and the
		// model picker showing "unreachable at <addr>" is the difference between a
		// two-second fix and a mystery.
		return nil, fmt.Errorf("could not reach Ollama at %s: %w", a.addr, err)
	}

	names := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// workDirSnapshot returns the currently selected directory (empty if none),
// safe for concurrent use.
func (a *App) workDirSnapshot() string {
	a.workDirMu.RLock()
	defer a.workDirMu.RUnlock()
	return a.workDir
}

// hasWorkDir reports whether a directory is currently selected, i.e. whether
// the file tools should be advertised to the model — see toolRegistry.
func (a *App) hasWorkDir() bool {
	return a.workDirSnapshot() != ""
}

// GetWorkDir returns the currently selected directory for the file tools, or
// "" if none is selected (file tools disabled).
func (a *App) GetWorkDir() string {
	return a.workDirSnapshot()
}

// SelectDirectory opens a native directory picker and, if the user chooses a
// directory, makes it the sandbox root for the file tools (enabling them) and
// returns the chosen path. If the user cancels the dialog, the previous
// selection (if any) is left untouched and its path is returned unchanged.
func (a *App) SelectDirectory() (string, error) {
	dir, err := wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Select a directory to enable file tools",
	})
	if err != nil {
		return "", fmt.Errorf("open directory dialog: %w", err)
	}
	if dir == "" {
		return a.workDirSnapshot(), nil
	}

	a.workDirMu.Lock()
	a.workDir = dir
	a.workDirMu.Unlock()
	slog.Info("file tool directory selected", "dir", dir)
	return dir, nil
}

// ClearDirectory deselects the current directory, disabling the file tools
// until another one is chosen via SelectDirectory.
func (a *App) ClearDirectory() {
	a.workDirMu.Lock()
	a.workDir = ""
	a.workDirMu.Unlock()
	slog.Info("file tool directory cleared")
}

// confDir returns the path to the "conf" directory (holding SYSTEM.md, cot/,
// and skills/), checked next to the executable and in the working directory
// (same convention as loadConfig).
func confDir() string {
	execPath, err := os.Executable()
	if err == nil {
		for _, dir := range []string{filepath.Dir(execPath), "."} {
			p := filepath.Join(dir, "conf")
			if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
				return p
			}
		}
	}
	return "./conf"
}

// cotDir returns the path to the "cot" directory under conf/.
func cotDir() string {
	return filepath.Join(confDir(), "cot")
}

// systemPromptPath returns the path to conf/SYSTEM.md.
func systemPromptPath() string {
	return filepath.Join(confDir(), "SYSTEM.md")
}

// loadSystemPrompt reads the configurable system prompt from conf/SYSTEM.md.
// A missing file is not an error — the system prompt is optional, and the
// file is re-read on every call (same reasoning as loadCotConfig: it may be
// hand-edited while the app is running).
func loadSystemPrompt() (string, error) {
	data, err := os.ReadFile(systemPromptPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ListCotModes returns all registered chain-of-thought modes: the reserved
// "none" and "built-in" options, plus one entry per markdown file found in
// the cot directory.
func (a *App) ListCotModes() ([]string, error) {
	modes := []string{CotModeNone, CotModeBuiltIn}

	entries, err := os.ReadDir(cotDir())
	if err != nil {
		if os.IsNotExist(err) {
			return modes, nil
		}
		return nil, fmt.Errorf("read cot dir: %w", err)
	}

	var custom []string
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".md" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if name == CotModeNone || name == CotModeBuiltIn {
			continue
		}
		custom = append(custom, name)
	}
	sort.Strings(custom)

	return append(modes, custom...), nil
}

// GetCotMode returns the currently active chain-of-thought mode.
func (a *App) GetCotMode() string {
	return a.mode
}

// SetCotMode changes the active chain-of-thought mode for subsequent chat requests.
func (a *App) SetCotMode(name string) error {
	if name == "" {
		return fmt.Errorf("mode name is empty")
	}
	a.mode = name
	slog.Info("cot mode changed", "mode", a.mode)
	return nil
}

// cotDefaultMaxTokens caps the hidden evaluation pass's response length when
// a cot mode's frontmatter omits max_tokens (or the file has no frontmatter
// at all). The eval prompt (cotEvalWrapper) already asks for a single tight
// pass, but some models still loop back and redo the analysis two or three
// times over ("wait, let me reconsider...") or need more room for a genuinely
// complex prompt — this cap is a backstop against the former, sized for the
// common case. Set max_tokens in a specific mode's frontmatter to raise it
// for that mode alone rather than raising the default for every mode.
const cotDefaultMaxTokens = 1024

// cotConfig is a custom (non-reserved) chain-of-thought mode's loaded prompt
// body plus its per-mode settings.
type cotConfig struct {
	Prompt    string
	MaxTokens int
}

// loadCotConfig loads the markdown prompt and frontmatter settings for a
// custom chain-of-thought mode. Frontmatter is optional — a file with none
// gets cotDefaultMaxTokens and its whole content as the prompt body.
func loadCotConfig(name string) (cotConfig, error) {
	data, err := os.ReadFile(filepath.Join(cotDir(), name+".md"))
	if err != nil {
		return cotConfig{}, fmt.Errorf("read cot prompt %q: %w", name, err)
	}

	fields, body := parseCotFrontmatter(string(data))
	cfg := cotConfig{Prompt: body, MaxTokens: cotDefaultMaxTokens}
	if v, ok := fields["max_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTokens = n
		} else {
			slog.Warn("invalid max_tokens in cot frontmatter, using default", "mode", name, "value", v, "default", cotDefaultMaxTokens)
		}
	}
	return cfg, nil
}

// parseCotFrontmatter splits a cot file into its frontmatter fields and body.
// Hand-rolled minimal parser, not a full YAML parser — mirrors
// skill.readFrontmatter (duplicated rather than imported, same reasoning as
// skill.Dir duplicating the cotDir/confDir convention): only flat single-line
// "key: value" pairs between a leading and closing "---" line are supported.
func parseCotFrontmatter(data string) (map[string]string, string) {
	lines := strings.Split(data, "\n")
	fields := map[string]string{}

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fields, data
	}

	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
		if idx := strings.Index(lines[i], ":"); idx > 0 {
			key := strings.TrimSpace(lines[i][:idx])
			val := strings.TrimSpace(lines[i][idx+1:])
			fields[key] = val
		}
	}

	if closeIdx == -1 {
		return map[string]string{}, data
	}

	body := strings.TrimLeft(strings.Join(lines[closeIdx+1:], "\n"), "\n")
	return fields, body
}

// cotEvalWrapper frames a custom cot prompt for the hidden evaluation pass.
// The prompt files themselves are written as reasoning frameworks (numbered
// steps, checklists) and vary in whether they explicitly tell the model not
// to answer yet — several don't. Without an explicit frame, the model reads
// the steps as instructions for the reply itself and answers the user right
// here, which defeats the point of a separate hidden pass. This wrapper is
// prepended/appended around every custom prompt so that framing never
// depends on how a given .md file happens to be worded.
const cotEvalWrapper = `You are in a hidden internal evaluation step, not the conversation itself. The user cannot see this step and has not yet received a reply. Do not write a final answer, greeting, or any text addressed to the user here — only produce internal analysis notes for the framework below. A separate pass afterward will use these notes to compose the actual reply.

Apply this framework to the user's message that follows:

---
%s
---

Produce your analysis now, in a single pass. Do not answer the user's question or request directly — stop at the analysis. Write it once and commit to it: do not restart, redo, or re-run the framework from the top to second-guess or correct an earlier step — if you notice a mistake, correct it in place and move on. Keep it tight — bullet points over prose, no repeating a point already made in an earlier step.`

// cotAnswerWrapper folds the hidden evaluation back in as part of the final
// user turn, rather than the system message. Instructions placed in a system
// message ahead of the whole conversation history compete with everything
// else for the model's attention ("lost in the middle"); folding the notes
// into the last user turn puts them exactly where the model attends most
// strongly, right before it starts generating the reply.
const cotAnswerWrapper = `%s

---
The section above is the prompt to answer. Below are your own hidden internal reasoning notes on it, produced in a prior step the user never saw — use them to inform your answer, but do not mention, quote, or refer to them ("notes", "analysis", etc.) in your reply. Just answer the prompt directly, as if this were the only step.

If the notes describe something that spans multiple distinct steps, or the full deliverable would be long (e.g. a multi-file app, a long document), do not attempt to fit the whole thing into this one reply — use manage_plan to lay out the remaining steps and work through them one at a time, and create_artifact for the actual deliverable content, per your operating instructions. Only write the full thing directly here if it's genuinely short.

%s`

// ChatMessage represents a message in the conversation history.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatTurnMessage is one message persisted during a single SendChat call,
// returned to the frontend so it can render (and let the user pin/unpin) it
// without a session reload.
type ChatTurnMessage struct {
	Seq        int    `json:"seq"`
	Role       string `json:"role"`
	Content    string `json:"content"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	Pinned     bool   `json:"pinned"`
	ToolName   string `json:"toolName,omitempty"`
	ToolArgs   string `json:"toolArgs,omitempty"`
	ToolResult string `json:"toolResult,omitempty"`
}

// ChatTurnResult is everything SendChat persisted for one turn, in
// conversation order (user, optional cot note, optional tool calls, assistant).
type ChatTurnResult struct {
	Messages []ChatTurnMessage `json:"messages"`
}

// SendChat sends the user message to Ollama with optional history and returns
// every message persisted this turn (user, optional cot note, optional tool
// calls, assistant).
//
// queuedByTool is empty for a message the user typed themselves, or the name
// of the tool that queued it (currently always "manage_plan") when the
// frontend is auto-dispatching the current step of an active plan. Queued
// steps always run as if the cot mode were "none" regardless of the mode
// currently selected in the UI — they're follow-up instructions the model
// wrote for itself, not a fresh user question, so there's nothing left to
// evaluate with a cot pass. Both are per-call overrides; neither touches
// a.mode, so the UI's mode selector and any hand-typed messages are
// unaffected. The override (and queuedByTool itself) is persisted on the row
// so a reloaded session can still tell a queued step apart from one the user
// actually typed.
//
// A queued step's user/assistant rows are also persisted unpinned (see
// `pinned` below), unlike a hand-typed turn's: the frontend re-injects the
// live plan state into every subsequent step's prompt anyway, so carrying
// last step's version of it forward in pinned history would just repeat
// near-duplicate content on every future turn — bloating context and giving
// the model a wall of repetitive "here's the plan again" turns to
// pattern-match against instead of acting on the current one.
//
// displayMsg, when non-empty, is what's persisted/shown as this turn's user
// message in the chat log instead of userMsg — used for a plan-driven turn,
// where userMsg is the full prompt (current plan state folded in) the model
// needs to actually act on, but showing that whole thing in the chat log
// would just repeat the Plan tab's own checklist back at the user. Empty
// for a hand-typed turn, where there's nothing to shorten.
//
// For a custom (file-based) cot mode, this runs two model calls: a hidden evaluation
// pass using the cot prompt as a system message, then a final answer pass that folds
// the evaluation back in as a hidden note. The cot note and any tool calls are
// persisted as their own unpinned message rows (visible collapsed in the UI, but
// excluded from context on future turns unless the user pins them).
func (a *App) SendChat(userMsg string, history []ChatMessage, queuedByTool string, displayMsg string) (ChatTurnResult, error) {
	if a.cli == nil {
		return ChatTurnResult{}, fmt.Errorf("Ollama client not initialized")
	}

	sessionID := a.sess.CurrentSession()

	// Checked before this turn's messages are saved below, and independent of the
	// frontend-supplied history (which already includes the current user message).
	hadMessages, err := a.sess.HasMessages(sessionID)
	if err != nil {
		slog.Warn("failed to check session message count", "session", sessionID, "error", err)
	}
	isFirstMessage := err == nil && !hadMessages

	// mode is the effective cot mode for this turn — normally the UI's
	// current selection, but forced to "none" for a queued task regardless
	// of what's selected (see queuedByTool doc above).
	mode := a.mode
	if queuedByTool != "" {
		mode = CotModeNone
	}

	// A plan-driven turn's user prompt (the injected current-step text — see
	// planPromptFor in app.js) and its reply are only relevant for that one
	// step: the frontend rebuilds the equivalent prompt from the live plan
	// state on every subsequent step anyway (see manage_plan/advancePlan), so
	// pinning them would just carry forward repeated near-duplicate content
	// on every future turn's context — for a long-running plan this bloats
	// context fast and, worse, gives the model a wall of near-identical
	// "here's the plan again" turns to pattern-match against instead of
	// actually acting, which is what let it drift into replying with prose
	// and no tool call at all. Both rows stay visible in the chat log
	// (dimmed, like any unpinned message) and can be pinned back manually.
	pinned := queuedByTool == ""

	displayText := userMsg
	if displayMsg != "" {
		displayText = displayMsg
	}

	baseMsgs := make([]api.Message, 0, len(history)+1)
	for _, h := range history {
		baseMsgs = append(baseMsgs, api.Message{Role: h.Role, Content: h.Content})
	}
	baseMsgs = append(baseMsgs, api.Message{Role: "user", Content: userMsg})

	systemPrompt, err := loadSystemPrompt()
	if err != nil {
		slog.Warn("failed to load system prompt", "error", err)
	}

	// systemParts collects every piece of system-level context for this turn
	// (the configurable conf/SYSTEM.md prompt, then the cot hidden-evaluation
	// note if applicable) into a single system message — most chat templates
	// expect exactly one system message before history/user, not several, and
	// a trailing system message after the final user turn confuses the model
	// into truncating its reply.
	var systemParts []string
	if systemPrompt != "" {
		systemParts = append(systemParts, systemPrompt)
	}

	// turnMsgs accumulates every message row this turn persists, in
	// conversation order. Each message is saved to the store as soon as it's
	// produced (not batched until the turn succeeds) so that a turn cut short
	// by an error (e.g. the underlying chat request failing) still leaves
	// everything that happened so far in the session.
	turnMsgs := []ChatTurnMessage{a.persist(sessionID, store.NewMessage{Role: "user", Content: displayText, Model: a.model, Mode: mode, Pinned: pinned, ToolName: queuedByTool})}

	// Title the session from the user's opening message as soon as it's in,
	// not after the model replies — a long tool-calling/plan-driven chain, or
	// one that errors out (e.g. the underlying chat request timing out),
	// would otherwise leave the session titled "New Chat" indefinitely.
	if isFirstMessage {
		title := titleFromMessage(displayText, 40)
		if err := a.sess.RenameSession(sessionID, title); err != nil {
			slog.Warn("failed to auto-title session", "session", sessionID, "error", err)
		} else {
			wailsruntime.EventsEmit(a.ctx, "session:renamed", map[string]any{"sessionId": sessionID, "title": title})
		}
	}

	msgs := baseMsgs
	if mode != CotModeNone && mode != CotModeBuiltIn && mode != "" {
		if cot, err := loadCotConfig(mode); err != nil {
			slog.Warn("failed to load cot prompt", "mode", mode, "error", err)
		} else {
			evalParts := append(append([]string{}, systemParts...), fmt.Sprintf(cotEvalWrapper, cot.Prompt))
			evalMsgs := append([]api.Message{{Role: "system", Content: strings.Join(evalParts, "\n\n")}}, baseMsgs...)

			wailsruntime.EventsEmit(a.ctx, "chat:status", map[string]any{"state": "thinking"})
			evaluation, err := a.chatOnce(evalMsgs, api.ThinkValue{Value: false}, map[string]any{"num_predict": cot.MaxTokens})
			if err != nil {
				slog.Warn("cot evaluation failed", "mode", mode, "error", err)
			} else {
				turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{Role: "cot", Content: evaluation, Model: a.model, Mode: mode, Pinned: false}))

				// Fold the evaluation into the final user turn itself (not the
				// system message, and not the persisted/history copy of the
				// turn — baseMsgs and the stored user row above keep the plain
				// prompt so future turns don't replay the CoT note back at the
				// model as if the user had written it).
				augmented := fmt.Sprintf(cotAnswerWrapper, userMsg, evaluation)
				msgs = append(append([]api.Message{}, baseMsgs[:len(baseMsgs)-1]...), api.Message{Role: "user", Content: augmented})
			}
		}
	}

	if len(systemParts) > 0 {
		msgs = append([]api.Message{{Role: "system", Content: strings.Join(systemParts, "\n\n")}}, msgs...)
	}

	think := api.ThinkValue{Value: mode == CotModeBuiltIn}
	fullReply, toolTurnMsgs, err := a.chatWithTools(sessionID, msgs, think, mode)
	turnMsgs = append(turnMsgs, toolTurnMsgs...)
	if err != nil {
		// Everything up to the failure (user message, cot note, dispatched tool
		// calls) has already been persisted via a.persist above/inside
		// chatWithTools, so the caller still gets a full record of the turn.
		return ChatTurnResult{Messages: turnMsgs}, fmt.Errorf("chat request failed: %w", err)
	}
	turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{Role: "assistant", Content: fullReply, Model: a.model, Mode: mode, Pinned: pinned}))

	a.enqueueTurnMemory(sessionID)

	return ChatTurnResult{Messages: turnMsgs}, nil
}

// enqueueTurnMemory queues this session's turns for ingestion into memory.
//
// Deliberately the last thing SendChat does, and deliberately non-blocking: the
// queue's single worker does the chunking and embedding, so the only cost on the
// chat turn is one mutex-guarded append. A failure here must never surface as a
// failed chat turn — the reply is already persisted and returned — so it is logged
// and dropped.
//
// Placed after the assistant row is persisted rather than before, because the
// ingestion unit is the *pair*: a job that ran a moment earlier would find a user
// message with no reply and skip it.
func (a *App) enqueueTurnMemory(sessionID string) {
	if a.mem == nil || sessionID == "" {
		return
	}
	title := ""
	if sessions, err := a.sess.GetSessions(); err == nil {
		for _, s := range sessions {
			if s.ID == sessionID {
				title = s.Title
				break
			}
		}
	}
	// The messages are loaded inside the job, on the worker goroutine — reading them
	// here would put a query on the chat turn's path for no reason, and the job may
	// run after further turns have landed anyway.
	if _, err := a.mem.EnqueueTurnIngest(sessionID, title, func() ([]store.StoredMessage, error) {
		return a.sess.GetMessages(sessionID)
	}); err != nil {
		slog.Warn("could not queue conversation for memory", "session", sessionID, "error", err)
	}
}

// titleFromMessage derives a short session title by collapsing a message to a single
// line and truncating it to maxLen characters on a word boundary.
func titleFromMessage(msg string, maxLen int) string {
	title := strings.Join(strings.Fields(msg), " ")
	if len(title) <= maxLen {
		return title
	}
	cut := title[:maxLen]
	if idx := strings.LastIndex(cut, " "); idx > 0 {
		cut = cut[:idx]
	}
	return strings.TrimSpace(cut) + "…"
}

// chatOnce issues a single non-streaming chat completion request (no tools
// attached) and returns the full reply text. Used for the cot hidden
// evaluation pass, which is a throwaway internal note that shouldn't be able
// to trigger skill/artifact tool calls.
func (a *App) chatOnce(msgs []api.Message, think api.ThinkValue, options map[string]any) (string, error) {
	resp, err := a.chatRequestOnce(msgs, think, nil, options)
	return resp.Content, err
}

// chatRequestOnce issues a single non-streaming chat completion request,
// optionally with a tool registry attached, and returns the full response
// message (content plus any requested tool calls).
func (a *App) chatRequestOnce(msgs []api.Message, think api.ThinkValue, tools api.Tools, options map[string]any) (api.Message, error) {
	stream := false
	req := &api.ChatRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   &stream,
		Think:    &think,
		Tools:    tools,
		Options:  options,
	}

	var full api.Message
	var mu sync.Mutex
	err := a.cli.Chat(a.ctx, req, func(resp api.ChatResponse) error {
		mu.Lock()
		defer mu.Unlock()
		full.Role = resp.Message.Role
		full.Content += resp.Message.Content
		if len(resp.Message.ToolCalls) > 0 {
			full.ToolCalls = append(full.ToolCalls, resp.Message.ToolCalls...)
		}
		return nil
	})
	return full, err
}

// chatWithTools runs the tool-calling loop for a chat turn: send the request
// with the tool registry attached, execute any requested tool calls, feed
// their results back as "tool" messages, and repeat until the model responds
// with no more tool calls. There is deliberately no cap on the number of
// rounds — a plan (see manage_plan) can legitimately require many tool calls
// to work through, and an artificial cap just aborts a turn partway through
// real progress. Each dispatched tool call is persisted immediately (role
// "tool", unpinned) and also returned as a ChatTurnMessage so the caller can
// report it to the frontend — even if the loop later errors out (e.g. the
// underlying chat request fails), everything dispatched so far is already in
// the session.
//
// manage_plan is special-cased to end the turn immediately once dispatched,
// instead of looping back for another model round: its whole point is to
// hand off the current step to the frontend's plan-driven continuation, and
// giving the model another generation afterward just invites it to go ahead
// and attempt later steps itself before the turn ends. Ending the turn right
// there removes that possibility structurally rather than relying on the
// model to respect the tool description's wording.
func (a *App) chatWithTools(sessionID string, msgs []api.Message, think api.ThinkValue, mode string) (string, []ChatTurnMessage, error) {
	tools := toAPITools(a.toolRegistry())
	var turnMsgs []ChatTurnMessage

	// lastNarration is the most recent non-empty resp.Content seen across any
	// round so far. Some models front-load their explanation into an earlier
	// round (e.g. "I'll break this into 3 steps..." alongside an unrelated
	// exploratory tool call) and then call manage_plan itself with no
	// accompanying text in a later round — without this, that explanation
	// would be silently lost (it's only ever appended to msgs for the
	// model's own context, never surfaced) and the turn would fall through
	// to the tool's mechanical echo instead, which is the "regurgitation"
	// behavior this is meant to avoid.
	lastNarration := ""

	for {
		resp, err := a.chatRequestOnce(msgs, think, tools, nil)
		if err != nil {
			return "", turnMsgs, err
		}
		if resp.Content != "" {
			lastNarration = resp.Content
		}
		if len(resp.ToolCalls) == 0 {
			if resp.Content == "" {
				return lastNarration, turnMsgs, nil
			}
			return resp.Content, turnMsgs, nil
		}

		msgs = append(msgs, resp)
		planCalled := false
		planEcho := ""
		for _, tc := range resp.ToolCalls {
			wailsruntime.EventsEmit(a.ctx, "chat:status", map[string]any{"state": "tool", "tool": tc.Function.Name})

			result, herr := a.dispatchTool(tc)
			if herr != nil {
				slog.Warn("tool call failed", "tool", tc.Function.Name, "error", herr)
				result = fmt.Sprintf("error: %v", herr)
			} else {
				slog.Info("tool call", "tool", tc.Function.Name)
				if tc.Function.Name == "manage_plan" {
					planCalled = true
					planEcho = result
				}
			}
			msgs = append(msgs, api.Message{Role: "tool", Content: result})

			argsJSON, jerr := json.Marshal(tc.Function.Arguments)
			if jerr != nil {
				slog.Warn("failed to marshal tool arguments for persistence", "tool", tc.Function.Name, "error", jerr)
			}
			turnMsgs = append(turnMsgs, a.persist(sessionID, store.NewMessage{
				Role: "tool", Content: tc.Function.Name, Model: a.model, Mode: mode, Pinned: false,
				ToolName: tc.Function.Name, ToolArgs: string(argsJSON), ToolResult: result,
			}))
		}

		if planCalled {
			// Prefer the model's own accompanying text (its actual explanation
			// of what it's doing/why) over the tool's mechanical echo of the
			// plan — the echo is already visible on the persisted "tool" row
			// above (and in the frontend's tool-call lightbox), so surfacing
			// it again here as the "assistant" reply just repeats it back
			// with no added information, and previously clobbered whatever
			// the model actually said. Fall back to the last non-empty
			// narration from an earlier round (see lastNarration above)
			// before finally falling back to the echo, so a real explanation
			// is never discarded just because the specific round that
			// dispatched manage_plan happened to carry no text of its own.
			reply := resp.Content
			if reply == "" {
				reply = lastNarration
			}
			if reply == "" {
				reply = planEcho
			}
			return reply, turnMsgs, nil
		}
	}
}

// persist saves a single message row immediately and returns it as a
// frontend-facing ChatTurnMessage. Called as each message is produced during
// a turn (user, cot note, tool calls, assistant) rather than batching
// everything until the turn completes, so a turn that errors out partway
// through still leaves what happened so far in the session. It also emits a
// "chat:message" event immediately, so the frontend can render tool calls and
// intermediate notes live as the tool-calling loop runs, instead of waiting
// for the whole (possibly multi-iteration) turn to finish.
func (a *App) persist(sessionID string, msg store.NewMessage) ChatTurnMessage {
	seq, err := a.sess.SaveMessage(sessionID, msg)
	if err != nil {
		slog.Warn("failed to persist message", "role", msg.Role, "session", sessionID, "error", err)
	}
	turnMsg := ChatTurnMessage{
		Seq: seq, Role: msg.Role, Content: msg.Content, Model: msg.Model, Mode: msg.Mode, Pinned: msg.Pinned,
		ToolName: msg.ToolName, ToolArgs: msg.ToolArgs, ToolResult: msg.ToolResult,
	}
	wailsruntime.EventsEmit(a.ctx, "chat:message", map[string]any{"sessionId": sessionID, "message": turnMsg})
	return turnMsg
}

// --- Session management (delegated to store.Store) ---

// CreateSession starts a new chat session and returns its ID.
func (a *App) CreateSession() string {
	id, _ := a.sess.CreateSession("New Chat")
	slog.Info("created session", "id", id)
	return id
}

// GetCurrentSession returns the ID of the active session.
func (a *App) GetCurrentSession() string {
	return a.sess.CurrentSession()
}

// GetSessions returns all sessions ordered newest first.
func (a *App) GetSessions() ([]store.Session, error) {
	return a.sess.GetSessions()
}

// GetMessages returns all messages for a given session.
func (a *App) GetMessages(sessionID string) ([]store.StoredMessage, error) {
	return a.sess.GetMessages(sessionID)
}

// GetPlan returns the current plan (see manage_plan) for a given session,
// empty if none has been created yet.
func (a *App) GetPlan(sessionID string) ([]store.PlanStep, error) {
	return a.sess.GetPlan(sessionID)
}

// SwitchSession changes the active session to an existing one.
func (a *App) SwitchSession(id string) error {
	err := a.sess.SwitchSession(id)
	if err == nil {
		slog.Info("switched session", "id", id)
	}
	return err
}

// DeleteSession removes a session and its messages from the database. If it was
// the current session, a fresh one is created to replace it.
func (a *App) DeleteSession(id string) error {
	err := a.sess.DeleteSession(id)
	if err == nil {
		slog.Info("deleted session", "id", id)
	}
	return err
}

// RenameSession updates the title of an existing session.
func (a *App) RenameSession(id string, title string) error {
	return a.sess.RenameSession(id, title)
}

// SaveMessage persists a single message for a given session (frontend-facing;
// not currently called by the frontend, kept as a manual affordance like
// CreateArtifactManual). Uses the currently active model/mode and pins it by
// default.
func (a *App) SaveMessage(sessionID, role, content string) error {
	_, err := a.sess.SaveMessage(sessionID, store.NewMessage{Role: role, Content: content, Model: a.model, Mode: a.mode, Pinned: true})
	return err
}

// SetMessagePinned toggles whether a message is included in the context sent
// to the model on future turns. The message stays visible and in the
// database either way — only its inclusion in context changes.
func (a *App) SetMessagePinned(sessionID string, seq int, pinned bool) error {
	return a.sess.SetMessagePinned(sessionID, seq, pinned)
}

// ToolState reports one tool's name and enabled/disabled state for a session.
type ToolState struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// GetSessionToolStates returns the enabled/disabled state of every currently
// available tool for the given session. The set reflects runtime availability
// (memory corpus populated, directory selected) as well as per-session
// preferences.
func (a *App) GetSessionToolStates(sessionID string) ([]ToolState, error) {
	all := a.availableTools()
	disabled, err := a.sess.GetDisabledTools(sessionID)
	if err != nil {
		return nil, fmt.Errorf("get disabled tools: %w", err)
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, n := range disabled {
		disabledSet[n] = true
	}
	states := make([]ToolState, len(all))
	for i, t := range all {
		states[i] = ToolState{Name: t.Name, Enabled: !disabledSet[t.Name]}
	}
	return states, nil
}

// SetSessionToolEnabled enables or disables a specific tool for a session.
func (a *App) SetSessionToolEnabled(sessionID, toolName string, enabled bool) error {
	return a.sess.SetToolEnabled(sessionID, toolName, enabled)
}
