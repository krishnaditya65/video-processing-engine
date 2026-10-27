// This file implements Dispatcher, the top-level orchestration
// component that ties together job fan-out (domain package), the
// bounded-concurrency JobQueue, and the WorkerPool: it accepts whole
// domain.EncodingJob submissions, expands each into its constituent
// domain.ChunkEncodingTask units, feeds them through a JobQueue into a
// WorkerPool, and aggregates the resulting per-task TaskOutcome stream
// back into a JobCompletionSummary and live per-job progress metrics.
package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"distributed-encoding-orchestrator/internal/checkpoint"
	"distributed-encoding-orchestrator/internal/cost"
	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/logging"
	"distributed-encoding-orchestrator/internal/metrics"
)

// JobCompletionSummary aggregates every TaskOutcome produced for a single
// domain.EncodingJob's chunk tasks once dispatch has finished (queue
// drained and all in-flight attempts settled, or ctx cancelled).
type JobCompletionSummary struct {
	// JobIdentifier identifies the summarized EncodingJob.
	JobIdentifier string
	// TotalTaskCount is how many ChunkEncodingTask units the job fanned
	// out into.
	TotalTaskCount int
	// SucceededTaskCount and FailedTaskCount partition TotalTaskCount.
	SucceededTaskCount int
	FailedTaskCount    int
	// FailedTaskIdentifiers lists the TaskIdentifier of every task that
	// never succeeded, for operator triage/re-submission.
	FailedTaskIdentifiers []string
	// WallClockDuration is how long dispatching this job's entire task
	// set took from submission to the last outcome observed.
	WallClockDuration time.Duration
}

// FinalStatus derives an domain.EncodingJobStatus from this summary:
// EncodingJobStatusCompleted if every task succeeded (and at least one
// task existed), otherwise EncodingJobStatusFailed.
func (summary JobCompletionSummary) FinalStatus() domain.EncodingJobStatus {
	if summary.TotalTaskCount > 0 && summary.FailedTaskCount == 0 {
		return domain.EncodingJobStatusCompleted
	}
	return domain.EncodingJobStatusFailed
}

// JobProgressSnapshot is a point-in-time view of an in-flight job's
// dispatch progress, safe to read concurrently while the job is still
// being processed (unlike reading fields directly off a shared
// domain.EncodingJob, which this codebase deliberately avoids mutating
// concurrently — see EncodingJob.Status's doc comment).
type JobProgressSnapshot struct {
	TotalTaskCount     int
	CompletedTaskCount int
	SucceededTaskCount int
	FailedTaskCount    int
}

// jobProgressTracker holds the live, concurrently-updated counters for
// one in-flight job's dispatch, using atomics so many outcome-processing
// goroutines can update it without a mutex.
type jobProgressTracker struct {
	totalTaskCount     int64
	completedTaskCount int64
	succeededTaskCount int64
	failedTaskCount    int64
}

func (tracker *jobProgressTracker) snapshot() JobProgressSnapshot {
	return JobProgressSnapshot{
		TotalTaskCount:     int(atomic.LoadInt64(&tracker.totalTaskCount)),
		CompletedTaskCount: int(atomic.LoadInt64(&tracker.completedTaskCount)),
		SucceededTaskCount: int(atomic.LoadInt64(&tracker.succeededTaskCount)),
		FailedTaskCount:    int(atomic.LoadInt64(&tracker.failedTaskCount)),
	}
}

// Dispatcher orchestrates end-to-end distributed encoding of
// domain.EncodingJob submissions across a shared WorkerPool.
//
// Concurrency/safety: safe for concurrent use — DispatchEncodingJob may
// be called concurrently from multiple goroutines to submit multiple
// jobs at once, each independently tracked; the underlying WorkerPool
// and its circuit breaker slots are shared across all jobs dispatched
// through the same Dispatcher instance (intentionally: a fleet-wide
// ffmpeg problem should trip breakers regardless of which job triggered
// it).
type Dispatcher struct {
	pool                   *WorkerPool
	logger                 logging.StructuredEventLogger
	queueCapacityPerJob    int
	inputOutputPathBuilder func(taskIdentifier string) (inputPath string, outputPath string)
	maxAttemptsPerTask     int

	progressTrackersMutex sync.RWMutex
	progressTrackers      map[string]*jobProgressTracker

	// checkpointStore, costAccountant, and metricsSink are optional
	// cross-cutting collaborators, each nil (disabled) by default and
	// attached post-construction via their With* setter methods below —
	// deliberately NOT threaded through NewDispatcher's parameter list, so
	// every existing call site (and every existing test) continues to
	// compile and behave identically without opting in to any of them.
	//
	// checkpointStore enables checkpoint-resume for preemptible workers
	// (see internal/checkpoint's package doc comment): when set,
	// DispatchEncodingJob skips any ChunkEncodingTask already recorded as
	// completed for this job (e.g. by a previous, preempted dispatch
	// attempt) and records newly-succeeded tasks as it goes.
	checkpointStore checkpoint.JobCheckpointStore
	// costAccountant enables per-chunk/per-job encode cost accounting
	// (see internal/cost's package doc comment): when set, every
	// successfully completed ChunkEncodingTask's wall-clock duration is
	// recorded against its parent job's running cost.TitleCostReport.
	costAccountant *cost.CostAccountant
	// metricsSink enables Prometheus metrics reporting for job-level
	// completion counters (see internal/metrics's package doc comment);
	// per-chunk/worker-pool-utilization metrics are reported directly by
	// WorkerPool instead (see WorkerPool.SetMetricsSink).
	metricsSink *metrics.OrchestratorMetrics
}

// WithJobCheckpointStore attaches a checkpoint.JobCheckpointStore this
// Dispatcher will consult (to skip already-completed chunks) and update
// (as chunks complete) on every DispatchEncodingJob call, enabling
// checkpoint-resume for preemptible workers. Returns dispatcher itself
// for fluent chaining. Passing nil disables checkpointing (the default).
func (dispatcher *Dispatcher) WithJobCheckpointStore(store checkpoint.JobCheckpointStore) *Dispatcher {
	dispatcher.checkpointStore = store
	return dispatcher
}

// WithCostAccountant attaches a cost.CostAccountant this Dispatcher will
// record every successfully completed ChunkEncodingTask's compute-time
// cost against. Returns dispatcher itself for fluent chaining. Passing
// nil disables cost accounting (the default).
func (dispatcher *Dispatcher) WithCostAccountant(accountant *cost.CostAccountant) *Dispatcher {
	dispatcher.costAccountant = accountant
	return dispatcher
}

// WithMetricsSink attaches a metrics.OrchestratorMetrics this Dispatcher
// will report job-completion counters to. Returns dispatcher itself for
// fluent chaining. Passing nil disables metrics reporting (the default).
func (dispatcher *Dispatcher) WithMetricsSink(sink *metrics.OrchestratorMetrics) *Dispatcher {
	dispatcher.metricsSink = sink
	return dispatcher
}

// NewDispatcher constructs a Dispatcher that submits work to pool.
//
// Params:
//   - pool: the shared WorkerPool tasks will be dispatched to.
//   - logger: structured logger for job-level events; a discarding
//     logger is used if nil.
//   - queueCapacityPerJob: the domain.JobQueue buffer capacity created
//     for each dispatched job; pass the expected chunk-task count (e.g.
//     ~3600*ladderRungCount for a 2-hour film) to avoid producer
//     blocking during fan-out, or a smaller value to bound memory at the
//     cost of enqueue backpressure.
//   - inputOutputPathBuilder: computes the (input, output) chunk file
//     paths for a given TaskIdentifier; see
//     domain.FanOutEncodingJobIntoChunkEncodingTasks.
func NewDispatcher(
	pool *WorkerPool,
	logger logging.StructuredEventLogger,
	queueCapacityPerJob int,
	inputOutputPathBuilder func(taskIdentifier string) (inputPath string, outputPath string),
) *Dispatcher {
	if logger == nil {
		logger = logging.NewDiscardingStructuredEventLogger()
	}
	if queueCapacityPerJob < 1 {
		queueCapacityPerJob = 1024
	}
	if inputOutputPathBuilder == nil {
		inputOutputPathBuilder = func(taskIdentifier string) (string, string) {
			return taskIdentifier + ".in", taskIdentifier + ".out"
		}
	}
	return &Dispatcher{
		pool:                   pool,
		logger:                 logger,
		queueCapacityPerJob:    queueCapacityPerJob,
		inputOutputPathBuilder: inputOutputPathBuilder,
		maxAttemptsPerTask:     pool.config.RetryPolicy.EffectiveMaxAttempts(),
		progressTrackers:       make(map[string]*jobProgressTracker),
	}
}

// DispatchEncodingJob fans job out into ChunkEncodingTask units (via
// domain.FanOutEncodingJobIntoChunkEncodingTasks), enqueues them into a
// fresh per-job domain.JobQueue, runs them through the shared WorkerPool,
// and blocks until every task has settled (succeeded or exhausted
// retries) or ctx is cancelled, returning the aggregated
// JobCompletionSummary.
//
// This call is itself blocking/synchronous from the caller's point of
// view (matching "wait for this one job to finish"), but internally
// drives fully concurrent, bounded fan-out execution via the WorkerPool;
// callers that want to dispatch many jobs concurrently should call
// DispatchEncodingJob from multiple goroutines (safe — see type doc).
//
// # Checkpoint-resume
//
// If WithJobCheckpointStore has attached a checkpoint.JobCheckpointStore,
// this call first lists every chunk already recorded as completed for
// job.JobIdentifier (e.g. by an earlier, preempted DispatchEncodingJob
// call for the same job) and excludes those tasks from the set actually
// submitted to the WorkerPool — they are counted directly into
// summary.SucceededTaskCount without being re-processed. This is what
// lets a preempted job resume from its last completed chunk rather than
// restarting from scratch: as long as job's fan-out is deterministic
// (domain.FanOutEncodingJobIntoChunkEncodingTasks always produces the
// same TaskIdentifier for the same (job, scene, chunk, rung) tuple, which
// it does — see domain.FormatChunkTaskIdentifier), the checkpoint
// store's record of "already done" TaskIdentifiers lines up exactly with
// a fresh fan-out of the same EncodingJob on a resumed dispatch attempt.
// Every task that DOES run through the WorkerPool and succeeds is
// recorded into the checkpoint store as it completes, so a second
// preemption partway through a resume attempt loses no additional
// progress either.
func (dispatcher *Dispatcher) DispatchEncodingJob(ctx context.Context, job domain.EncodingJob) JobCompletionSummary {
	startTime := time.Now()

	allTasks := domain.FanOutEncodingJobIntoChunkEncodingTasks(job, dispatcher.inputOutputPathBuilder, dispatcher.maxAttemptsPerTask)

	tasksToDispatch, alreadyCompletedCount := dispatcher.excludeAlreadyCheckpointedTasks(ctx, job.JobIdentifier, allTasks)

	tracker := &jobProgressTracker{
		totalTaskCount:     int64(len(allTasks)),
		completedTaskCount: int64(alreadyCompletedCount),
		succeededTaskCount: int64(alreadyCompletedCount),
	}
	dispatcher.progressTrackersMutex.Lock()
	dispatcher.progressTrackers[job.JobIdentifier] = tracker
	dispatcher.progressTrackersMutex.Unlock()
	defer func() {
		dispatcher.progressTrackersMutex.Lock()
		delete(dispatcher.progressTrackers, job.JobIdentifier)
		dispatcher.progressTrackersMutex.Unlock()
	}()

	dispatcher.logger.Info("dispatching encoding job",
		"jobIdentifier", job.JobIdentifier, "totalChunkTasks", len(allTasks),
		"alreadyCheckpointedTasks", alreadyCompletedCount, "codec", job.TargetCodec.String())

	queue := domain.NewJobQueue(dispatcher.queueCapacityPerJob)
	outcomes := dispatcher.pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)

	go func() {
		_ = queue.EnqueueAll(ctx, tasksToDispatch)
		queue.Close()
	}()

	summary := JobCompletionSummary{
		JobIdentifier:      job.JobIdentifier,
		TotalTaskCount:     len(allTasks),
		SucceededTaskCount: alreadyCompletedCount,
	}
	for outcome := range outcomes {
		atomic.AddInt64(&tracker.completedTaskCount, 1)
		if outcome.Succeeded {
			summary.SucceededTaskCount++
			atomic.AddInt64(&tracker.succeededTaskCount, 1)
			dispatcher.recordSuccessfulTaskCompletion(ctx, job, outcome)
		} else {
			summary.FailedTaskCount++
			summary.FailedTaskIdentifiers = append(summary.FailedTaskIdentifiers, outcome.Task.TaskIdentifier)
			atomic.AddInt64(&tracker.failedTaskCount, 1)
			dispatcher.logger.Error("chunk encoding task permanently failed",
				"taskIdentifier", outcome.Task.TaskIdentifier, "attemptsMade", outcome.AttemptsMade, "error", errString(outcome.FinalError))
		}
	}

	summary.WallClockDuration = time.Since(startTime)
	dispatcher.logger.Info("encoding job dispatch complete",
		"jobIdentifier", job.JobIdentifier,
		"succeeded", summary.SucceededTaskCount,
		"failed", summary.FailedTaskCount,
		"wallClockDuration", summary.WallClockDuration.String(),
	)
	dispatcher.metricsSink.RecordJobCompletion(summary.FinalStatus().String())
	return summary
}

// excludeAlreadyCheckpointedTasks filters allTasks down to the subset not
// already recorded as completed in dispatcher.checkpointStore for
// jobIdentifier (or returns allTasks unchanged, 0 if no checkpoint store
// is attached, or if listing fails — a checkpoint-store read error is
// treated as "assume nothing is checkpointed yet" so a resume attempt
// degrades to a full re-run rather than failing outright).
func (dispatcher *Dispatcher) excludeAlreadyCheckpointedTasks(ctx context.Context, jobIdentifier string, allTasks []domain.ChunkEncodingTask) (tasksToDispatch []domain.ChunkEncodingTask, alreadyCompletedCount int) {
	if dispatcher.checkpointStore == nil {
		return allTasks, 0
	}

	completed, err := dispatcher.checkpointStore.ListCompletedChunks(ctx, jobIdentifier)
	if err != nil || len(completed) == 0 {
		return allTasks, 0
	}

	remaining := make([]domain.ChunkEncodingTask, 0, len(allTasks))
	for _, task := range allTasks {
		if _, done := completed[task.TaskIdentifier]; done {
			alreadyCompletedCount++
			continue
		}
		remaining = append(remaining, task)
	}
	return remaining, alreadyCompletedCount
}

// recordSuccessfulTaskCompletion folds one successful TaskOutcome into
// every attached optional collaborator (checkpoint store, cost
// accountant, metrics sink), each independently no-op-safe if not
// attached.
func (dispatcher *Dispatcher) recordSuccessfulTaskCompletion(ctx context.Context, job domain.EncodingJob, outcome TaskOutcome) {
	if dispatcher.checkpointStore != nil {
		if err := dispatcher.checkpointStore.RecordChunkCompleted(ctx, job.JobIdentifier, outcome.Task.TaskIdentifier, outcome.Task.OutputChunkFilePath); err != nil {
			dispatcher.logger.Warn("failed to record chunk checkpoint",
				"taskIdentifier", outcome.Task.TaskIdentifier, "error", err.Error())
		}
	}

	if dispatcher.costAccountant != nil {
		resourceKind := cost.ResourceKindCpu
		if outcome.Task.TargetCodec.IsHardwareAccelerated() {
			resourceKind = cost.ResourceKindGpu
		}
		record := dispatcher.costAccountant.RecordChunkCompletion(
			job.JobIdentifier, outcome.Task.TaskIdentifier, resourceKind, outcome.TotalWallClockDuration.Seconds())
		dispatcher.metricsSink.RecordCost(resourceKind.String(), record.ResourceSecondsConsumed, record.EstimatedCostUsd)
	}
}

// GetJobProgressSnapshot returns the current JobProgressSnapshot for the
// given jobIdentifier if it is currently being dispatched, or
// (JobProgressSnapshot{}, false) if no such job is in flight (either it
// was never submitted, or it already completed and its tracker was
// cleaned up).
func (dispatcher *Dispatcher) GetJobProgressSnapshot(jobIdentifier string) (JobProgressSnapshot, bool) {
	dispatcher.progressTrackersMutex.RLock()
	tracker, ok := dispatcher.progressTrackers[jobIdentifier]
	dispatcher.progressTrackersMutex.RUnlock()
	if !ok {
		return JobProgressSnapshot{}, false
	}
	return tracker.snapshot(), true
}

// errString safely renders a possibly-nil error as a string for logging.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
