package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/checkpoint"
	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
)

// callCountingProcessor is a ChunkEncodingTaskProcessor that records every
// TaskIdentifier it is asked to process (guarded by a mutex, since the
// WorkerPool invokes it from many concurrent goroutines) and always
// succeeds. It exists to let tests assert exactly which chunks a
// Dispatcher actually dispatched to the underlying processor — the key
// observable needed to prove checkpoint-resume correctness: no chunk
// already checkpointed as complete should ever appear in this log again.
type callCountingProcessor struct {
	mutex            sync.Mutex
	processedTaskIDs []string
}

func (p *callCountingProcessor) ProcessChunkEncodingTask(_ context.Context, task domain.ChunkEncodingTask) error {
	p.mutex.Lock()
	p.processedTaskIDs = append(p.processedTaskIDs, task.TaskIdentifier)
	p.mutex.Unlock()
	return nil
}

func (p *callCountingProcessor) snapshotProcessedTaskIDs() []string {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	out := make([]string, len(p.processedTaskIDs))
	copy(out, p.processedTaskIDs)
	return out
}

// artificiallyDelayingProcessor wraps another ChunkEncodingTaskProcessor
// and sleeps for a fixed delay before delegating, purely so tests can
// deterministically win the race between "background goroutine cancels
// the dispatch context after observing enough progress" and "the
// WorkerPool finishes the whole job before that goroutine gets a chance
// to act" — without this, a fast test machine could occasionally finish
// all tasks before the polling goroutine ever cancels anything, making
// the simulated-preemption scenario flaky rather than deterministic.
type artificiallyDelayingProcessor struct {
	inner ChunkEncodingTaskProcessor
	delay time.Duration
}

func (p artificiallyDelayingProcessor) ProcessChunkEncodingTask(ctx context.Context, task domain.ChunkEncodingTask) error {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.inner.ProcessChunkEncodingTask(ctx, task)
}

// TestDispatcher_CheckpointResume_SimulatedPreemptionLosesNoWorkAndDuplicatesNone
// proves the core checkpoint-resume correctness property end to end using
// two dispatch "phases" against the SAME checkpoint.JobCheckpointStore,
// simulating a preemptible worker fleet where the first phase is
// interrupted (preempted) partway through a job and a second phase later
// resumes the same job:
//
//   - Phase 1 dispatches the job through a Dispatcher wired to a shared
//     InMemoryJobCheckpointStore, but its context is cancelled by a
//     background goroutine as soon as GetJobProgressSnapshot first
//     observes at least K completed chunks — simulating the moment a
//     preemption signal arrives after K chunks have already finished and
//     been checkpointed, but before the whole job completed.
//   - Phase 2 dispatches the SAME job (fresh context, same processor, same
//     checkpoint store) and must: (a) never re-process any TaskIdentifier
//     the processor already saw in phase 1 (no duplicate work), and (b) by
//     the time it returns, have processed (across both phases combined)
//     every single TaskIdentifier the job fans out into exactly once (no
//     data loss), with a final JobCompletionSummary reporting
//     SucceededTaskCount == TotalTaskCount.
func TestDispatcher_CheckpointResume_SimulatedPreemptionLosesNoWorkAndDuplicatesNone(t *testing.T) {
	job, err := domain.NewEncodingJobBuilder("resumable-job", "s3://bucket/resumable.mov").
		WithTargetCodec(ffmpeg.CodecH264).
		WithSceneBoundaries([]domain.SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 24, FrameRateFramesPerSecond: 24},  // 1s, 1 chunk
			{SceneIndex: 1, StartFrameIndex: 24, EndFrameIndex: 48, FrameRateFramesPerSecond: 24}, // 1s, 1 chunk
			{SceneIndex: 2, StartFrameIndex: 48, EndFrameIndex: 72, FrameRateFramesPerSecond: 24}, // 1s, 1 chunk
			{SceneIndex: 3, StartFrameIndex: 72, EndFrameIndex: 96, FrameRateFramesPerSecond: 24}, // 1s, 1 chunk
		}).
		WithBitrateLadderRungs([]domain.BitrateLadderRung{
			{RenditionLabel: "1080p", WidthPixels: 1920, HeightPixels: 1080, BitrateKilobitsPerSecond: 5000},
			{RenditionLabel: "720p", WidthPixels: 1280, HeightPixels: 720, BitrateKilobitsPerSecond: 2500},
		}).
		WithMaxChunkDurationSeconds(2.0).
		Build()
	if err != nil {
		t.Fatalf("failed to build fixture job: %v", err)
	}
	const totalTaskCount = 4 * 2 // 4 chunks * 2 rungs

	checkpointStore := checkpoint.NewInMemoryJobCheckpointStore()
	processor := &callCountingProcessor{}

	// --- Phase 1: an interrupted run, preempted after >= 2 chunks complete. ---
	// MaxConcurrentTasks=1 (serial dispatch) plus an artificial per-task
	// delay makes the preemption timing deterministic: see
	// artificiallyDelayingProcessor's doc comment.
	delayedProcessor := artificiallyDelayingProcessor{inner: processor, delay: 20 * time.Millisecond}
	pool1 := NewWorkerPool(delayedProcessor, WorkerPoolConfiguration{MaxConcurrentTasks: 1})
	dispatcher1 := NewDispatcher(pool1, nil, 64, nil).WithJobCheckpointStore(checkpointStore)

	ctx1, cancel1 := context.WithCancel(context.Background())
	const preemptAfterCompletedCount = 2
	go func() {
		// The progress tracker is registered synchronously at the start of
		// DispatchEncodingJob (called below, concurrently with this
		// goroutine), but that registration can genuinely race this
		// goroutine's very first poll — GetJobProgressSnapshot correctly
		// returns ok=false both "before the job has started" and "after
		// the job has already finished and been cleaned up", and those two
		// cases are indistinguishable from a single sample. So this loop
		// tolerates a bounded number of not-yet-registered misses before
		// giving up, rather than bailing out on the very first ok==false.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			snapshot, ok := dispatcher1.GetJobProgressSnapshot(job.JobIdentifier)
			if ok && snapshot.CompletedTaskCount >= preemptAfterCompletedCount {
				cancel1()
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	phase1Summary := dispatcher1.DispatchEncodingJob(ctx1, job)
	cancel1()

	if phase1Summary.SucceededTaskCount == 0 {
		t.Fatal("expected phase 1 to have completed at least some chunks before preemption")
	}
	if phase1Summary.SucceededTaskCount >= totalTaskCount {
		t.Fatal("expected phase 1 to be preempted before completing the whole job (test setup assumption violated)")
	}

	phase1ProcessedIDs := processor.snapshotProcessedTaskIDs()
	phase1ProcessedSet := make(map[string]bool, len(phase1ProcessedIDs))
	for _, id := range phase1ProcessedIDs {
		if phase1ProcessedSet[id] {
			t.Fatalf("processor was invoked twice for the same TaskIdentifier %q within phase 1 alone", id)
		}
		phase1ProcessedSet[id] = true
	}

	checkpointedAfterPhase1, err := checkpointStore.ListCompletedChunks(context.Background(), job.JobIdentifier)
	if err != nil {
		t.Fatalf("ListCompletedChunks returned error: %v", err)
	}
	if len(checkpointedAfterPhase1) != len(phase1ProcessedSet) {
		t.Fatalf("expected exactly the processed chunks to be checkpointed after phase 1: checkpointed=%d processed=%d",
			len(checkpointedAfterPhase1), len(phase1ProcessedSet))
	}

	// --- Phase 2: resume the same job with a fresh context. ---
	pool2 := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 4})
	dispatcher2 := NewDispatcher(pool2, nil, 64, nil).WithJobCheckpointStore(checkpointStore)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	phase2Summary := dispatcher2.DispatchEncodingJob(ctx2, job)

	// (a) No data loss and no duplicates: every TaskIdentifier the job
	// fans out into must have been processed EXACTLY ONCE across both
	// phases combined.
	allProcessedIDs := processor.snapshotProcessedTaskIDs()
	occurrenceCount := make(map[string]int, len(allProcessedIDs))
	for _, id := range allProcessedIDs {
		occurrenceCount[id]++
	}
	for id, count := range occurrenceCount {
		if count != 1 {
			t.Fatalf("expected TaskIdentifier %q to be processed exactly once across both phases, got %d", id, count)
		}
	}

	expectedTaskIDs := taskIdentifiersFor(job)
	if len(occurrenceCount) != len(expectedTaskIDs) {
		t.Fatalf("expected exactly %d distinct processed TaskIdentifiers (no data loss), got %d: %v",
			len(expectedTaskIDs), len(occurrenceCount), allProcessedIDs)
	}
	for _, expectedID := range expectedTaskIDs {
		if occurrenceCount[expectedID] != 1 {
			t.Fatalf("expected TaskIdentifier %q to have been processed exactly once, got %d (no data loss check)", expectedID, occurrenceCount[expectedID])
		}
	}

	// (b) Phase 2's own summary reports full completion: the
	// already-checkpointed chunks from phase 1 are counted as succeeded
	// without being reprocessed, plus every remaining chunk phase 2 itself
	// processed.
	if phase2Summary.TotalTaskCount != totalTaskCount {
		t.Fatalf("expected phase 2 TotalTaskCount=%d, got %d", totalTaskCount, phase2Summary.TotalTaskCount)
	}
	if phase2Summary.SucceededTaskCount != totalTaskCount {
		t.Fatalf("expected phase 2 SucceededTaskCount=%d (all chunks succeeded cumulatively), got %d (failed=%d, failedIDs=%v)",
			totalTaskCount, phase2Summary.SucceededTaskCount, phase2Summary.FailedTaskCount, phase2Summary.FailedTaskIdentifiers)
	}
	if phase2Summary.FinalStatus() != domain.EncodingJobStatusCompleted {
		t.Fatalf("expected phase 2 FinalStatus()==Completed, got %v", phase2Summary.FinalStatus())
	}

	// Final sanity: the checkpoint store now has every chunk recorded.
	checkpointedAfterPhase2, err := checkpointStore.ListCompletedChunks(context.Background(), job.JobIdentifier)
	if err != nil {
		t.Fatalf("ListCompletedChunks returned error: %v", err)
	}
	if len(checkpointedAfterPhase2) != totalTaskCount {
		t.Fatalf("expected all %d chunks checkpointed after phase 2, got %d", totalTaskCount, len(checkpointedAfterPhase2))
	}
}

// taskIdentifiersFor fans job out exactly as Dispatcher does internally
// (using the same domain.FanOutEncodingJobIntoChunkEncodingTasks
// function with a nil path builder, matching NewDispatcher(..., nil)'s
// default) and returns just the resulting TaskIdentifiers, giving tests a
// ground-truth expected set to compare processed-task logs against.
func taskIdentifiersFor(job domain.EncodingJob) []string {
	tasks := domain.FanOutEncodingJobIntoChunkEncodingTasks(job, func(taskID string) (string, string) {
		return taskID + ".in", taskID + ".out"
	}, 4)
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.TaskIdentifier
	}
	return ids
}

// TestDispatcher_CheckpointResume_PreCheckpointedTasksAreNeverReprocessed
// is a simpler, fully deterministic (no timing-based preemption)
// complement to the simulated-preemption test above: it directly
// pre-populates a checkpoint store with a chosen subset of a job's chunks
// (as if a prior, now-finished dispatch attempt had already completed
// them) and verifies a single DispatchEncodingJob call skips exactly
// those chunks — proving the skip-already-completed logic in isolation
// from any concurrency/timing concerns.
func TestDispatcher_CheckpointResume_PreCheckpointedTasksAreNeverReprocessed(t *testing.T) {
	job := buildFixtureJob(t) // 6 tasks: (1+2 chunks) * 2 rungs
	allTaskIDs := taskIdentifiersFor(job)
	if len(allTaskIDs) != 6 {
		t.Fatalf("expected fixture job to fan out into 6 tasks, got %d", len(allTaskIDs))
	}

	checkpointStore := checkpoint.NewInMemoryJobCheckpointStore()
	preCompleted := map[string]bool{allTaskIDs[0]: true, allTaskIDs[2]: true, allTaskIDs[4]: true}
	for taskID := range preCompleted {
		if err := checkpointStore.RecordChunkCompleted(context.Background(), job.JobIdentifier, taskID, taskID+".out"); err != nil {
			t.Fatalf("RecordChunkCompleted returned error: %v", err)
		}
	}

	processor := &callCountingProcessor{}
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})
	dispatcher := NewDispatcher(pool, nil, 64, nil).WithJobCheckpointStore(checkpointStore)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary := dispatcher.DispatchEncodingJob(ctx, job)

	if summary.TotalTaskCount != 6 {
		t.Fatalf("expected TotalTaskCount=6, got %d", summary.TotalTaskCount)
	}
	if summary.SucceededTaskCount != 6 {
		t.Fatalf("expected SucceededTaskCount=6 (3 pre-checkpointed + 3 freshly processed), got %d", summary.SucceededTaskCount)
	}

	processedIDs := processor.snapshotProcessedTaskIDs()
	if len(processedIDs) != 3 {
		t.Fatalf("expected exactly 3 tasks to actually be dispatched to the processor (skipping the 3 pre-checkpointed ones), got %d: %v",
			len(processedIDs), processedIDs)
	}
	for _, id := range processedIDs {
		if preCompleted[id] {
			t.Fatalf("processor was invoked for TaskIdentifier %q, which was already checkpointed as complete", id)
		}
	}
}
