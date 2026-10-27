package domain

import (
	"context"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/ffmpeg"
)

// TestFanOutEncodingJobIntoChunkEncodingTasks_SplitsLongSceneIntoChunks
// hand-verifies chunk splitting: a 10-second scene at 24fps with a
// 2-second max chunk duration must split into exactly 5 chunks (48
// frames each), crossed with 2 ladder rungs = 10 tasks.
func TestFanOutEncodingJobIntoChunkEncodingTasks_SplitsLongSceneIntoChunks(t *testing.T) {
	job := EncodingJob{
		JobIdentifier: "job-1",
		TargetCodec:   ffmpeg.CodecHevc,
		SceneBoundaries: []SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 240, FrameRateFramesPerSecond: 24}, // 10s
		},
		BitrateLadderRungs: []BitrateLadderRung{
			{RenditionLabel: "1080p"},
			{RenditionLabel: "720p"},
		},
		MaxChunkDurationSeconds: 2.0,
	}

	tasks := FanOutEncodingJobIntoChunkEncodingTasks(job, func(id string) (string, string) { return id, id }, 4)

	if len(tasks) != 10 {
		t.Fatalf("expected 5 chunks * 2 rungs = 10 tasks, got %d", len(tasks))
	}

	chunkIndices := map[int]bool{}
	for _, task := range tasks {
		chunkIndices[task.ChunkIndexWithinScene] = true
		if task.ChunkEndFrameIndex-task.ChunkStartFrameIndex != 48 {
			t.Errorf("expected each chunk to span 48 frames (2s @ 24fps), got %d for task %s",
				task.ChunkEndFrameIndex-task.ChunkStartFrameIndex, task.TaskIdentifier)
		}
		if task.MaximumAttemptCount != 4 {
			t.Errorf("expected MaximumAttemptCount propagated as 4, got %d", task.MaximumAttemptCount)
		}
	}
	if len(chunkIndices) != 5 {
		t.Fatalf("expected 5 distinct chunk indices, got %d: %v", len(chunkIndices), chunkIndices)
	}
}

// TestFanOutEncodingJobIntoChunkEncodingTasks_ShortSceneProducesSingleChunk
// verifies a scene shorter than MaxChunkDurationSeconds is not split.
func TestFanOutEncodingJobIntoChunkEncodingTasks_ShortSceneProducesSingleChunk(t *testing.T) {
	job := EncodingJob{
		JobIdentifier: "job-2",
		SceneBoundaries: []SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 24, FrameRateFramesPerSecond: 24}, // 1s
		},
		BitrateLadderRungs:      []BitrateLadderRung{{RenditionLabel: "1080p"}},
		MaxChunkDurationSeconds: 2.0,
	}
	tasks := FanOutEncodingJobIntoChunkEncodingTasks(job, func(id string) (string, string) { return id, id }, 1)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task for a scene shorter than max chunk duration, got %d", len(tasks))
	}
	if tasks[0].ChunkStartFrameIndex != 0 || tasks[0].ChunkEndFrameIndex != 24 {
		t.Fatalf("expected chunk spanning the whole scene, got [%d,%d)", tasks[0].ChunkStartFrameIndex, tasks[0].ChunkEndFrameIndex)
	}
}

// TestFanOutEncodingJobIntoChunkEncodingTasks_TwoHourFilmProducesExpectedChunkCount
// is the throughput-math sanity check referenced in the README: a
// 2-hour film (7200s) at 24fps split into 2-second chunks should
// produce 3600 chunks per scene-span, matching the documented
// "~3600 two-second chunks" target for a single-scene approximation.
func TestFanOutEncodingJobIntoChunkEncodingTasks_TwoHourFilmProducesExpectedChunkCount(t *testing.T) {
	const frameRate = 24.0
	const totalSeconds = 7200.0
	job := EncodingJob{
		JobIdentifier: "job-3",
		SceneBoundaries: []SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: int64(totalSeconds * frameRate), FrameRateFramesPerSecond: frameRate},
		},
		BitrateLadderRungs:      []BitrateLadderRung{{RenditionLabel: "only-rung"}},
		MaxChunkDurationSeconds: 2.0,
	}
	tasks := FanOutEncodingJobIntoChunkEncodingTasks(job, func(id string) (string, string) { return id, id }, 1)
	if len(tasks) != 3600 {
		t.Fatalf("expected 3600 chunks for a 2-hour film at 2s/chunk, got %d", len(tasks))
	}
}

// TestFanOutEncodingJobIntoChunkEncodingTasks_TaskIdentifiersAreUnique
// verifies no two produced tasks share a TaskIdentifier, which would
// silently corrupt downstream progress tracking (map-keyed by ID).
func TestFanOutEncodingJobIntoChunkEncodingTasks_TaskIdentifiersAreUnique(t *testing.T) {
	job := EncodingJob{
		JobIdentifier: "job-4",
		SceneBoundaries: []SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 100, FrameRateFramesPerSecond: 25},
			{SceneIndex: 1, StartFrameIndex: 100, EndFrameIndex: 300, FrameRateFramesPerSecond: 25},
		},
		BitrateLadderRungs: []BitrateLadderRung{
			{RenditionLabel: "a"}, {RenditionLabel: "b"}, {RenditionLabel: "c"},
		},
		MaxChunkDurationSeconds: 1.0,
	}
	tasks := FanOutEncodingJobIntoChunkEncodingTasks(job, func(id string) (string, string) { return id, id }, 1)

	seen := make(map[string]bool)
	for _, task := range tasks {
		if seen[task.TaskIdentifier] {
			t.Fatalf("duplicate task identifier: %s", task.TaskIdentifier)
		}
		seen[task.TaskIdentifier] = true
	}
}

// TestSceneBoundary_DurationSecondsComputation hand-verifies the
// duration computation and its zero-frame-rate guard.
func TestSceneBoundary_DurationSecondsComputation(t *testing.T) {
	scene := SceneBoundary{StartFrameIndex: 0, EndFrameIndex: 48, FrameRateFramesPerSecond: 24}
	if got := scene.DurationSeconds(); got != 2.0 {
		t.Fatalf("expected 2.0s, got %v", got)
	}
	zeroRate := SceneBoundary{StartFrameIndex: 0, EndFrameIndex: 48, FrameRateFramesPerSecond: 0}
	if got := zeroRate.DurationSeconds(); got != 0 {
		t.Fatalf("expected 0 for zero frame rate, got %v", got)
	}
}

// TestEncodingJobBuilder_BuildSucceedsWithAllRequiredFields verifies the
// happy path of the Builder pattern.
func TestEncodingJobBuilder_BuildSucceedsWithAllRequiredFields(t *testing.T) {
	job, err := NewEncodingJobBuilder("job-x", "s3://bucket/x.mov").
		WithTargetCodec(ffmpeg.CodecAv1).
		WithSceneBoundaries([]SceneBoundary{{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 10, FrameRateFramesPerSecond: 10}}).
		WithBitrateLadderRungs([]BitrateLadderRung{{RenditionLabel: "r"}}).
		Build()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.Status != EncodingJobStatusPending {
		t.Fatalf("expected new job status Pending, got %v", job.Status)
	}
	if job.TargetCodec != ffmpeg.CodecAv1 {
		t.Fatalf("expected codec AV1, got %v", job.TargetCodec)
	}
	if job.MaxChunkDurationSeconds != 2.0 {
		t.Fatalf("expected default MaxChunkDurationSeconds=2.0, got %v", job.MaxChunkDurationSeconds)
	}
}

// TestEncodingJobBuilder_BuildFailsWithMissingRequiredFields verifies
// every required-field validation branch.
func TestEncodingJobBuilder_BuildFailsWithMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		buildFn func() (EncodingJob, error)
	}{
		{"missing jobIdentifier", func() (EncodingJob, error) {
			return NewEncodingJobBuilder("", "uri").
				WithTargetCodec(ffmpeg.CodecH264).
				WithSceneBoundaries([]SceneBoundary{{EndFrameIndex: 1}}).
				WithBitrateLadderRungs([]BitrateLadderRung{{}}).
				Build()
		}},
		{"missing sourceAssetURI", func() (EncodingJob, error) {
			return NewEncodingJobBuilder("id", "").
				WithTargetCodec(ffmpeg.CodecH264).
				WithSceneBoundaries([]SceneBoundary{{EndFrameIndex: 1}}).
				WithBitrateLadderRungs([]BitrateLadderRung{{}}).
				Build()
		}},
		{"missing codec", func() (EncodingJob, error) {
			return NewEncodingJobBuilder("id", "uri").
				WithSceneBoundaries([]SceneBoundary{{EndFrameIndex: 1}}).
				WithBitrateLadderRungs([]BitrateLadderRung{{}}).
				Build()
		}},
		{"missing scenes", func() (EncodingJob, error) {
			return NewEncodingJobBuilder("id", "uri").
				WithTargetCodec(ffmpeg.CodecH264).
				WithBitrateLadderRungs([]BitrateLadderRung{{}}).
				Build()
		}},
		{"missing rungs", func() (EncodingJob, error) {
			return NewEncodingJobBuilder("id", "uri").
				WithTargetCodec(ffmpeg.CodecH264).
				WithSceneBoundaries([]SceneBoundary{{EndFrameIndex: 1}}).
				Build()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.buildFn()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

// TestJobQueue_EnqueueDequeueRoundTrip verifies basic FIFO enqueue/dequeue.
func TestJobQueue_EnqueueDequeueRoundTrip(t *testing.T) {
	queue := NewJobQueue(4)
	ctx := context.Background()

	task := ChunkEncodingTask{TaskIdentifier: "t1"}
	if err := queue.Enqueue(ctx, task); err != nil {
		t.Fatalf("unexpected enqueue error: %v", err)
	}

	got, ok, err := queue.Dequeue(ctx)
	if err != nil || !ok {
		t.Fatalf("unexpected dequeue result: got=%v ok=%v err=%v", got, ok, err)
	}
	if got.TaskIdentifier != "t1" {
		t.Fatalf("expected t1, got %s", got.TaskIdentifier)
	}
}

// TestJobQueue_DequeueAfterCloseDrainsRemainingThenReturnsNotOk verifies
// that Close allows already-buffered tasks to still be drained before
// signalling completion via (zero, false, nil).
func TestJobQueue_DequeueAfterCloseDrainsRemainingThenReturnsNotOk(t *testing.T) {
	queue := NewJobQueue(4)
	ctx := context.Background()

	_ = queue.Enqueue(ctx, ChunkEncodingTask{TaskIdentifier: "a"})
	_ = queue.Enqueue(ctx, ChunkEncodingTask{TaskIdentifier: "b"})
	queue.Close()

	first, ok1, _ := queue.Dequeue(ctx)
	second, ok2, _ := queue.Dequeue(ctx)
	_, ok3, err3 := queue.Dequeue(ctx)

	if !ok1 || !ok2 || first.TaskIdentifier != "a" || second.TaskIdentifier != "b" {
		t.Fatalf("expected to drain buffered tasks a,b before close signal, got first=%v second=%v", first, second)
	}
	if ok3 || err3 != nil {
		t.Fatalf("expected (false, nil) once drained and closed, got ok=%v err=%v", ok3, err3)
	}
}

// TestJobQueue_EnqueueAfterCloseReturnsErrQueueClosed verifies the
// documented post-close Enqueue error.
func TestJobQueue_EnqueueAfterCloseReturnsErrQueueClosed(t *testing.T) {
	queue := NewJobQueue(2)
	queue.Close()

	err := queue.Enqueue(context.Background(), ChunkEncodingTask{TaskIdentifier: "late"})
	if err != ErrQueueClosed {
		t.Fatalf("expected ErrQueueClosed, got %v", err)
	}
}

// TestJobQueue_EnqueueRespectsContextCancellationWhenFull verifies that
// Enqueue on a full queue with an already-cancelled context returns
// promptly with a context error rather than blocking forever.
func TestJobQueue_EnqueueRespectsContextCancellationWhenFull(t *testing.T) {
	queue := NewJobQueue(1)
	_ = queue.Enqueue(context.Background(), ChunkEncodingTask{TaskIdentifier: "fills-buffer"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := queue.Enqueue(ctx, ChunkEncodingTask{TaskIdentifier: "blocked"})
	if err == nil {
		t.Fatal("expected context deadline error when enqueueing into a full queue past deadline")
	}
}

// TestJobQueue_TasksChannelSupportsRangeConsumption verifies the
// idiomatic `for task := range queue.Tasks()` consumption pattern used
// by WorkerPool.
func TestJobQueue_TasksChannelSupportsRangeConsumption(t *testing.T) {
	queue := NewJobQueue(8)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = queue.Enqueue(ctx, ChunkEncodingTask{TaskIdentifier: string(rune('a' + i))})
	}
	queue.Close()

	count := 0
	for range queue.Tasks() {
		count++
	}
	if count != 5 {
		t.Fatalf("expected to range over 5 tasks, got %d", count)
	}
}

// TestFormatChunkTaskIdentifier_ProducesExpectedFormat hand-verifies the
// canonical identifier format.
func TestFormatChunkTaskIdentifier_ProducesExpectedFormat(t *testing.T) {
	got := FormatChunkTaskIdentifier("job-9", 2, 3, "1080p-5000k")
	want := "job-9/scene2/chunk3/1080p-5000k"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestEncodingJobStatus_StringRendersKnownLabels verifies String().
func TestEncodingJobStatus_StringRendersKnownLabels(t *testing.T) {
	cases := map[EncodingJobStatus]string{
		EncodingJobStatusPending:    "pending",
		EncodingJobStatusInProgress: "in_progress",
		EncodingJobStatusCompleted:  "completed",
		EncodingJobStatusFailed:     "failed",
		EncodingJobStatus(999):      "unknown",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("status %d: expected %q, got %q", status, want, got)
		}
	}
}
