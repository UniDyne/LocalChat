// Package memory implements the memory subsystem: ingestion, chunking,
// embedding, and retrieval over the DuckDB-backed store.
//
// This file holds the ingestion queue. Ingestion must never run on a chat
// turn's goroutine: DuckDB is single-writer and store.Store serializes through
// one mutex, so a bulk ingest sharing that goroutine would stall SaveMessage
// mid-turn. Everything therefore goes through this queue, drained by exactly one
// worker.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Job is one unit of ingestion work. Kind is informational, used for progress
// reporting and for coalescing duplicate work.
type Job struct {
	// Kind is one of "directory", "conversation", "artifact", "backfill",
	// "entities". Used for logging and dedup, not dispatch.
	Kind string
	// Key uniquely identifies the target. Enqueuing a Job whose Key is already
	// queued is a no-op, which keeps a rapid series of file-change or turn
	// events from piling up redundant work.
	Key string
	// Run performs the work. It receives a context cancelled on Queue.Stop.
	Run func(ctx context.Context) error
}

// Progress describes queue state for the UI.
type Progress struct {
	Pending   int    `json:"pending"`
	Running   string `json:"running"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
}

// Queue is a single-worker job queue. Zero value is not usable; call NewQueue.
type Queue struct {
	mu      sync.Mutex
	pending []Job
	queued  map[string]bool // Key -> queued, for dedup
	running string
	wake    chan struct{}

	completed atomic.Uint64
	failed    atomic.Uint64

	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	started bool

	// onProgress, if set, is called whenever queue state changes. It runs on the
	// worker goroutine, so it must not block or call back into the Queue.
	onProgress func(Progress)
}

// ErrStopped is returned by Enqueue after the queue has been stopped.
var ErrStopped = errors.New("memory: ingestion queue stopped")

// NewQueue creates a stopped queue. Call Start to begin draining.
func NewQueue(onProgress func(Progress)) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	return &Queue{
		queued:     make(map[string]bool),
		wake:       make(chan struct{}, 1),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		onProgress: onProgress,
	}
}

// Start launches the single worker goroutine. Calling it twice is a no-op.
func (q *Queue) Start() {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()
	go q.loop()
}

// Stop cancels in-flight work and waits for the worker to exit. Safe to call on
// a queue that was never started.
func (q *Queue) Stop() {
	q.cancel()
	q.mu.Lock()
	started := q.started
	q.mu.Unlock()
	if !started {
		return
	}
	q.signal()
	<-q.done
}

// Enqueue adds a job. A job whose Key is already pending is dropped, and the
// returned bool says whether it was actually accepted.
func (q *Queue) Enqueue(j Job) (bool, error) {
	if j.Run == nil {
		return false, fmt.Errorf("memory: job %q has no Run function", j.Kind)
	}
	if err := q.ctx.Err(); err != nil {
		return false, ErrStopped
	}

	q.mu.Lock()
	if j.Key != "" && q.queued[j.Key] {
		q.mu.Unlock()
		return false, nil
	}
	if j.Key != "" {
		q.queued[j.Key] = true
	}
	q.pending = append(q.pending, j)
	q.mu.Unlock()

	q.signal()
	q.emitProgress()
	return true, nil
}

// Progress returns a snapshot of queue state.
func (q *Queue) Progress() Progress {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Progress{
		Pending:   len(q.pending),
		Running:   q.running,
		Completed: q.completed.Load(),
		Failed:    q.failed.Load(),
	}
}

// Idle reports whether nothing is queued or running.
func (q *Queue) Idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) == 0 && q.running == ""
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default: // a wake-up is already pending; one is enough
	}
}

func (q *Queue) emitProgress() {
	if q.onProgress == nil {
		return
	}
	q.onProgress(q.Progress())
}

func (q *Queue) loop() {
	defer close(q.done)
	for {
		job, ok := q.next()
		if !ok {
			select {
			case <-q.ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		q.runJob(job)
	}
}

func (q *Queue) next() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Job{}, false
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	if j.Key != "" {
		delete(q.queued, j.Key)
	}
	q.running = j.Kind
	if j.Key != "" {
		q.running = j.Kind + ":" + j.Key
	}
	return j, true
}

// runJob executes one job, converting a panic into a failure so one bad
// document cannot take down the worker and silently stop all future ingestion.
func (q *Queue) runJob(j Job) {
	defer func() {
		if r := recover(); r != nil {
			q.failed.Add(1)
			slog.Error("memory ingestion job panicked", "kind", j.Kind, "key", j.Key, "panic", r)
		}
		q.mu.Lock()
		q.running = ""
		q.mu.Unlock()
		q.emitProgress()
	}()

	q.emitProgress()
	if err := j.Run(q.ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		q.failed.Add(1)
		slog.Warn("memory ingestion job failed", "kind", j.Kind, "key", j.Key, "error", err)
		return
	}
	q.completed.Add(1)
}
