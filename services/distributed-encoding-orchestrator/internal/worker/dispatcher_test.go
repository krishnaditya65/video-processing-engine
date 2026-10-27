package worker

import (
	"context"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
)

// buildFixtureJob constructs a small EncodingJob (2 scenes, 2 ladder
// rungs, 1s chunks) used across Dispatcher tests.
func buildFixtureJob(t *testing.T) domain.EncodingJob {
	t.Helper()
	job, err := domain.NewEncodingJobBuilder("job-fixture", "s3://bucket/fixture.mov").
		WithTargetCodec(ffmpeg.CodecH264).
		WithSceneBoundaries([]domain.SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 48, FrameRateFramesPerSecond: 24},   // 2s, 1 chunk
			{SceneIndex: 1, StartFrameIndex: 48, EndFrameIndex: 144, FrameRateFramesPerSecond: 24}, // 4s, 2 chunks
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
	return job
}

// TestDispatcher_DispatchEncodingJob_AllTasksSucceedProducesCompletedSummary
// verifies end-to-end dispatch through a FakeSubprocessRunner: the
// fixture job fans out into (1+2 chunks)*2 rungs = 6 tasks, and with an
// always-succeeding fake ffmpeg, the summary must report 6/6 succeeded
// and FinalStatus()==Completed.
func TestDispatcher_DispatchEncodingJob_AllTasksSucceedProducesCompletedSummary(t *testing.T) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		ScriptedStandardErrorLines: []string{"frame=10", "progress=end"},
	}
	processor := NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", nil, nil)
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})
	dispatcher := NewDispatcher(pool, nil, 64, nil)

	job := buildFixtureJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary := dispatcher.DispatchEncodingJob(ctx, job)

	const expectedTaskCount = 3 * 2 // 3 chunks total * 2 ladder rungs
	if summary.TotalTaskCount != expectedTaskCount {
		t.Fatalf("expected %d total tasks, got %d", expectedTaskCount, summary.TotalTaskCount)
	}
	if summary.SucceededTaskCount != expectedTaskCount {
		t.Fatalf("expected all %d tasks to succeed, got %d succeeded, %d failed (failed ids: %v)",
			expectedTaskCount, summary.SucceededTaskCount, summary.FailedTaskCount, summary.FailedTaskIdentifiers)
	}
	if summary.FinalStatus() != domain.EncodingJobStatusCompleted {
		t.Fatalf("expected FinalStatus()==Completed, got %v", summary.FinalStatus())
	}
	if fakeRunner.InvocationCount() != expectedTaskCount {
		t.Fatalf("expected ffmpeg invoked exactly %d times, got %d", expectedTaskCount, fakeRunner.InvocationCount())
	}
}

// TestDispatcher_DispatchEncodingJob_PersistentFailuresProduceFailedSummary
// verifies that when ffmpeg always fails, every task exhausts its
// retries and the summary correctly reports FinalStatus()==Failed with
// every task identifier listed.
func TestDispatcher_DispatchEncodingJob_PersistentFailuresProduceFailedSummary(t *testing.T) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		FailFirstNInvocations: 1_000_000, // always fail
	}
	processor := NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", nil, nil)
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{
		MaxConcurrentTasks: 4,
		RetryPolicy: ExponentialBackoffRetryPolicy{
			MaxAttempts: 2,
			BaseDelay:   1 * time.Millisecond,
			Multiplier:  1.0,
			MaxDelay:    5 * time.Millisecond,
		},
		CircuitBreakerFailureThreshold: 1000, // avoid breaker short-circuiting attempts for this assertion
	})
	dispatcher := NewDispatcher(pool, nil, 64, nil)

	job := buildFixtureJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary := dispatcher.DispatchEncodingJob(ctx, job)

	if summary.FinalStatus() != domain.EncodingJobStatusFailed {
		t.Fatalf("expected FinalStatus()==Failed, got %v", summary.FinalStatus())
	}
	if summary.FailedTaskCount != summary.TotalTaskCount {
		t.Fatalf("expected all %d tasks to fail, got %d failed", summary.TotalTaskCount, summary.FailedTaskCount)
	}
	if len(summary.FailedTaskIdentifiers) != summary.TotalTaskCount {
		t.Fatalf("expected FailedTaskIdentifiers to list every failed task, got %d entries for %d tasks",
			len(summary.FailedTaskIdentifiers), summary.TotalTaskCount)
	}
}

// TestDispatcher_GetJobProgressSnapshot_ReflectsInFlightProgress verifies
// that progress can be observed while a job is dispatching and is
// cleaned up (returns false) once the job completes.
func TestDispatcher_GetJobProgressSnapshot_ReflectsInFlightProgress(t *testing.T) {
	releaseCh := make(chan struct{})
	processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
		<-releaseCh
		return nil
	})
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})
	dispatcher := NewDispatcher(pool, nil, 64, nil)

	job := buildFixtureJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summaryCh := make(chan JobCompletionSummary, 1)
	go func() {
		summaryCh <- dispatcher.DispatchEncodingJob(ctx, job)
	}()

	// Poll briefly for the in-flight tracker to appear (it is registered
	// synchronously at the start of DispatchEncodingJob, but we still
	// allow the goroutine a moment to actually start running).
	var snapshot JobProgressSnapshot
	var ok bool
	for attempt := 0; attempt < 100; attempt++ {
		snapshot, ok = dispatcher.GetJobProgressSnapshot(job.JobIdentifier)
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("expected in-flight progress snapshot to be available")
	}
	if snapshot.TotalTaskCount != 6 {
		t.Fatalf("expected TotalTaskCount==6, got %d", snapshot.TotalTaskCount)
	}

	close(releaseCh)
	summary := <-summaryCh
	if summary.SucceededTaskCount != 6 {
		t.Fatalf("expected 6 succeeded, got %d", summary.SucceededTaskCount)
	}

	if _, stillTracked := dispatcher.GetJobProgressSnapshot(job.JobIdentifier); stillTracked {
		t.Fatal("expected progress tracker to be cleaned up after job completion")
	}
}
