package worker

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/domain"
)

// buildFixtureTask constructs a minimal ChunkEncodingTask for pool tests
// that don't care about ffmpeg specifics.
func buildFixtureTask(id string) domain.ChunkEncodingTask {
	return domain.ChunkEncodingTask{TaskIdentifier: id, MaximumAttemptCount: 1}
}

// enqueueAllAndClose is a small test helper that pushes every task into
// queue and then closes it, mirroring what Dispatcher does internally.
func enqueueAllAndClose(t *testing.T, ctx context.Context, queue *domain.JobQueue, tasks []domain.ChunkEncodingTask) {
	t.Helper()
	go func() {
		for _, task := range tasks {
			if err := queue.Enqueue(ctx, task); err != nil {
				return
			}
		}
		queue.Close()
	}()
}

// TestWorkerPool_ProcessesAllTasksExactlyOnceOnSuccess verifies the
// baseline correctness property: N enqueued tasks with an
// always-succeeding processor yields exactly N successful TaskOutcome
// values, each for a distinct task, with AttemptsMade==1.
func TestWorkerPool_ProcessesAllTasksExactlyOnceOnSuccess(t *testing.T) {
	const taskCount = 2000

	var processedCount int64
	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		atomic.AddInt64(&processedCount, 1)
		return nil
	})

	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 64})

	queue := domain.NewJobQueue(taskCount)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tasks := make([]domain.ChunkEncodingTask, taskCount)
	for i := range tasks {
		tasks[i] = buildFixtureTask(taskIDFor(i))
	}
	enqueueAllAndClose(t, ctx, queue, tasks)

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)

	seen := make(map[string]bool, taskCount)
	succeededCount := 0
	for outcome := range outcomes {
		if seen[outcome.Task.TaskIdentifier] {
			t.Fatalf("task %s reported more than once", outcome.Task.TaskIdentifier)
		}
		seen[outcome.Task.TaskIdentifier] = true
		if !outcome.Succeeded {
			t.Errorf("expected task %s to succeed, got error %v", outcome.Task.TaskIdentifier, outcome.FinalError)
		}
		if outcome.AttemptsMade != 1 {
			t.Errorf("expected AttemptsMade==1 for task %s, got %d", outcome.Task.TaskIdentifier, outcome.AttemptsMade)
		}
		succeededCount++
	}

	if succeededCount != taskCount {
		t.Fatalf("expected %d outcomes, got %d", taskCount, succeededCount)
	}
	if int(atomic.LoadInt64(&processedCount)) != taskCount {
		t.Fatalf("expected processor invoked exactly %d times, got %d", taskCount, processedCount)
	}
}

// TestWorkerPool_NeverExceedsConfiguredConcurrencyBound instruments the
// processor to track the maximum number of concurrently-executing calls
// observed and asserts it never exceeds MaxConcurrentTasks — the core
// correctness property of the semaphore-bounded-concurrency design.
func TestWorkerPool_NeverExceedsConfiguredConcurrencyBound(t *testing.T) {
	const taskCount = 300
	const maxConcurrent = 8

	var currentInFlight int64
	var observedMax int64

	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		current := atomic.AddInt64(&currentInFlight, 1)
		defer atomic.AddInt64(&currentInFlight, -1)

		for {
			observed := atomic.LoadInt64(&observedMax)
			if current <= observed || atomic.CompareAndSwapInt64(&observedMax, observed, current) {
				break
			}
		}

		time.Sleep(2 * time.Millisecond) // hold the "slot" briefly so overlap is observable
		return nil
	})

	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: maxConcurrent})
	queue := domain.NewJobQueue(taskCount)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tasks := make([]domain.ChunkEncodingTask, taskCount)
	for i := range tasks {
		tasks[i] = buildFixtureTask(taskIDFor(i))
	}
	enqueueAllAndClose(t, ctx, queue, tasks)

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)
	completed := 0
	for range outcomes {
		completed++
	}

	if completed != taskCount {
		t.Fatalf("expected %d completions, got %d", taskCount, completed)
	}
	if observedMax > maxConcurrent {
		t.Fatalf("observed max concurrency %d exceeds configured bound %d", observedMax, maxConcurrent)
	}
	if observedMax < 2 {
		t.Logf("warning: test did not observe meaningful concurrency (max=%d); still passes but is a weak signal", observedMax)
	}
}

// TestWorkerPool_RetriesFailedTasksAndEventuallySucceeds verifies the
// Dispatcher-level retry/exponential-backoff logic: a processor that
// fails a fixed number of times per task before succeeding should still
// yield a Succeeded outcome once retries are exhausted-with-success, and
// the outcome should report the correct AttemptsMade.
func TestWorkerPool_RetriesFailedTasksAndEventuallySucceeds(t *testing.T) {
	var attemptsForTask sync.Map // taskID -> *int64

	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		counterVal, _ := attemptsForTask.LoadOrStore(task.TaskIdentifier, new(int64))
		counter := counterVal.(*int64)
		n := atomic.AddInt64(counter, 1)
		if n < 3 { // fail the first 2 attempts, succeed on the 3rd
			return errors.New("simulated transient ffmpeg failure")
		}
		return nil
	})

	pool := NewWorkerPool(processor, WorkerPoolConfiguration{
		MaxConcurrentTasks: 4,
		RetryPolicy: ExponentialBackoffRetryPolicy{
			MaxAttempts:    5,
			BaseDelay:      1 * time.Millisecond,
			Multiplier:     1.5,
			MaxDelay:       20 * time.Millisecond,
			JitterFraction: 0,
		},
		CircuitBreakerFailureThreshold: 100, // effectively disabled for this test
	})

	queue := domain.NewJobQueue(10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task := buildFixtureTask("retry-task-1")
	enqueueAllAndClose(t, ctx, queue, []domain.ChunkEncodingTask{task})

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)
	var outcome TaskOutcome
	count := 0
	for o := range outcomes {
		outcome = o
		count++
	}

	if count != 1 {
		t.Fatalf("expected exactly 1 outcome, got %d", count)
	}
	if !outcome.Succeeded {
		t.Fatalf("expected eventual success, got failure: %v", outcome.FinalError)
	}
	if outcome.AttemptsMade != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", outcome.AttemptsMade)
	}
}

// TestWorkerPool_TaskThatAlwaysFailsReportsFailureAfterExhaustingRetries
// verifies the negative case: a permanently-failing task is retried up
// to MaxAttempts and then reported as failed, not silently dropped.
func TestWorkerPool_TaskThatAlwaysFailsReportsFailureAfterExhaustingRetries(t *testing.T) {
	var attemptCount int64
	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		atomic.AddInt64(&attemptCount, 1)
		return errors.New("permanent failure")
	})

	pool := NewWorkerPool(processor, WorkerPoolConfiguration{
		MaxConcurrentTasks: 2,
		RetryPolicy: ExponentialBackoffRetryPolicy{
			MaxAttempts: 3,
			BaseDelay:   1 * time.Millisecond,
			Multiplier:  1.0,
			MaxDelay:    5 * time.Millisecond,
		},
		CircuitBreakerFailureThreshold: 100,
	})

	queue := domain.NewJobQueue(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task := buildFixtureTask("always-fails")
	enqueueAllAndClose(t, ctx, queue, []domain.ChunkEncodingTask{task})

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)
	var outcome TaskOutcome
	for o := range outcomes {
		outcome = o
	}

	if outcome.Succeeded {
		t.Fatal("expected task to fail permanently")
	}
	if outcome.AttemptsMade != 3 {
		t.Fatalf("expected exactly 3 attempts (MaxAttempts), got %d", outcome.AttemptsMade)
	}
	if int(atomic.LoadInt64(&attemptCount)) != 3 {
		t.Fatalf("expected processor called exactly 3 times, got %d", attemptCount)
	}
}

// TestWorkerPool_NoGoroutineLeakAfterCompletion verifies that once
// RunUntilQueueDrainedOrContextCancelled's output channel closes, no
// pool-owned goroutines remain running: we snapshot runtime.NumGoroutine
// before dispatch, run a batch of tasks to completion, drain the
// channel fully, allow a brief settle window for the Go scheduler to
// finish tearing down completed goroutines, and assert the goroutine
// count has returned to (approximately) its baseline.
//
// This is the concurrency-safety check requested by the task
// ("no goroutine leaks... under -race"); run this test file with
// `go test -race ./internal/worker/...` as documented in the README.
func TestWorkerPool_NoGoroutineLeakAfterCompletion(t *testing.T) {
	const taskCount = 500

	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		return nil
	})
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 32})

	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()

	queue := domain.NewJobQueue(taskCount)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tasks := make([]domain.ChunkEncodingTask, taskCount)
	for i := range tasks {
		tasks[i] = buildFixtureTask(taskIDFor(i))
	}
	enqueueAllAndClose(t, ctx, queue, tasks)

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)
	for range outcomes {
		// drain fully; loop exits once the channel is closed, which
		// RunUntilQueueDrainedOrContextCancelled only does after every
		// spawned goroutine has called wg.Done().
	}

	// Give the runtime a brief window to finish scheduling-related
	// goroutine teardown bookkeeping that can lag slightly behind the
	// logical completion we just observed.
	var settledGoroutines int
	for attempt := 0; attempt < 20; attempt++ {
		runtime.GC()
		settledGoroutines = runtime.NumGoroutine()
		if settledGoroutines <= baselineGoroutines+2 { // small tolerance for test-runner/background goroutines
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if settledGoroutines > baselineGoroutines+2 {
		t.Fatalf("possible goroutine leak: baseline=%d, after completion+settle=%d", baselineGoroutines, settledGoroutines)
	}
}

// TestWorkerPool_ContextCancellationStopsDispatchPromptly verifies that
// cancelling ctx causes RunUntilQueueDrainedOrContextCancelled's output
// channel to close promptly even if the queue still has undispatched
// tasks buffered, rather than hanging until the (much longer) test
// timeout.
func TestWorkerPool_ContextCancellationStopsDispatchPromptly(t *testing.T) {
	blockCh := make(chan struct{})
	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		select {
		case <-blockCh:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 2})

	queue := domain.NewJobQueue(100)
	ctx, cancel := context.WithCancel(context.Background())

	tasks := make([]domain.ChunkEncodingTask, 50)
	for i := range tasks {
		tasks[i] = buildFixtureTask(taskIDFor(i))
	}
	// Enqueue without closing (simulates a producer slower than cancellation).
	go func() {
		for _, task := range tasks {
			_ = queue.Enqueue(ctx, task)
		}
	}()

	outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)

	time.Sleep(20 * time.Millisecond) // let a couple of tasks start
	cancel()
	close(blockCh)

	done := make(chan struct{})
	go func() {
		for range outcomes {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected outcomes channel to close promptly after context cancellation")
	}
}

// taskIDFor generates a stable, unique task identifier for index i,
// used across pool tests that don't care about realistic domain IDs.
func taskIDFor(i int) string {
	return "task-" + itoa(i)
}

// itoa avoids importing strconv solely for this tiny test helper file's
// simple non-negative integer formatting needs.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := [20]byte{}
	pos := len(digits)
	for i > 0 {
		pos--
		digits[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(digits[pos:])
}
