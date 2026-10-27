// This file implements the Circuit Breaker design pattern guarding
// against a worker goroutine hammering a persistently broken ffmpeg
// invocation path (e.g. a corrupt input asset, a missing codec library on
// one fleet node, or disk exhaustion) with an unbounded stream of retries
// that would otherwise waste CPU/scheduling slots that could go to
// healthy work.
package ffmpeg

import (
	"errors"
	"sync"
	"time"
)

// CircuitBreakerState is the classic three-state circuit breaker state
// machine: Closed (normal operation, calls pass through), Open (calls
// are rejected immediately without attempting the underlying operation),
// and HalfOpen (a single trial call is allowed through to test whether
// the underlying dependency has recovered).
type CircuitBreakerState int

const (
	// CircuitBreakerClosed is the normal operating state: calls are
	// allowed through and failures are counted.
	CircuitBreakerClosed CircuitBreakerState = iota
	// CircuitBreakerOpen rejects all calls immediately until
	// openStateCooldownDuration has elapsed since the breaker tripped.
	CircuitBreakerOpen
	// CircuitBreakerHalfOpen allows exactly one trial call through to
	// probe whether the dependency has recovered.
	CircuitBreakerHalfOpen
)

// String renders a CircuitBreakerState as a short label for logs.
func (state CircuitBreakerState) String() string {
	switch state {
	case CircuitBreakerClosed:
		return "closed"
	case CircuitBreakerOpen:
		return "open"
	case CircuitBreakerHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitBreakerOpen is returned by
// FfmpegFailureCircuitBreaker.AttemptCall when the breaker is currently
// open and rejecting calls.
var ErrCircuitBreakerOpen = errors.New("ffmpeg: circuit breaker is open; refusing to attempt subprocess call")

// FfmpegFailureCircuitBreaker protects a single worker's repeated ffmpeg
// subprocess invocations from cascading failure: once
// consecutiveFailureThreshold consecutive failures have been observed,
// the breaker trips to CircuitBreakerOpen and rejects further attempts
// for openStateCooldownDuration, after which it transitions to
// CircuitBreakerHalfOpen to allow one probe call through. A successful
// probe (or any successful call while Closed) resets the consecutive
// failure counter to zero and (if HalfOpen) closes the breaker; a failed
// probe re-opens it and restarts the cooldown.
//
// This mirrors the standard Circuit Breaker pattern (as popularized by
// Netflix's Hystrix library) applied at the granularity of "one breaker
// per WorkerPool worker goroutine", so that one worker stuck against a
// bad node/disk does not retry-storm while its siblings continue
// processing healthy tasks from the shared queue.
//
// Concurrency/safety: safe for concurrent use by multiple goroutines; all
// state transitions are guarded by an internal mutex. In this
// orchestrator's normal usage each WorkerPool worker owns its own
// breaker instance (no sharing needed), but the type itself does not
// assume single-goroutine ownership.
type FfmpegFailureCircuitBreaker struct {
	mutex sync.Mutex

	consecutiveFailureThreshold int
	openStateCooldownDuration   time.Duration

	currentState            CircuitBreakerState
	consecutiveFailureCount int
	openedAtTime            time.Time
	nowFunc                 func() time.Time
}

// NewFfmpegFailureCircuitBreaker constructs a circuit breaker that trips
// to Open after consecutiveFailureThreshold consecutive failed calls and
// stays Open for openStateCooldownDuration before probing recovery via
// HalfOpen.
//
// Params:
//   - consecutiveFailureThreshold: must be >= 1; a value of 1 means "trip
//     immediately on the first failure" (rarely desirable — production
//     callers typically use 3-5).
//   - openStateCooldownDuration: how long to reject calls after tripping
//     before allowing a HalfOpen probe attempt.
func NewFfmpegFailureCircuitBreaker(consecutiveFailureThreshold int, openStateCooldownDuration time.Duration) *FfmpegFailureCircuitBreaker {
	if consecutiveFailureThreshold < 1 {
		consecutiveFailureThreshold = 1
	}
	return &FfmpegFailureCircuitBreaker{
		consecutiveFailureThreshold: consecutiveFailureThreshold,
		openStateCooldownDuration:   openStateCooldownDuration,
		currentState:                CircuitBreakerClosed,
		nowFunc:                     time.Now,
	}
}

// CurrentState returns the breaker's current CircuitBreakerState,
// resolving an expired Open cooldown into HalfOpen as a side effect (the
// same lazy-transition approach used by AttemptCall) so callers observing
// state get an up-to-date answer without a background timer goroutine.
func (breaker *FfmpegFailureCircuitBreaker) CurrentState() CircuitBreakerState {
	breaker.mutex.Lock()
	defer breaker.mutex.Unlock()
	breaker.transitionOutOfExpiredOpenStateLocked()
	return breaker.currentState
}

// transitionOutOfExpiredOpenStateLocked moves the breaker from Open to
// HalfOpen once openStateCooldownDuration has elapsed since it tripped.
// Callers must hold breaker.mutex.
func (breaker *FfmpegFailureCircuitBreaker) transitionOutOfExpiredOpenStateLocked() {
	if breaker.currentState == CircuitBreakerOpen && breaker.nowFunc().Sub(breaker.openedAtTime) >= breaker.openStateCooldownDuration {
		breaker.currentState = CircuitBreakerHalfOpen
	}
}

// AllowCall reports whether a call should currently be attempted through
// this breaker, lazily resolving an expired cooldown into HalfOpen first.
// It does NOT by itself consume the HalfOpen "single trial" slot —
// callers must report the outcome via RecordSuccess/RecordFailure exactly
// once per attempt they make after AllowCall returns true.
func (breaker *FfmpegFailureCircuitBreaker) AllowCall() bool {
	breaker.mutex.Lock()
	defer breaker.mutex.Unlock()
	breaker.transitionOutOfExpiredOpenStateLocked()
	return breaker.currentState != CircuitBreakerOpen
}

// RecordSuccess reports that an attempt allowed through by AllowCall
// succeeded. Resets the consecutive failure counter to zero and, if the
// breaker was HalfOpen, closes it.
func (breaker *FfmpegFailureCircuitBreaker) RecordSuccess() {
	breaker.mutex.Lock()
	defer breaker.mutex.Unlock()
	breaker.consecutiveFailureCount = 0
	breaker.currentState = CircuitBreakerClosed
}

// RecordFailure reports that an attempt allowed through by AllowCall
// failed. Increments the consecutive failure counter; if the breaker was
// HalfOpen, the single failed probe immediately re-opens it (restarting
// the cooldown) regardless of consecutiveFailureThreshold. If the breaker
// was Closed, it trips to Open once consecutiveFailureThreshold
// consecutive failures have accumulated.
func (breaker *FfmpegFailureCircuitBreaker) RecordFailure() {
	breaker.mutex.Lock()
	defer breaker.mutex.Unlock()

	breaker.consecutiveFailureCount++

	if breaker.currentState == CircuitBreakerHalfOpen {
		breaker.tripOpenLocked()
		return
	}
	if breaker.consecutiveFailureCount >= breaker.consecutiveFailureThreshold {
		breaker.tripOpenLocked()
	}
}

// tripOpenLocked transitions the breaker into CircuitBreakerOpen and
// records the trip time used to compute cooldown expiry. Callers must
// hold breaker.mutex.
func (breaker *FfmpegFailureCircuitBreaker) tripOpenLocked() {
	breaker.currentState = CircuitBreakerOpen
	breaker.openedAtTime = breaker.nowFunc()
}
