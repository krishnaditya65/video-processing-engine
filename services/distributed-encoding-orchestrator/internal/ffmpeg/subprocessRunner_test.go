package ffmpeg

import (
	"context"
	"testing"
)

// TestFakeSubprocessRunner_ReplaysScriptedOutputLines verifies the fake
// delivers scripted stdout/stderr lines to the supplied callbacks in
// order and accumulates stderr into the result.
func TestFakeSubprocessRunner_ReplaysScriptedOutputLines(t *testing.T) {
	fake := &FakeSubprocessRunner{
		ScriptedStandardOutputLines: []string{"stdout-1", "stdout-2"},
		ScriptedStandardErrorLines:  []string{"frame=1", "progress=end"},
		ScriptedExitCode:            0,
	}

	var stdoutLines, stderrLines []string
	result := fake.RunSubprocess(context.Background(), "ffmpeg", []string{"-y"},
		func(line string) { stdoutLines = append(stdoutLines, line) },
		func(line string) { stderrLines = append(stderrLines, line) },
	)

	if !result.Succeeded() {
		t.Fatalf("expected success, got %+v", result)
	}
	if len(stdoutLines) != 2 || stdoutLines[0] != "stdout-1" || stdoutLines[1] != "stdout-2" {
		t.Errorf("unexpected stdout lines: %v", stdoutLines)
	}
	if len(stderrLines) != 2 {
		t.Errorf("unexpected stderr lines: %v", stderrLines)
	}
	if result.StandardErrorOutput != "frame=1\nprogress=end\n" {
		t.Errorf("unexpected accumulated stderr: %q", result.StandardErrorOutput)
	}
}

// TestFakeSubprocessRunner_RecordsInvocationsForAssertion verifies the
// fake records each call's command name/arguments for test assertions
// (used by worker-pool tests to verify correct dispatch fan-out).
func TestFakeSubprocessRunner_RecordsInvocationsForAssertion(t *testing.T) {
	fake := &FakeSubprocessRunner{}

	fake.RunSubprocess(context.Background(), "ffmpeg", []string{"-i", "a.mp4"}, nil, nil)
	fake.RunSubprocess(context.Background(), "ffmpeg", []string{"-i", "b.mp4"}, nil, nil)

	if fake.InvocationCount() != 2 {
		t.Fatalf("expected 2 invocations, got %d", fake.InvocationCount())
	}
	recorded := fake.RecordedInvocations()
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded invocations, got %d", len(recorded))
	}
	if recorded[0].CommandArguments[1] != "a.mp4" || recorded[1].CommandArguments[1] != "b.mp4" {
		t.Errorf("unexpected recorded arguments: %+v", recorded)
	}
}

// TestFakeSubprocessRunner_FailFirstNInvocationsThenSucceeds verifies the
// scripted-flakiness mechanism used to test Dispatcher/WorkerPool retry
// behavior: the first N invocations fail, subsequent ones succeed.
func TestFakeSubprocessRunner_FailFirstNInvocationsThenSucceeds(t *testing.T) {
	fake := &FakeSubprocessRunner{FailFirstNInvocations: 2}

	r1 := fake.RunSubprocess(context.Background(), "ffmpeg", nil, nil, nil)
	r2 := fake.RunSubprocess(context.Background(), "ffmpeg", nil, nil, nil)
	r3 := fake.RunSubprocess(context.Background(), "ffmpeg", nil, nil, nil)

	if r1.Succeeded() || r2.Succeeded() {
		t.Fatalf("expected first 2 invocations to fail, got r1=%+v r2=%+v", r1, r2)
	}
	if !r3.Succeeded() {
		t.Fatalf("expected 3rd invocation to succeed, got %+v", r3)
	}
}

// TestFakeSubprocessRunner_RespectsAlreadyCancelledContext verifies the
// fake honors context cancellation like the real runner must.
func TestFakeSubprocessRunner_RespectsAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := &FakeSubprocessRunner{}
	result := fake.RunSubprocess(ctx, "ffmpeg", nil, nil, nil)
	if result.Succeeded() {
		t.Fatal("expected failure for already-cancelled context")
	}
	if result.ExecutionError == nil {
		t.Fatal("expected non-nil ExecutionError for cancelled context")
	}
}

// TestRealOsExecSubprocessRunner_RunsTrueAndFalse exercises the real
// os/exec-backed implementation against the near-universally-available
// "true" and "false" POSIX utilities (not ffmpeg — this test only
// verifies our process-management plumbing, per the task's explicit
// allowance that ffmpeg itself need not be installed in this sandbox).
// It is skipped automatically on platforms where these binaries are
// unavailable in PATH.
func TestRealOsExecSubprocessRunner_RunsTrueAndFalse(t *testing.T) {
	runner := NewRealOsExecSubprocessRunner()

	trueResult := runner.RunSubprocess(context.Background(), "true", nil, nil, nil)
	if trueResult.ExecutionError != nil && isExecutableNotFoundError(trueResult.ExecutionError) {
		t.Skip("'true' binary not available in PATH on this platform")
	}
	if !trueResult.Succeeded() {
		t.Fatalf("expected 'true' to succeed, got %+v", trueResult)
	}

	falseResult := runner.RunSubprocess(context.Background(), "false", nil, nil, nil)
	if falseResult.Succeeded() {
		t.Fatalf("expected 'false' to report failure, got %+v", falseResult)
	}
	if falseResult.ExitCode != 1 {
		t.Errorf("expected exit code 1 from 'false', got %d", falseResult.ExitCode)
	}
}

// TestRealOsExecSubprocessRunner_StreamsStdoutLines verifies real stdout
// streaming plumbing using the "echo" utility.
func TestRealOsExecSubprocessRunner_StreamsStdoutLines(t *testing.T) {
	runner := NewRealOsExecSubprocessRunner()

	var lines []string
	result := runner.RunSubprocess(context.Background(), "echo", []string{"hello-orchestrator"}, func(line string) {
		lines = append(lines, line)
	}, nil)

	if result.ExecutionError != nil && isExecutableNotFoundError(result.ExecutionError) {
		t.Skip("'echo' binary not available in PATH on this platform")
	}
	if !result.Succeeded() {
		t.Fatalf("expected echo to succeed, got %+v", result)
	}
	if len(lines) != 1 || lines[0] != "hello-orchestrator" {
		t.Fatalf("expected to capture stdout line 'hello-orchestrator', got %v", lines)
	}
}

// TestRealOsExecSubprocessRunner_ContextCancellationTerminatesProcess
// verifies that cancelling ctx while a subprocess is running causes
// RunSubprocess to return promptly with a non-nil ExecutionError, using
// "sleep 30" as a long-running process to cancel.
func TestRealOsExecSubprocessRunner_ContextCancellationTerminatesProcess(t *testing.T) {
	runner := NewRealOsExecSubprocessRunner()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan SubprocessExecutionResult, 1)
	go func() {
		done <- runner.RunSubprocess(ctx, "sleep", []string{"30"}, nil, nil)
	}()

	cancel()

	result := <-done
	if result.ExecutionError != nil && isExecutableNotFoundError(result.ExecutionError) {
		t.Skip("'sleep' binary not available in PATH on this platform")
	}
	if result.ExecutionError == nil {
		t.Fatal("expected non-nil ExecutionError after context cancellation")
	}
}

// isExecutableNotFoundError is a best-effort heuristic used only to make
// the small number of tests that shell out to real POSIX utilities
// gracefully skip (rather than fail) on unusual platforms/sandboxes
// where even "true"/"echo"/"sleep" are not on PATH.
func isExecutableNotFoundError(err error) bool {
	return err != nil && (contains(err.Error(), "executable file not found") || contains(err.Error(), "no such file"))
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
