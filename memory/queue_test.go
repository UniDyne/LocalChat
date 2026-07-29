package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitFor polls until cond holds or the deadline passes, so the tests don't
// depend on sleep durations.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueRunsJobsInOrder(t *testing.T) {
	q := NewQueue(nil)
	q.Start()
	defer q.Stop()

	var mu sync.Mutex
	var order []string
	for _, name := range []string{"a", "b", "c"} {
		n := name
		if ok, err := q.Enqueue(Job{Kind: "test", Key: n, Run: func(ctx context.Context) error {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			return nil
		}}); err != nil || !ok {
			t.Fatalf("enqueue %s: ok=%v err=%v", n, ok, err)
		}
	}

	waitFor(t, "all jobs to finish", func() bool { return q.Idle() })

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("jobs ran out of order: %v", order)
	}
	if p := q.Progress(); p.Completed != 3 || p.Failed != 0 {
		t.Errorf("progress = %+v, want 3 completed", p)
	}
}

// TestQueueSerializesWork is the property that matters most: ingestion must never
// run concurrently with itself, because DuckDB is single-writer and fails the loser
// of a write-write conflict. (The store's write mutex, added in Phase 7, protects
// against writers *outside* the queue — a search recomputing BM25 statistics. It is
// not a substitute for this: two concurrent ingests would still interleave their
// transactions.)
func TestQueueSerializesWork(t *testing.T) {
	q := NewQueue(nil)
	q.Start()
	defer q.Stop()

	var mu sync.Mutex
	concurrent, maxConcurrent := 0, 0
	for i := 0; i < 20; i++ {
		if _, err := q.Enqueue(Job{Kind: "test", Run: func(ctx context.Context) error {
			mu.Lock()
			concurrent++
			if concurrent > maxConcurrent {
				maxConcurrent = concurrent
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			concurrent--
			mu.Unlock()
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "jobs to finish", func() bool { return q.Idle() })

	mu.Lock()
	defer mu.Unlock()
	if maxConcurrent != 1 {
		t.Errorf("max concurrency = %d, want 1 — the store cannot take concurrent writers", maxConcurrent)
	}
}

func TestQueueDedupsByKey(t *testing.T) {
	q := NewQueue(nil)
	// Deliberately not started, so jobs accumulate and dedup is observable.

	first, err := q.Enqueue(Job{Kind: "dir", Key: "/vault", Run: func(context.Context) error { return nil }})
	if err != nil || !first {
		t.Fatalf("first enqueue: ok=%v err=%v", first, err)
	}
	second, err := q.Enqueue(Job{Kind: "dir", Key: "/vault", Run: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("duplicate key was accepted — repeated rescans would pile up redundant work")
	}
	if p := q.Progress(); p.Pending != 1 {
		t.Errorf("pending = %d, want 1", p.Pending)
	}

	// A different key is accepted.
	if ok, err := q.Enqueue(Job{Kind: "dir", Key: "/other", Run: func(context.Context) error { return nil }}); err != nil || !ok {
		t.Fatalf("different key rejected: ok=%v err=%v", ok, err)
	}

	// Once the job has been dequeued, the same key may be enqueued again — this
	// is what lets a file change after ingestion trigger a fresh pass.
	q.Start()
	defer q.Stop()
	waitFor(t, "queue to drain", func() bool { return q.Idle() })
	if ok, err := q.Enqueue(Job{Kind: "dir", Key: "/vault", Run: func(context.Context) error { return nil }}); err != nil || !ok {
		t.Errorf("re-enqueue after completion rejected: ok=%v err=%v", ok, err)
	}
}

func TestQueueFailureDoesNotStopWorker(t *testing.T) {
	q := NewQueue(nil)
	q.Start()
	defer q.Stop()

	var ran bool
	var mu sync.Mutex
	if _, err := q.Enqueue(Job{Kind: "bad", Run: func(context.Context) error {
		return errors.New("boom")
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(Job{Kind: "good", Run: func(context.Context) error {
		mu.Lock()
		ran = true
		mu.Unlock()
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queue to drain", func() bool { return q.Idle() })

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Error("a failing job stopped subsequent work")
	}
	if p := q.Progress(); p.Failed != 1 || p.Completed != 1 {
		t.Errorf("progress = %+v, want 1 failed and 1 completed", p)
	}
}

// TestQueuePanicIsContained matters because one malformed document must not kill
// the worker and silently stop all future ingestion.
func TestQueuePanicIsContained(t *testing.T) {
	q := NewQueue(nil)
	q.Start()
	defer q.Stop()

	var ran bool
	var mu sync.Mutex
	if _, err := q.Enqueue(Job{Kind: "panic", Run: func(context.Context) error {
		panic("malformed document")
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(Job{Kind: "after", Run: func(context.Context) error {
		mu.Lock()
		ran = true
		mu.Unlock()
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queue to drain after panic", func() bool { return q.Idle() })

	mu.Lock()
	defer mu.Unlock()
	if !ran {
		t.Error("a panicking job killed the worker")
	}
	if p := q.Progress(); p.Failed != 1 {
		t.Errorf("panic not recorded as a failure: %+v", p)
	}
}

func TestQueueStopCancelsInFlight(t *testing.T) {
	q := NewQueue(nil)
	q.Start()

	started := make(chan struct{})
	var cancelled bool
	var mu sync.Mutex
	if _, err := q.Enqueue(Job{Kind: "long", Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		mu.Lock()
		cancelled = true
		mu.Unlock()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	q.Stop()

	mu.Lock()
	defer mu.Unlock()
	if !cancelled {
		t.Error("Stop did not cancel the running job's context")
	}
	// A cancelled job is not a failure — it was asked to stop.
	if p := q.Progress(); p.Failed != 0 {
		t.Errorf("cancellation counted as failure: %+v", p)
	}
}

func TestQueueEnqueueAfterStop(t *testing.T) {
	q := NewQueue(nil)
	q.Start()
	q.Stop()

	if _, err := q.Enqueue(Job{Kind: "late", Run: func(context.Context) error { return nil }}); !errors.Is(err, ErrStopped) {
		t.Errorf("expected ErrStopped, got %v", err)
	}
}

func TestQueueRejectsJobWithoutRun(t *testing.T) {
	q := NewQueue(nil)
	if _, err := q.Enqueue(Job{Kind: "empty"}); err == nil {
		t.Error("expected an error for a job with no Run function")
	}
}

func TestQueueProgressCallback(t *testing.T) {
	var mu sync.Mutex
	var sawRunning bool
	var last Progress
	q := NewQueue(func(p Progress) {
		mu.Lock()
		if p.Running != "" {
			sawRunning = true
		}
		last = p
		mu.Unlock()
	})
	q.Start()
	defer q.Stop()

	if _, err := q.Enqueue(Job{Kind: "work", Key: "k1", Run: func(context.Context) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queue to drain", func() bool { return q.Idle() })
	waitFor(t, "final progress callback", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return last.Completed == 1 && last.Running == ""
	})

	mu.Lock()
	defer mu.Unlock()
	if !sawRunning {
		t.Error("progress never reported a running job")
	}
}

// TestQueueStopWithoutStart guards a plausible shutdown path: Stop on a queue
// that was constructed but never started must not block forever.
func TestQueueStopWithoutStart(t *testing.T) {
	q := NewQueue(nil)
	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked on a queue that was never started")
	}
}
