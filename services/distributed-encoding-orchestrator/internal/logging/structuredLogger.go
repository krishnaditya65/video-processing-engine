// Package logging provides a thin, dependency-free structured logging
// facade built on top of the Go standard library's log/slog package.
//
// The rest of the distributed-encoding-orchestrator codebase depends only
// on the narrow StructuredEventLogger interface exported from this
// package rather than on log/slog directly, so that:
//
//  1. Unit tests can substitute a no-op or capturing logger without any
//     global state or monkey-patching.
//  2. The wall-clock/JSON-vs-text encoding decision is centralized in one
//     place (NewJSONStructuredEventLogger / NewHumanReadableStructuredEventLogger).
//
// Concurrency: log/slog's Logger type is safe for concurrent use by
// multiple goroutines, and every function in this package that returns a
// StructuredEventLogger returns a value that is likewise safe for
// concurrent use without additional synchronization.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// StructuredEventLogger is the minimal structured-logging surface consumed
// by the orchestration, worker-pool, ffmpeg-wrapper, and ladder-optimizer
// packages. It intentionally mirrors the leveled-logging-with-key/value-
// attributes shape of log/slog so that a *slog.Logger satisfies it
// directly, while still allowing fully independent fake implementations
// in tests.
//
// Concurrency/safety: implementations MUST be safe for concurrent use by
// multiple goroutines, since the WorkerPool logs from many worker
// goroutines simultaneously.
type StructuredEventLogger interface {
	// Debug logs a low-level diagnostic event with the supplied
	// alternating key/value attribute pairs (e.g. "chunkIndex", 42).
	Debug(message string, keyValueAttributePairs ...any)

	// Info logs a normal operational event.
	Info(message string, keyValueAttributePairs ...any)

	// Warn logs a recoverable but noteworthy condition, such as a retried
	// ffmpeg subprocess failure.
	Warn(message string, keyValueAttributePairs ...any)

	// Error logs a failure condition that a human operator should be able
	// to find and act on.
	Error(message string, keyValueAttributePairs ...any)

	// WithFields returns a derived StructuredEventLogger that annotates
	// every subsequent log record with the supplied alternating
	// key/value pairs in addition to its own. Implementations must not
	// mutate the receiver; WithFields returns a new, independent value so
	// that concurrent callers deriving different child loggers from the
	// same parent never race with each other.
	WithFields(keyValueAttributePairs ...any) StructuredEventLogger
}

// slogBackedStructuredEventLogger adapts a *slog.Logger to the
// StructuredEventLogger interface. It holds no mutable state beyond the
// wrapped *slog.Logger, which is itself concurrency-safe, so
// slogBackedStructuredEventLogger is safe for concurrent use.
type slogBackedStructuredEventLogger struct {
	backingLogger *slog.Logger
}

// NewJSONStructuredEventLogger constructs a StructuredEventLogger that
// emits one JSON object per log line to the supplied writer. JSON output
// is intended for production deployments where logs are shipped to a
// centralized aggregation system (e.g. an ELK/Loki stack) and must be
// machine-parseable.
//
// minimumLevel controls the lowest severity that will be emitted; pass
// slog.LevelInfo in production to suppress Debug-level chatter from the
// hot path of the worker pool.
func NewJSONStructuredEventLogger(destination io.Writer, minimumLevel slog.Level) StructuredEventLogger {
	handler := slog.NewJSONHandler(destination, &slog.HandlerOptions{Level: minimumLevel})
	return &slogBackedStructuredEventLogger{backingLogger: slog.New(handler)}
}

// NewHumanReadableStructuredEventLogger constructs a StructuredEventLogger
// that emits aligned, human-readable text lines to the supplied writer.
// This is primarily intended for local development and for the bundled
// cmd/orchestrator demo binary.
func NewHumanReadableStructuredEventLogger(destination io.Writer, minimumLevel slog.Level) StructuredEventLogger {
	handler := slog.NewTextHandler(destination, &slog.HandlerOptions{Level: minimumLevel})
	return &slogBackedStructuredEventLogger{backingLogger: slog.New(handler)}
}

// NewDiscardingStructuredEventLogger returns a StructuredEventLogger that
// silently drops every record. It is useful in unit tests that exercise
// production code paths but do not want to assert on, or clutter test
// output with, log lines.
func NewDiscardingStructuredEventLogger() StructuredEventLogger {
	return NewJSONStructuredEventLogger(io.Discard, slog.LevelError+1)
}

func (l *slogBackedStructuredEventLogger) Debug(message string, keyValueAttributePairs ...any) {
	l.backingLogger.Log(context.Background(), slog.LevelDebug, message, keyValueAttributePairs...)
}

func (l *slogBackedStructuredEventLogger) Info(message string, keyValueAttributePairs ...any) {
	l.backingLogger.Log(context.Background(), slog.LevelInfo, message, keyValueAttributePairs...)
}

func (l *slogBackedStructuredEventLogger) Warn(message string, keyValueAttributePairs ...any) {
	l.backingLogger.Log(context.Background(), slog.LevelWarn, message, keyValueAttributePairs...)
}

func (l *slogBackedStructuredEventLogger) Error(message string, keyValueAttributePairs ...any) {
	l.backingLogger.Log(context.Background(), slog.LevelError, message, keyValueAttributePairs...)
}

func (l *slogBackedStructuredEventLogger) WithFields(keyValueAttributePairs ...any) StructuredEventLogger {
	return &slogBackedStructuredEventLogger{backingLogger: l.backingLogger.With(keyValueAttributePairs...)}
}

// defaultStdoutLoggerSingleton is a package-level convenience logger used
// only by the cmd/orchestrator demo binary. It is initialized once at
// import time and is safe for concurrent use because the underlying
// *slog.Logger is safe for concurrent use.
var defaultStdoutLoggerSingleton = NewHumanReadableStructuredEventLogger(os.Stdout, slog.LevelInfo)

// DefaultStdoutLogger returns the shared, process-wide human-readable
// StructuredEventLogger writing to os.Stdout. It is a convenience for
// callers (mainly cmd/orchestrator) that do not need a dedicated logger
// instance.
func DefaultStdoutLogger() StructuredEventLogger {
	return defaultStdoutLoggerSingleton
}
