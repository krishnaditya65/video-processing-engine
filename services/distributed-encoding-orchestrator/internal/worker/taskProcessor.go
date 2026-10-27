// Package worker implements the concurrent fan-out execution layer of
// the distributed encoding orchestrator: the WorkerPool that drains a
// domain.JobQueue with bounded concurrency (semaphore pattern) over
// goroutines, the Dispatcher-level retry/exponential-backoff logic, and
// the FfmpegBackedTaskProcessor that turns a domain.ChunkEncodingTask
// into an actual ffmpeg invocation via the ffmpeg package's Strategy
// (CodecArgumentBuilder) and SubprocessRunner abstractions.
package worker

import (
	"context"
	"fmt"

	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
	"distributed-encoding-orchestrator/internal/logging"
)

// ChunkEncodingTaskProcessor is the abstraction the WorkerPool depends on
// to actually perform one unit of work. Decoupling the pool's
// concurrency/retry machinery from the concrete "how do we encode a
// chunk" logic lets tests substitute a trivial fake processor (e.g. one
// that just sleeps or fails on schedule) to exercise pool behavior
// without any ffmpeg involvement at all.
//
// Concurrency/safety: implementations must be safe for concurrent
// invocation from many goroutines simultaneously, since the WorkerPool
// may have many ChunkEncodingTaskProcessor.ProcessChunkEncodingTask calls
// in flight at once (up to its configured concurrency bound).
type ChunkEncodingTaskProcessor interface {
	// ProcessChunkEncodingTask performs the work described by task and
	// returns a non-nil error on failure. Implementations should honor
	// ctx cancellation promptly (in particular, an ffmpeg-backed
	// implementation must propagate ctx into the subprocess call so a
	// cancelled job's in-flight encodes are killed rather than left to
	// run to completion).
	ProcessChunkEncodingTask(ctx context.Context, task domain.ChunkEncodingTask) error
}

// ChunkEncodingTaskProcessorFunc adapts a plain function to the
// ChunkEncodingTaskProcessor interface, analogous to http.HandlerFunc.
// Primarily useful in tests for constructing lightweight fake processors
// inline.
type ChunkEncodingTaskProcessorFunc func(ctx context.Context, task domain.ChunkEncodingTask) error

// ProcessChunkEncodingTask implements ChunkEncodingTaskProcessor by
// calling the wrapped function.
func (f ChunkEncodingTaskProcessorFunc) ProcessChunkEncodingTask(ctx context.Context, task domain.ChunkEncodingTask) error {
	return f(ctx, task)
}

// FfmpegBackedChunkEncodingTaskProcessor is the production
// ChunkEncodingTaskProcessor implementation: it translates a
// domain.ChunkEncodingTask into a ffmpeg.ChunkEncodeSpecification, builds
// the codec-specific CLI arguments via the appropriate
// ffmpeg.CodecArgumentBuilder strategy, and executes them via an injected
// ffmpeg.SubprocessRunner, streaming progress through a
// ffmpeg.StreamingFfmpegProgressParser.
//
// Concurrency/safety: safe for concurrent use — holds only immutable
// configuration (binary path, runner, logger) and constructs all
// per-call mutable state (the progress parser) fresh inside each call to
// ProcessChunkEncodingTask.
type FfmpegBackedChunkEncodingTaskProcessor struct {
	subprocessRunner  ffmpeg.SubprocessRunner
	ffmpegBinaryPath  string
	logger            logging.StructuredEventLogger
	onProgressUpdated func(task domain.ChunkEncodingTask, snapshot ffmpeg.EncodingProgressSnapshot)
}

// NewFfmpegBackedChunkEncodingTaskProcessor constructs the production
// task processor.
//
// Params:
//   - subprocessRunner: typically ffmpeg.NewRealOsExecSubprocessRunner()
//     in production, or a *ffmpeg.FakeSubprocessRunner in tests.
//   - ffmpegBinaryPath: path/name of the ffmpeg executable to invoke,
//     e.g. "ffmpeg" (resolved via PATH) or an absolute path.
//   - logger: structured logger for per-task diagnostic events.
//   - onProgressUpdated: optional callback invoked for every parsed
//     ffmpeg progress snapshot; pass nil to ignore progress entirely.
func NewFfmpegBackedChunkEncodingTaskProcessor(
	subprocessRunner ffmpeg.SubprocessRunner,
	ffmpegBinaryPath string,
	logger logging.StructuredEventLogger,
	onProgressUpdated func(task domain.ChunkEncodingTask, snapshot ffmpeg.EncodingProgressSnapshot),
) *FfmpegBackedChunkEncodingTaskProcessor {
	if ffmpegBinaryPath == "" {
		ffmpegBinaryPath = "ffmpeg"
	}
	if logger == nil {
		logger = logging.NewDiscardingStructuredEventLogger()
	}
	return &FfmpegBackedChunkEncodingTaskProcessor{
		subprocessRunner:  subprocessRunner,
		ffmpegBinaryPath:  ffmpegBinaryPath,
		logger:            logger,
		onProgressUpdated: onProgressUpdated,
	}
}

// ProcessChunkEncodingTask implements ChunkEncodingTaskProcessor by
// building and running the ffmpeg command line for task.
func (processor *FfmpegBackedChunkEncodingTaskProcessor) ProcessChunkEncodingTask(ctx context.Context, task domain.ChunkEncodingTask) error {
	argumentBuilder, err := ffmpeg.CodecArgumentBuilderFactory(task.TargetCodec)
	if err != nil {
		return fmt.Errorf("worker: building argument builder for task %s: %w", task.TaskIdentifier, err)
	}

	spec := ffmpeg.ChunkEncodeSpecification{
		InputMediaFilePath:             task.InputChunkFilePath,
		OutputMediaFilePath:            task.OutputChunkFilePath,
		TargetWidthPixels:              task.TargetRung.WidthPixels,
		TargetHeightPixels:             task.TargetRung.HeightPixels,
		TargetBitrateKilobitsPerSecond: task.TargetRung.BitrateKilobitsPerSecond,
		EncodePresetName:               "medium",
	}
	commandArguments := argumentBuilder.BuildFfmpegCommandLineArguments(spec)

	progressParser := ffmpeg.NewStreamingFfmpegProgressParser(func(snapshot ffmpeg.EncodingProgressSnapshot) {
		if processor.onProgressUpdated != nil {
			processor.onProgressUpdated(task, snapshot)
		}
	})

	processor.logger.Debug("starting ffmpeg subprocess for chunk task",
		"taskIdentifier", task.TaskIdentifier,
		"codec", task.TargetCodec.String(),
		"rendition", task.TargetRung.RenditionLabel,
	)

	result := processor.subprocessRunner.RunSubprocess(
		ctx,
		processor.ffmpegBinaryPath,
		commandArguments,
		nil, // ffmpeg writes progress/log info to stderr, not stdout, in our invocation convention.
		func(line string) { progressParser.ConsumeLine(line) },
	)

	if !result.Succeeded() {
		return fmt.Errorf("worker: ffmpeg failed for task %s (exitCode=%d): %w; stderr tail follows:\n%s",
			task.TaskIdentifier, result.ExitCode, result.ExecutionError, tailOfString(result.StandardErrorOutput, 2000))
	}

	processor.logger.Debug("completed ffmpeg subprocess for chunk task", "taskIdentifier", task.TaskIdentifier)
	return nil
}

// tailOfString returns at most the last maxLength characters of s,
// used to keep error messages bounded when embedding a subprocess's
// accumulated stderr output.
func tailOfString(s string, maxLength int) string {
	if len(s) <= maxLength {
		return s
	}
	return "..." + s[len(s)-maxLength:]
}
