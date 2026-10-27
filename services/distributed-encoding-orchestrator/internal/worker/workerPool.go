// This file implements WorkerPool, the concurrent execution engine that
// drains a domain.JobQueue and dispatches each domain.ChunkEncodingTask
// to a ChunkEncodingTaskProcessor with bounded concurrency, retry with
// exponential backoff, and per-slot circuit breaking.
//
// # Concurrency design
//
// WorkerPool.RunUntilQueueDrainedOrContextCancelled runs one dispatcher
// goroutine that ranges over the queue's task channel. For every task it
// receives, it first ACQUIRES A PERMIT from a buffered channel of size
// maxConcurrentTasks (the semaphore pattern: a channel used purely for
// its capacity-limited send/receive blocking behavior, not to carry
// data), then spawns a new goroutine to execute that one task and
// release the permit when done. This means the number of
// simultaneously-executing task goroutines is bounded above by
// maxConcurrentTasks at all times, regardless of how many tasks are
// sitting in the JobQueue, which is exactly the "thousands of chunk jobs,
// bounded worker concurrency" requirement.
//
// A sync.WaitGroup tracks all spawned task goroutines so that
// RunUntilQueueDrainedOrContextCancelled can safely close its output
// TaskOutcome channel only after every in-flight goroutine has finished
// and reported its outcome — this is what prevents goroutine leaks: every
// spawned goroutine unconditionally sends exactly one TaskOutcome (or
// observes ctx cancellation and still returns promptly) and then calls
// wg.Done(), and the dispatcher loop itself exits promptly on ctx
// cancellation or channel closure.
package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
	"distributed-encoding-orchestrator/internal/logging"
	"distributed-encoding-orchestrator/internal/metrics"
)

// TaskOutcome reports the final result of dispatching one
// domain.ChunkEncodingTask through the WorkerPool, after exhausting
// retries if needed.
type TaskOutcome struct {
	// Task is the task this outcome describes.
	Task domain.ChunkEncodingTask
	// Succeeded is true if any attempt (initial or retry) succeeded.
	Succeeded bool
	// AttemptsMade is how many ProcessChunkEncodingTask calls were made
	// (1 if the first attempt succeeded, up to the policy's
	// EffectiveMaxAttempts()).
	AttemptsMade int
	// FinalError holds the error from the last failed attempt if
	// Succeeded is false; nil if Succeeded is true.
	FinalError error
	// TotalWallClockDuration is how long dispatch of this task took
	// end-to-end, including all retry backoff waits.
	TotalWallClockDuration time.Duration
}

// WorkerPoolConfiguration bundles the tunable parameters of a WorkerPool.
type WorkerPoolConfiguration struct {
	// MaxConcurrentTasks bounds how many ChunkEncodingTask executions may
	// be in flight at once across the whole pool — the semaphore
	// capacity. Must be >= 1; values <= 0 are clamped to 1.
	MaxConcurrentTasks int
	// RetryPolicy governs retry attempt count and backoff timing for
	// failed tasks.
	RetryPolicy ExponentialBackoffRetryPolicy
	// CircuitBreakerSlotCount is how many independent
	// ffmpeg.FfmpegFailureCircuitBreaker instances the pool maintains;
	// each in-flight task is assigned to one slot (round-robin over a
	// monotonically increasing counter), approximating "one breaker per
	// concurrent worker" even though this pool spawns a fresh goroutine
	// per task rather than reusing long-lived worker goroutines. A
	// sensible default is to set this equal to MaxConcurrentTasks.
	CircuitBreakerSlotCount int
	// CircuitBreakerFailureThreshold and CircuitBreakerCooldown
	// configure every slot's breaker; see
	// ffmpeg.NewFfmpegFailureCircuitBreaker.
	CircuitBreakerFailureThreshold int
	CircuitBreakerCooldown         time.Duration
	// Logger receives structured diagnostic events. Defaults to a
	// discarding logger if nil.
	Logger logging.StructuredEventLogger
}

// withDefaults returns a copy of config with zero-value fields replaced
// by sensible defaults.
func (config WorkerPoolConfiguration) withDefaults() WorkerPoolConfiguration {
	if config.MaxConcurrentTasks < 1 {
		config.MaxConcurrentTasks = 1
	}
	if config.CircuitBreakerSlotCount < 1 {
		config.CircuitBreakerSlotCount = config.MaxConcurrentTasks
	}
	if config.CircuitBreakerFailureThreshold < 1 {
		config.CircuitBreakerFailureThreshold = 5
	}
	if config.CircuitBreakerCooldown <= 0 {
		config.CircuitBreakerCooldown = 10 * time.Second
	}
	if config.RetryPolicy.MaxAttempts < 1 {
		config.RetryPolicy = DefaultExponentialBackoffRetryPolicy()
	}
	if config.Logger == nil {
		config.Logger = logging.NewDiscardingStructuredEventLogger()
	}
	return config
}

// WorkerPool is the bounded-concurrency execution engine described in
// this file's package-level doc comment. Construct with NewWorkerPool
// and run with RunUntilQueueDrainedOrContextCancelled.
//
// Concurrency/safety: a WorkerPool instance's exported methods are safe
// to call from any goroutine, but RunUntilQueueDrainedOrContextCancelled
// is intended to be called once per WorkerPool instance (running it
// twice concurrently on the same instance would share circuit breaker
// slot state across the two runs, which is harmless but not a documented
// or intended use case).
type WorkerPool struct {
	processor       ChunkEncodingTaskProcessor
	config          WorkerPoolConfiguration
	circuitBreakers []*ffmpeg.FfmpegFailureCircuitBreaker
	nextSlotCounter uint64
	slotCounterLock sync.Mutex

	// activeTaskCount tracks how many ChunkEncodingTask executions are
	// currently in flight, updated via atomic ops around each task's
	// semaphore acquire/release in RunUntilQueueDrainedOrContextCancelled,
	// and mirrored into metricsSink (if set) via
	// metrics.OrchestratorMetrics.SetWorkerPoolUtilization for the
	// worker-pool utilization gauges exposed on the /metrics endpoint.
	activeTaskCount int64
	// metricsSink is an optional Prometheus metrics collaborator; nil
	// (the default) means metrics are disabled and every call site that
	// touches it below is guarded to be a safe no-op, matching
	// OrchestratorMetrics' own nil-receiver-safe method contract. Set via
	// SetMetricsSink.
	metricsSink *metrics.OrchestratorMetrics
}

// SetMetricsSink attaches an optional metrics.OrchestratorMetrics
// collaborator that this WorkerPool will report worker-pool utilization
// gauges to as tasks start and finish. Passing nil disables metrics
// reporting (the default state of a freshly-constructed WorkerPool).
// Intended to be called once, before RunUntilQueueDrainedOrContextCancelled
// starts dispatching tasks.
func (pool *WorkerPool) SetMetricsSink(sink *metrics.OrchestratorMetrics) {
	pool.metricsSink = sink
}

// NewWorkerPool constructs a WorkerPool that dispatches tasks to
// processor according to config.
func NewWorkerPool(processor ChunkEncodingTaskProcessor, config WorkerPoolConfiguration) *WorkerPool {
	config = config.withDefaults()

	breakers := make([]*ffmpeg.FfmpegFailureCircuitBreaker, config.CircuitBreakerSlotCount)
	for i := range breakers {
		breakers[i] = ffmpeg.NewFfmpegFailureCircuitBreaker(config.CircuitBreakerFailureThreshold, config.CircuitBreakerCooldown)
	}

	return &WorkerPool{
		processor:       processor,
		config:          config,
		circuitBreakers: breakers,
	}
}

// acquireNextCircuitBreakerSlot returns the next circuit breaker in
// round-robin order, guarded by a mutex since many task goroutines call
// this concurrently.
func (pool *WorkerPool) acquireNextCircuitBreakerSlot() *ffmpeg.FfmpegFailureCircuitBreaker {
	pool.slotCounterLock.Lock()
	index := pool.nextSlotCounter % uint64(len(pool.circuitBreakers))
	pool.nextSlotCounter++
	pool.slotCounterLock.Unlock()
	return pool.circuitBreakers[index]
}

// RunUntilQueueDrainedOrContextCancelled drains queue, dispatching each
// task with bounded concurrency (see package doc for the semaphore
// mechanism), retrying failed attempts per pool.config.RetryPolicy, and
// reporting one TaskOutcome per task on the returned channel.
//
// The returned channel is closed once EITHER the queue has been fully
// drained and every dispatched task (including retries) has completed,
// OR ctx is cancelled and all in-flight attempts have unwound — whichever
// happens first. Callers MUST continue reading from the returned channel
// until it closes (or abandon the WorkerPool entirely) to avoid leaking
// the goroutines that are blocked trying to send their outcome; a
// buffered-enough channel sized to the expected task count (which this
// function itself chooses generously) makes this a non-issue in
// practice for typical usage patterns that just collect all outcomes.
//
// This method itself returns immediately; the returned channel is fed by
// background goroutines whose lifetime is exactly bounded by the queue
// draining/context cancellation described above — no goroutine outlives
// the channel closing.
func (pool *WorkerPool) RunUntilQueueDrainedOrContextCancelled(ctx context.Context, queue *domain.JobQueue) <-chan TaskOutcome {
	outcomes := make(chan TaskOutcome, pool.config.MaxConcurrentTasks*2+16)
	semaphore := make(chan struct{}, pool.config.MaxConcurrentTasks)

	go func() {
		var inFlightWaitGroup sync.WaitGroup

	dispatchLoop:
		for {
			select {
			case task, ok := <-queue.Tasks():
				if !ok {
					break dispatchLoop
				}

				select {
				case semaphore <- struct{}{}:
				case <-ctx.Done():
					break dispatchLoop
				}

				inFlightWaitGroup.Add(1)
				active := atomic.AddInt64(&pool.activeTaskCount, 1)
				pool.metricsSink.SetWorkerPoolUtilization(int(active), pool.config.MaxConcurrentTasks)
				go func(t domain.ChunkEncodingTask) {
					defer inFlightWaitGroup.Done()
					defer func() { <-semaphore }()
					defer func() {
						remaining := atomic.AddInt64(&pool.activeTaskCount, -1)
						pool.metricsSink.SetWorkerPoolUtilization(int(remaining), pool.config.MaxConcurrentTasks)
					}()

					outcome := pool.dispatchSingleTaskWithRetry(ctx, t)
					pool.metricsSink.RecordChunkOutcome(outcome.Succeeded)

					select {
					case outcomes <- outcome:
					case <-ctx.Done():
						// Best-effort: still try a non-blocking send so
						// callers draining promptly still see the
						// outcome, but never block pool shutdown on a
						// slow/absent consumer once cancelled.
						select {
						case outcomes <- outcome:
						default:
						}
					}
				}(task)

			case <-ctx.Done():
				break dispatchLoop
			}
		}

		inFlightWaitGroup.Wait()
		close(outcomes)
	}()

	return outcomes
}

// dispatchSingleTaskWithRetry executes task via pool.processor, retrying
// on failure per pool.config.RetryPolicy with exponential backoff, and
// consulting/updating a round-robin-assigned circuit breaker slot. If
// the assigned breaker is open, the task fails fast without ever calling
// the processor, and is still subject to the normal retry loop/backoff
// (so a task can "wait out" a breaker's cooldown across its own retry
// attempts).
func (pool *WorkerPool) dispatchSingleTaskWithRetry(ctx context.Context, task domain.ChunkEncodingTask) TaskOutcome {
	startTime := time.Now()
	breaker := pool.acquireNextCircuitBreakerSlot()
	maxAttempts := pool.config.RetryPolicy.EffectiveMaxAttempts()

	var lastErr error
	attemptsCompleted := 0

attemptLoop:
	for attemptNumber := 1; attemptNumber <= maxAttempts; attemptNumber++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break attemptLoop
		}

		attemptsCompleted = attemptNumber

		if !breaker.AllowCall() {
			lastErr = ffmpeg.ErrCircuitBreakerOpen
			pool.config.Logger.Warn("circuit breaker open; skipping attempt",
				"taskIdentifier", task.TaskIdentifier, "attempt", attemptNumber)
		} else {
			attemptErr := pool.processor.ProcessChunkEncodingTask(ctx, task)
			if attemptErr == nil {
				breaker.RecordSuccess()
				return TaskOutcome{
					Task:                   task,
					Succeeded:              true,
					AttemptsMade:           attemptNumber,
					TotalWallClockDuration: time.Since(startTime),
				}
			}
			breaker.RecordFailure()
			lastErr = attemptErr
			pool.config.Logger.Warn("chunk encoding task attempt failed",
				"taskIdentifier", task.TaskIdentifier, "attempt", attemptNumber, "error", attemptErr.Error())
		}

		if attemptNumber < maxAttempts {
			backoffDelay := pool.config.RetryPolicy.ComputeBackoffDelayForRetryAttempt(attemptNumber)
			select {
			case <-time.After(backoffDelay):
			case <-ctx.Done():
				lastErr = ctx.Err()
				break attemptLoop
			}
		}
	}

	return TaskOutcome{
		Task:                   task,
		Succeeded:              false,
		AttemptsMade:           attemptsCompleted,
		FinalError:             lastErr,
		TotalWallClockDuration: time.Since(startTime),
	}
}
