// Package ffmpeg provides everything needed to invoke, monitor, and
// recover from failures of the ffmpeg subprocess that performs the
// actual video encoding for a ChunkEncodingTask, WITHOUT requiring an
// actual ffmpeg binary to be present in order to unit test the
// command-construction and process-management logic.
//
// The package is organized around three concerns, each independently
// testable:
//
//  1. Argument construction (codecArgumentBuilder.go): the Strategy
//     pattern — CodecArgumentBuilder implementations for H.264, HEVC, and
//     AV1 build the exact ffmpeg CLI argument slice for a given encode
//     spec. Pure functions, no subprocess involved.
//  2. Process execution (this file): the SubprocessRunner interface
//     abstracts "run a command and give me its output streams" so
//     production code uses a real os/exec-backed implementation while
//     tests inject a FakeSubprocessRunner.
//  3. Progress parsing (progressParser.go) and failure handling
//     (circuitBreaker.go).
package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// SubprocessExecutionResult captures the outcome of running one ffmpeg
// subprocess to completion (or to early termination via context
// cancellation/timeout).
type SubprocessExecutionResult struct {
	// ExitCode is the process's OS exit code. Conventionally 0 means
	// success. If the process was killed by a signal or never started,
	// ExitCode is -1.
	ExitCode int

	// StandardErrorOutput accumulates everything ffmpeg wrote to stderr,
	// which is where ffmpeg emits both its human-readable log lines and
	// (when invoked with "-progress pipe:2"-style flags in some
	// configurations) progress key=value lines. This orchestrator asks
	// ffmpeg to write progress to a dedicated pipe (see
	// CodecArgumentBuilder docs), so in the default configuration this
	// field holds diagnostic/error log text useful for post-mortem
	// debugging of failures.
	StandardErrorOutput string

	// LatestProgressSnapshot is the most recent successfully parsed
	// EncodingProgressSnapshot observed on the progress stream, or nil if
	// none was ever parsed (e.g. the process failed immediately).
	LatestProgressSnapshot *EncodingProgressSnapshot

	// ExecutionError is non-nil if the process could not be started, was
	// killed due to context cancellation, or exited with a non-zero
	// status. A non-nil ExecutionError with ExitCode == 0 indicates a
	// failure to even launch the subprocess (e.g. binary not found).
	ExecutionError error
}

// Succeeded reports whether the subprocess ran to completion with a zero
// exit code and no execution error.
func (result SubprocessExecutionResult) Succeeded() bool {
	return result.ExecutionError == nil && result.ExitCode == 0
}

// SubprocessRunner abstracts "execute an external command, stream its
// stdout/stderr, honor context cancellation, and report the result" so
// that FfmpegSubprocessInvoker (the production caller) can be exercised
// in unit tests without ever spawning a real ffmpeg process.
//
// Concurrency/safety: implementations must support being called
// concurrently from many worker goroutines, each with its own
// independent context.Context and argument slice; no shared mutable
// state between calls is assumed by callers.
type SubprocessRunner interface {
	// RunSubprocess executes commandName with the given commandArguments,
	// blocking until the process exits, ctx is cancelled, or an error
	// occurs starting the process. Every line written to the child
	// process's stdout is passed to onStandardOutputLine as it arrives
	// (streaming, not buffered to completion first); every line of
	// stderr is both passed to onStandardErrorLine and accumulated into
	// the returned result's StandardErrorOutput.
	//
	// If ctx is cancelled while the subprocess is running, implementations
	// must terminate the subprocess promptly and return a
	// SubprocessExecutionResult whose ExecutionError wraps ctx.Err().
	RunSubprocess(
		ctx context.Context,
		commandName string,
		commandArguments []string,
		onStandardOutputLine func(line string),
		onStandardErrorLine func(line string),
	) SubprocessExecutionResult
}

// realOsExecSubprocessRunner is the production SubprocessRunner
// implementation, backed by os/exec.CommandContext. It holds no state and
// is safe for concurrent use — every call to RunSubprocess creates an
// independent *exec.Cmd.
type realOsExecSubprocessRunner struct{}

// NewRealOsExecSubprocessRunner constructs the production
// SubprocessRunner that actually shells out to the named binary (e.g.
// "ffmpeg") using os/exec. Safe for concurrent use; a single instance can
// (and should) be shared across all worker goroutines.
func NewRealOsExecSubprocessRunner() SubprocessRunner {
	return &realOsExecSubprocessRunner{}
}

func (r *realOsExecSubprocessRunner) RunSubprocess(
	ctx context.Context,
	commandName string,
	commandArguments []string,
	onStandardOutputLine func(line string),
	onStandardErrorLine func(line string),
) SubprocessExecutionResult {
	command := exec.CommandContext(ctx, commandName, commandArguments...)

	stdoutPipe, stdoutPipeErr := command.StdoutPipe()
	if stdoutPipeErr != nil {
		return SubprocessExecutionResult{ExitCode: -1, ExecutionError: fmt.Errorf("acquiring stdout pipe: %w", stdoutPipeErr)}
	}
	stderrPipe, stderrPipeErr := command.StderrPipe()
	if stderrPipeErr != nil {
		return SubprocessExecutionResult{ExitCode: -1, ExecutionError: fmt.Errorf("acquiring stderr pipe: %w", stderrPipeErr)}
	}

	if startErr := command.Start(); startErr != nil {
		return SubprocessExecutionResult{ExitCode: -1, ExecutionError: fmt.Errorf("starting subprocess %q: %w", commandName, startErr)}
	}

	var stderrAccumulator []byte
	var streamWaitGroup sync.WaitGroup
	streamWaitGroup.Add(2)

	go func() {
		defer streamWaitGroup.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if onStandardOutputLine != nil {
				onStandardOutputLine(scanner.Text())
			}
		}
	}()

	go func() {
		defer streamWaitGroup.Done()
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stderrAccumulator = append(stderrAccumulator, line...)
			stderrAccumulator = append(stderrAccumulator, '\n')
			if onStandardErrorLine != nil {
				onStandardErrorLine(line)
			}
		}
	}()

	streamWaitGroup.Wait()
	waitErr := command.Wait()

	exitCode := 0
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}

	var executionError error
	if ctx.Err() != nil {
		executionError = fmt.Errorf("subprocess %q cancelled: %w", commandName, ctx.Err())
	} else if waitErr != nil {
		executionError = fmt.Errorf("subprocess %q failed: %w", commandName, waitErr)
	}

	return SubprocessExecutionResult{
		ExitCode:            exitCode,
		StandardErrorOutput: string(stderrAccumulator),
		ExecutionError:      executionError,
	}
}

// FakeSubprocessRunner is a fully in-memory, deterministic
// SubprocessRunner implementation for unit tests. It never spawns a real
// process; instead it replays a scripted sequence of stdout/stderr lines
// and a final exit code/error supplied by the test, and records every
// invocation it receives for later assertion.
//
// Concurrency/safety: guarded by an internal mutex, so it is safe to share
// a single FakeSubprocessRunner across concurrently-running worker
// goroutines in a test, e.g. to verify WorkerPool fan-out behavior.
type FakeSubprocessRunner struct {
	mutex sync.Mutex

	// ScriptedStandardOutputLines are replayed, in order, to
	// onStandardOutputLine on every call to RunSubprocess.
	ScriptedStandardOutputLines []string
	// ScriptedStandardErrorLines are replayed, in order, to
	// onStandardErrorLine (and accumulated into StandardErrorOutput) on
	// every call.
	ScriptedStandardErrorLines []string
	// ScriptedExitCode is returned as SubprocessExecutionResult.ExitCode.
	ScriptedExitCode int
	// ScriptedExecutionError is returned as
	// SubprocessExecutionResult.ExecutionError.
	ScriptedExecutionError error
	// FailFirstNInvocations, if > 0, causes the first N calls to
	// RunSubprocess to return ScriptedExecutionError/a non-zero exit
	// code regardless of the other Scripted* fields, then subsequent
	// calls succeed. This is used to test the Dispatcher's retry/backoff
	// and the WorkerPool's circuit breaker without needing a real
	// flaky process.
	FailFirstNInvocations int

	recordedInvocations []RecordedSubprocessInvocation
	invocationCount     int
}

// RecordedSubprocessInvocation is one call captured by FakeSubprocessRunner.
type RecordedSubprocessInvocation struct {
	CommandName      string
	CommandArguments []string
}

// RunSubprocess implements SubprocessRunner by replaying scripted output
// rather than executing a real process. See FakeSubprocessRunner's doc
// comment for the scripting fields.
func (f *FakeSubprocessRunner) RunSubprocess(
	ctx context.Context,
	commandName string,
	commandArguments []string,
	onStandardOutputLine func(line string),
	onStandardErrorLine func(line string),
) SubprocessExecutionResult {
	f.mutex.Lock()
	f.invocationCount++
	currentInvocationNumber := f.invocationCount
	f.recordedInvocations = append(f.recordedInvocations, RecordedSubprocessInvocation{
		CommandName:      commandName,
		CommandArguments: append([]string(nil), commandArguments...),
	})
	f.mutex.Unlock()

	if ctx.Err() != nil {
		return SubprocessExecutionResult{ExitCode: -1, ExecutionError: ctx.Err()}
	}

	for _, line := range f.ScriptedStandardOutputLines {
		if onStandardOutputLine != nil {
			onStandardOutputLine(line)
		}
	}
	var stderrAccumulator string
	for _, line := range f.ScriptedStandardErrorLines {
		stderrAccumulator += line + "\n"
		if onStandardErrorLine != nil {
			onStandardErrorLine(line)
		}
	}

	if currentInvocationNumber <= f.FailFirstNInvocations {
		exitCode := f.ScriptedExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		executionError := f.ScriptedExecutionError
		if executionError == nil {
			executionError = fmt.Errorf("fake subprocess scripted failure on invocation %d", currentInvocationNumber)
		}
		return SubprocessExecutionResult{ExitCode: exitCode, StandardErrorOutput: stderrAccumulator, ExecutionError: executionError}
	}

	return SubprocessExecutionResult{
		ExitCode:            f.ScriptedExitCode,
		StandardErrorOutput: stderrAccumulator,
		ExecutionError:      nil,
	}
}

// InvocationCount returns how many times RunSubprocess has been called so
// far. Safe for concurrent use.
func (f *FakeSubprocessRunner) InvocationCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.invocationCount
}

// RecordedInvocations returns a copy of every invocation recorded so far,
// in call order. Safe for concurrent use.
func (f *FakeSubprocessRunner) RecordedInvocations() []RecordedSubprocessInvocation {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	result := make([]RecordedSubprocessInvocation, len(f.recordedInvocations))
	copy(result, f.recordedInvocations)
	return result
}

var _ io.Writer = (*discardWriter)(nil)

// discardWriter is an io.Writer sink used internally when a caller does
// not care about a particular output stream; kept as a tiny local type so
// this package does not need to import io/ioutil's deprecated surface.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
