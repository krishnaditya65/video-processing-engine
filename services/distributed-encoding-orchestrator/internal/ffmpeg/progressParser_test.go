package ffmpeg

import "testing"

// TestStreamingFfmpegProgressParser_ParsesCompleteReportingPeriod feeds a
// realistic ffmpeg "-progress" reporting period (as separate lines, the
// way SubprocessRunner delivers them) and verifies every numeric field is
// parsed correctly.
func TestStreamingFfmpegProgressParser_ParsesCompleteReportingPeriod(t *testing.T) {
	var snapshots []EncodingProgressSnapshot
	parser := NewStreamingFfmpegProgressParser(func(s EncodingProgressSnapshot) {
		snapshots = append(snapshots, s)
	})

	lines := []string{
		"frame=120",
		"fps=30.00",
		"out_time_us=4000000",
		"total_size=1048576",
		"speed=2.5x",
		"progress=continue",
	}
	for _, line := range lines {
		parser.ConsumeLine(line)
	}

	if len(snapshots) != 1 {
		t.Fatalf("expected exactly 1 snapshot after one reporting period, got %d", len(snapshots))
	}
	snap := snapshots[0]
	if snap.EncodedFrameCount != 120 {
		t.Errorf("expected frame=120, got %d", snap.EncodedFrameCount)
	}
	if snap.OutputTimeMicroseconds != 4000000 {
		t.Errorf("expected out_time_us=4000000, got %d", snap.OutputTimeMicroseconds)
	}
	if snap.TotalSizeBytes != 1048576 {
		t.Errorf("expected total_size=1048576, got %d", snap.TotalSizeBytes)
	}
	if snap.EncodingSpeedMultiplier != 2.5 {
		t.Errorf("expected speed=2.5, got %f", snap.EncodingSpeedMultiplier)
	}
	if snap.IsFinalSnapshotForProcess {
		t.Error("expected IsFinalSnapshotForProcess=false for progress=continue")
	}
}

// TestStreamingFfmpegProgressParser_ProgressEndMarksFinalSnapshot verifies
// that a "progress=end" line correctly sets IsFinalSnapshotForProcess.
func TestStreamingFfmpegProgressParser_ProgressEndMarksFinalSnapshot(t *testing.T) {
	var snapshots []EncodingProgressSnapshot
	parser := NewStreamingFfmpegProgressParser(func(s EncodingProgressSnapshot) {
		snapshots = append(snapshots, s)
	})

	parser.ConsumeLine("frame=300")
	parser.ConsumeLine("progress=end")

	if len(snapshots) != 1 || !snapshots[0].IsFinalSnapshotForProcess {
		t.Fatalf("expected final snapshot with IsFinalSnapshotForProcess=true, got %v", snapshots)
	}
}

// TestStreamingFfmpegProgressParser_MultiplePeriodsProduceMultipleSnapshots
// verifies the parser correctly resets its pending-fields buffer between
// reporting periods rather than accumulating stale values.
func TestStreamingFfmpegProgressParser_MultiplePeriodsProduceMultipleSnapshots(t *testing.T) {
	var snapshots []EncodingProgressSnapshot
	parser := NewStreamingFfmpegProgressParser(func(s EncodingProgressSnapshot) {
		snapshots = append(snapshots, s)
	})

	parser.ConsumeLine("frame=10")
	parser.ConsumeLine("progress=continue")
	parser.ConsumeLine("frame=20")
	parser.ConsumeLine("progress=continue")

	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].EncodedFrameCount != 10 {
		t.Errorf("expected first snapshot frame=10, got %d", snapshots[0].EncodedFrameCount)
	}
	if snapshots[1].EncodedFrameCount != 20 {
		t.Errorf("expected second snapshot frame=20, got %d", snapshots[1].EncodedFrameCount)
	}
}

// TestStreamingFfmpegProgressParser_IgnoresMalformedAndBlankLines verifies
// that non key=value lines (ffmpeg's ordinary banner/log text) and blank
// lines are silently ignored rather than causing a panic or spurious
// snapshot.
func TestStreamingFfmpegProgressParser_IgnoresMalformedAndBlankLines(t *testing.T) {
	callCount := 0
	parser := NewStreamingFfmpegProgressParser(func(EncodingProgressSnapshot) { callCount++ })

	parser.ConsumeLine("")
	parser.ConsumeLine("   ")
	parser.ConsumeLine("ffmpeg version 6.0 Copyright (c) 2000-2023")
	parser.ConsumeLine("  built with clang  ")
	parser.ConsumeLine("frame=5")
	parser.ConsumeLine("progress=continue")

	if callCount != 1 {
		t.Fatalf("expected exactly 1 snapshot emitted (malformed lines ignored), got %d", callCount)
	}
}

// TestStreamingFfmpegProgressParser_NilCallbackDoesNotPanic verifies the
// documented "nil callback discards snapshots" contract.
func TestStreamingFfmpegProgressParser_NilCallbackDoesNotPanic(t *testing.T) {
	parser := NewStreamingFfmpegProgressParser(nil)
	parser.ConsumeLine("frame=1")
	parser.ConsumeLine("progress=end")
	// Reaching this line without panicking is the assertion.
}

// TestStreamingFfmpegProgressParser_UnparseableNumericFieldLeftAtZero
// verifies ffmpeg's occasional "N/A" placeholder values do not cause an
// error and simply leave the corresponding field at its zero value.
func TestStreamingFfmpegProgressParser_UnparseableNumericFieldLeftAtZero(t *testing.T) {
	var snapshot EncodingProgressSnapshot
	parser := NewStreamingFfmpegProgressParser(func(s EncodingProgressSnapshot) { snapshot = s })

	parser.ConsumeLine("out_time_us=N/A")
	parser.ConsumeLine("speed=N/A")
	parser.ConsumeLine("progress=continue")

	if snapshot.OutputTimeMicroseconds != 0 {
		t.Errorf("expected zero-value out_time_us for unparseable N/A, got %d", snapshot.OutputTimeMicroseconds)
	}
	if snapshot.EncodingSpeedMultiplier != 0 {
		t.Errorf("expected zero-value speed for unparseable N/A, got %f", snapshot.EncodingSpeedMultiplier)
	}
}
