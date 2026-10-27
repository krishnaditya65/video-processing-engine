// Package domain defines the core orchestration domain model shared
// across the distributed encoding orchestrator: an EncodingJob
// represents one source video asset to be transcoded into a full bitrate
// ladder across all its scenes; a ChunkEncodingTask represents one
// concrete unit of dispatchable work (one scene-chunk at one ladder
// rung); and JobQueue is the thread-safe, bounded, context-cancellable
// channel-backed queue the WorkerPool drains tasks from.
package domain

import (
	"context"
	"fmt"
	"time"

	"distributed-encoding-orchestrator/internal/ffmpeg"
)

// SceneBoundary describes one scene detected by the upstream ingestion
// service's structural map for a source asset: a contiguous half-open
// interval [StartFrameIndex, EndFrameIndex) of frames that the ingestion
// pipeline's shot-boundary detection determined belong to one visually
// coherent scene. The orchestrator fans a source asset out into one or
// more ChunkEncodingTask values per SceneBoundary (a scene longer than
// the configured max chunk duration is itself split into multiple
// fixed-duration chunks, all inheriting the same scene's ladder decision
// inputs).
type SceneBoundary struct {
	// SceneIndex is the zero-based ordinal position of this scene within
	// the asset's structural map.
	SceneIndex int
	// StartFrameIndex is the first frame (inclusive) belonging to this scene.
	StartFrameIndex int64
	// EndFrameIndex is one past the last frame belonging to this scene
	// (exclusive), matching Go slice-bound conventions.
	EndFrameIndex int64
	// FrameRateFramesPerSecond is the asset's frame rate, needed to
	// convert the frame-index interval into wall-clock chunk durations.
	FrameRateFramesPerSecond float64
}

// DurationSeconds returns how long this scene lasts in seconds, computed
// from its frame span and frame rate. Returns 0 if
// FrameRateFramesPerSecond is non-positive.
func (scene SceneBoundary) DurationSeconds() float64 {
	if scene.FrameRateFramesPerSecond <= 0 {
		return 0
	}
	return float64(scene.EndFrameIndex-scene.StartFrameIndex) / scene.FrameRateFramesPerSecond
}

// BitrateLadderRung is one target (resolution, bitrate) output rendition
// this job must produce for every chunk, typically pre-computed by the
// internal/ladder convex-hull optimizer per scene, but expressed here
// generically since the orchestrator also supports a fixed/manual ladder
// for assets where per-scene VMAF sampling has not been run.
type BitrateLadderRung struct {
	// RenditionLabel is a human-readable identifier such as "1080p-4500k".
	RenditionLabel string
	// WidthPixels and HeightPixels give the output resolution.
	WidthPixels  int
	HeightPixels int
	// BitrateKilobitsPerSecond is the target encode bitrate for this rung.
	BitrateKilobitsPerSecond int
}

// EncodingJobStatus enumerates the lifecycle states of an EncodingJob.
type EncodingJobStatus int

const (
	// EncodingJobStatusPending means the job has been constructed and
	// enqueued but no chunk tasks have started yet.
	EncodingJobStatusPending EncodingJobStatus = iota
	// EncodingJobStatusInProgress means at least one chunk task has
	// started and not all have completed.
	EncodingJobStatusInProgress
	// EncodingJobStatusCompleted means every chunk task for the job
	// finished successfully.
	EncodingJobStatusCompleted
	// EncodingJobStatusFailed means the job was abandoned because one or
	// more chunk tasks exhausted their retry budget, or the job's
	// context was cancelled.
	EncodingJobStatusFailed
)

// String renders an EncodingJobStatus as a short label for logs/metrics.
func (status EncodingJobStatus) String() string {
	switch status {
	case EncodingJobStatusPending:
		return "pending"
	case EncodingJobStatusInProgress:
		return "in_progress"
	case EncodingJobStatusCompleted:
		return "completed"
	case EncodingJobStatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// EncodingJob represents one source video asset submitted to the
// distributed encoding factory for transcoding into a full bitrate
// ladder, fanned out into per-scene, per-rung ChunkEncodingTask units.
//
// EncodingJob values should be constructed via NewEncodingJobBuilder
// rather than struct-literal, both to guarantee required fields are set
// and because the Builder validates cross-field invariants (e.g. at
// least one scene, at least one ladder rung) that a bare struct literal
// cannot enforce at compile time.
type EncodingJob struct {
	// JobIdentifier uniquely identifies this job, typically a UUID
	// assigned at ingestion time.
	JobIdentifier string
	// SourceAssetURI locates the source media (e.g. an S3/GCS URI) the
	// ingestion service already validated and structurally mapped.
	SourceAssetURI string
	// TargetCodec selects which CodecArgumentBuilder strategy every
	// chunk of this job will be encoded with.
	TargetCodec ffmpeg.SupportedVideoCodec
	// SceneBoundaries is the ingestion service's structural map for this
	// asset, in ascending SceneIndex order.
	SceneBoundaries []SceneBoundary
	// BitrateLadderRungs is the set of (resolution, bitrate) renditions
	// to produce for every chunk of this job.
	BitrateLadderRungs []BitrateLadderRung
	// MaxChunkDurationSeconds bounds how long any single
	// ChunkEncodingTask's media span may be; scenes longer than this are
	// split into multiple consecutive chunks.
	MaxChunkDurationSeconds float64
	// SubmittedAt records when the job was accepted by the orchestrator.
	SubmittedAt time.Time
	// Status is the job's current lifecycle state. In a real deployment
	// this would be updated by the Dispatcher under a mutex/atomic as
	// chunk tasks complete; this in-memory demo tracks it via
	// JobQueue/Dispatcher bookkeeping rather than mutation of this
	// struct field directly, to avoid data races on shared EncodingJob
	// values (see JobProgressTracker in dispatcher.go).
	Status EncodingJobStatus
}

// ChunkEncodingTask is the atomic, independently-dispatchable unit of
// work the WorkerPool processes: one scene-derived chunk of one
// EncodingJob, encoded to one specific BitrateLadderRung.
//
// A single EncodingJob with S scenes (each split into at most C chunks)
// and R ladder rungs fans out into up to S*C*R ChunkEncodingTask values,
// which is the mechanism by which a two-hour film becomes thousands of
// independently-schedulable units of work distributed across the worker
// fleet.
type ChunkEncodingTask struct {
	// TaskIdentifier uniquely identifies this task within its parent job,
	// formatted as "<jobIdentifier>/scene<sceneIndex>/chunk<chunkIndex>/<renditionLabel>".
	TaskIdentifier string
	// ParentJobIdentifier links back to the owning EncodingJob.
	ParentJobIdentifier string
	// SceneIndex identifies which SceneBoundary this chunk was derived from.
	SceneIndex int
	// ChunkIndexWithinScene is the zero-based ordinal of this chunk among
	// the (possibly several) chunks a long scene was split into.
	ChunkIndexWithinScene int
	// ChunkStartFrameIndex and ChunkEndFrameIndex give this specific
	// chunk's frame span (a sub-interval of, or equal to, its parent
	// scene's span).
	ChunkStartFrameIndex int64
	ChunkEndFrameIndex   int64
	// TargetCodec is inherited from the parent EncodingJob.
	TargetCodec ffmpeg.SupportedVideoCodec
	// TargetRung is the specific BitrateLadderRung this task encodes to.
	TargetRung BitrateLadderRung
	// InputChunkFilePath and OutputChunkFilePath are the concrete
	// filesystem/object-store locations ffmpeg should read from and
	// write to for this task.
	InputChunkFilePath  string
	OutputChunkFilePath string
	// MaximumAttemptCount bounds how many times the Dispatcher will
	// retry this task after a failed attempt before giving up and
	// marking it permanently failed.
	MaximumAttemptCount int
}

// String renders a ChunkEncodingTask as its TaskIdentifier for
// compact logging.
func (task ChunkEncodingTask) String() string {
	return task.TaskIdentifier
}

// FormatChunkTaskIdentifier builds the canonical TaskIdentifier string
// for a chunk of a given job/scene/chunk-index/rendition, so both the
// job fan-out logic and any later lookup code construct/parse identical
// identifiers.
func FormatChunkTaskIdentifier(jobIdentifier string, sceneIndex int, chunkIndexWithinScene int, renditionLabel string) string {
	return fmt.Sprintf("%s/scene%d/chunk%d/%s", jobIdentifier, sceneIndex, chunkIndexWithinScene, renditionLabel)
}

// FanOutEncodingJobIntoChunkEncodingTasks expands an EncodingJob's scene
// boundaries and bitrate ladder into the full ordered slice of
// ChunkEncodingTask units that must be completed for the job to be
// considered done. Long scenes (DurationSeconds() >
// job.MaxChunkDurationSeconds) are split into consecutive
// equal-length-in-frames chunks, each of which is then crossed with
// every rung in job.BitrateLadderRungs.
//
// Params:
//   - job: the EncodingJob to expand. job.MaxChunkDurationSeconds <= 0 is
//     treated as "do not split scenes" (one chunk per scene).
//   - inputOutputPathTemplate: a function computing the
//     (inputChunkFilePath, outputChunkFilePath) pair for a given
//     TaskIdentifier, so path/URI conventions stay outside this pure
//     domain package.
//   - maximumAttemptCount: copied onto every produced task's
//     MaximumAttemptCount.
//
// Returns the tasks in ascending (sceneIndex, chunkIndexWithinScene,
// rung-order-as-given) order. This function is pure and allocates a new
// slice; it is safe to call concurrently for different jobs, and could
// safely be called concurrently for the same job too since it only reads
// from job.
func FanOutEncodingJobIntoChunkEncodingTasks(
	job EncodingJob,
	inputOutputPathTemplate func(taskIdentifier string) (inputPath string, outputPath string),
	maximumAttemptCount int,
) []ChunkEncodingTask {
	var tasks []ChunkEncodingTask

	for _, scene := range job.SceneBoundaries {
		chunkFrameSpans := splitSceneIntoChunkFrameSpans(scene, job.MaxChunkDurationSeconds)

		for chunkIndex, frameSpan := range chunkFrameSpans {
			for _, rung := range job.BitrateLadderRungs {
				taskIdentifier := FormatChunkTaskIdentifier(job.JobIdentifier, scene.SceneIndex, chunkIndex, rung.RenditionLabel)
				inputPath, outputPath := inputOutputPathTemplate(taskIdentifier)

				tasks = append(tasks, ChunkEncodingTask{
					TaskIdentifier:        taskIdentifier,
					ParentJobIdentifier:   job.JobIdentifier,
					SceneIndex:            scene.SceneIndex,
					ChunkIndexWithinScene: chunkIndex,
					ChunkStartFrameIndex:  frameSpan.startFrameIndex,
					ChunkEndFrameIndex:    frameSpan.endFrameIndex,
					TargetCodec:           job.TargetCodec,
					TargetRung:            rung,
					InputChunkFilePath:    inputPath,
					OutputChunkFilePath:   outputPath,
					MaximumAttemptCount:   maximumAttemptCount,
				})
			}
		}
	}

	return tasks
}

// frameSpan is an internal half-open [start, end) frame interval used
// while splitting a scene into chunks.
type frameSpan struct {
	startFrameIndex int64
	endFrameIndex   int64
}

// splitSceneIntoChunkFrameSpans divides one SceneBoundary into one or
// more consecutive frameSpan values, each spanning at most
// maxChunkDurationSeconds of media (converted to a frame count via the
// scene's frame rate). If maxChunkDurationSeconds <= 0 or the scene's
// frame rate is non-positive, the whole scene is returned as a single
// span.
func splitSceneIntoChunkFrameSpans(scene SceneBoundary, maxChunkDurationSeconds float64) []frameSpan {
	totalFrames := scene.EndFrameIndex - scene.StartFrameIndex
	if totalFrames <= 0 {
		return nil
	}
	if maxChunkDurationSeconds <= 0 || scene.FrameRateFramesPerSecond <= 0 {
		return []frameSpan{{startFrameIndex: scene.StartFrameIndex, endFrameIndex: scene.EndFrameIndex}}
	}

	maxFramesPerChunk := int64(maxChunkDurationSeconds * scene.FrameRateFramesPerSecond)
	if maxFramesPerChunk <= 0 {
		maxFramesPerChunk = totalFrames
	}

	var spans []frameSpan
	for cursor := scene.StartFrameIndex; cursor < scene.EndFrameIndex; cursor += maxFramesPerChunk {
		end := cursor + maxFramesPerChunk
		if end > scene.EndFrameIndex {
			end = scene.EndFrameIndex
		}
		spans = append(spans, frameSpan{startFrameIndex: cursor, endFrameIndex: end})
	}
	return spans
}

// JobSubmissionContext bundles the context.Context governing an
// EncodingJob's lifetime with the job itself, so JobQueue can propagate
// cancellation (e.g. a customer deletes the asset mid-encode, or a
// deployment-wide shutdown is triggered) down to every in-flight
// ChunkEncodingTask derived from it.
type JobSubmissionContext struct {
	// Context is cancelled to abandon all remaining, not-yet-started
	// chunk tasks for Job; in-flight ffmpeg subprocesses are also
	// terminated promptly (see ffmpeg.SubprocessRunner's context
	// handling).
	Context context.Context
	// Job is the submitted EncodingJob.
	Job EncodingJob
}
