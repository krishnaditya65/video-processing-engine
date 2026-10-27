// This file parses ffmpeg's machine-readable progress output, produced
// when ffmpeg is invoked with "-progress <target>" (this package's
// argument builders always pass "-progress pipe:2", i.e. progress lines
// are interleaved on stderr). The format is a flat stream of "key=value"
// lines, with a line "progress=continue" or "progress=end" terminating
// each reporting period. See ffmpeg(1) documentation for "-progress".
package ffmpeg

import (
	"strconv"
	"strings"
)

// EncodingProgressSnapshot is one parsed reporting period from ffmpeg's
// "-progress" output stream, capturing the fields this orchestrator cares
// about for ETA estimation and dead-process detection.
type EncodingProgressSnapshot struct {
	// OutputTimeMicroseconds is ffmpeg's "out_time_us" field: how much
	// output media has been written so far, in microseconds of media
	// duration (not wall-clock time).
	OutputTimeMicroseconds int64
	// TotalSizeBytes is ffmpeg's "total_size" field: bytes written to
	// the output so far.
	TotalSizeBytes int64
	// EncodedFrameCount is ffmpeg's "frame" field.
	EncodedFrameCount int64
	// EncodingSpeedMultiplier is ffmpeg's "speed" field parsed as a
	// float, e.g. 2.5 for "2.5x" (the trailing "x" is stripped), 0 if
	// unparseable/absent.
	EncodingSpeedMultiplier float64
	// IsFinalSnapshotForProcess is true when this snapshot was produced
	// by a period reporting progress=end, indicating ffmpeg believes
	// encoding has completed.
	IsFinalSnapshotForProcess bool
}

// StreamingFfmpegProgressParser incrementally consumes individual lines
// of ffmpeg's "-progress" key=value output (as delivered line-by-line by
// SubprocessRunner's onStandardErrorLine callback) and assembles them
// into EncodingProgressSnapshot values, invoking a callback once per
// completed reporting period.
//
// Concurrency/safety: NOT safe for concurrent use from multiple
// goroutines simultaneously — it is meant to be fed sequentially from a
// single SubprocessRunner output-streaming goroutine, matching how
// SubprocessRunner delivers each stream via one dedicated goroutine. Wrap
// externally with a mutex if multiple producers must feed the same
// parser instance.
type StreamingFfmpegProgressParser struct {
	pendingFields    map[string]string
	onSnapshotParsed func(EncodingProgressSnapshot)
}

// NewStreamingFfmpegProgressParser constructs a parser that invokes
// onSnapshotParsed once for every complete "progress=continue"/
// "progress=end" reporting period observed via ConsumeLine.
// onSnapshotParsed may be nil, in which case parsed snapshots are simply
// discarded (useful if a caller only wants the final accumulated state
// via a different mechanism).
func NewStreamingFfmpegProgressParser(onSnapshotParsed func(EncodingProgressSnapshot)) *StreamingFfmpegProgressParser {
	return &StreamingFfmpegProgressParser{
		pendingFields:    make(map[string]string),
		onSnapshotParsed: onSnapshotParsed,
	}
}

// ConsumeLine feeds one line of raw ffmpeg stderr output to the parser.
// Lines that are not well-formed "key=value" progress fields (e.g.
// ffmpeg's ordinary human-readable log banners) are silently ignored,
// since this orchestrator only cares about the structured progress
// fields and must not fail on the substantial volume of non-progress
// text ffmpeg also writes to stderr.
func (parser *StreamingFfmpegProgressParser) ConsumeLine(rawLine string) {
	trimmedLine := strings.TrimSpace(rawLine)
	if trimmedLine == "" {
		return
	}

	key, value, found := strings.Cut(trimmedLine, "=")
	if !found {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	parser.pendingFields[key] = value

	if key == "progress" {
		snapshot := parser.buildSnapshotFromPendingFields(value == "end")
		parser.pendingFields = make(map[string]string)
		if parser.onSnapshotParsed != nil {
			parser.onSnapshotParsed(snapshot)
		}
	}
}

// buildSnapshotFromPendingFields converts the accumulated key=value
// fields for one reporting period into an EncodingProgressSnapshot.
// Fields that fail to parse as the expected numeric type are left at
// their zero value rather than causing an error, since ffmpeg
// occasionally reports "N/A" for fields that are not yet meaningful
// (e.g. early in encoding).
func (parser *StreamingFfmpegProgressParser) buildSnapshotFromPendingFields(isFinal bool) EncodingProgressSnapshot {
	snapshot := EncodingProgressSnapshot{IsFinalSnapshotForProcess: isFinal}

	if v, ok := parser.pendingFields["out_time_us"]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			snapshot.OutputTimeMicroseconds = parsed
		}
	}
	if v, ok := parser.pendingFields["total_size"]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			snapshot.TotalSizeBytes = parsed
		}
	}
	if v, ok := parser.pendingFields["frame"]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			snapshot.EncodedFrameCount = parsed
		}
	}
	if v, ok := parser.pendingFields["speed"]; ok {
		trimmed := strings.TrimSuffix(strings.TrimSpace(v), "x")
		if parsed, err := strconv.ParseFloat(trimmed, 64); err == nil {
			snapshot.EncodingSpeedMultiplier = parsed
		}
	}

	return snapshot
}
