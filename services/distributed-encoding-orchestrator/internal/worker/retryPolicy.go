// This file defines the exponential-backoff retry policy used by the
// WorkerPool/Dispatcher when a ChunkEncodingTaskProcessor attempt fails.
package worker

import (
	"math"
	"math/rand"
	"time"
)

// ExponentialBackoffRetryPolicy configures how the Dispatcher retries a
// failed ChunkEncodingTask attempt: delay grows geometrically with the
// attempt number, capped at MaxDelay, with optional random jitter to
// avoid thundering-herd retries across the fleet all backing off in
// lockstep.
//
// The delay before retry attempt N (N=1 for the first retry, i.e. after
// the first failed attempt) is:
//
//	delay(N) = min(MaxDelay, BaseDelay * Multiplier^(N-1))
//	delay(N) = delay(N) * (1 + jitterFraction), jitterFraction ~ Uniform[0, JitterFraction]
type ExponentialBackoffRetryPolicy struct {
	// MaxAttempts is the total number of attempts allowed, including the
	// initial (non-retry) attempt. MaxAttempts=3 means: try once, and if
	// it fails, retry up to 2 more times. Must be >= 1.
	MaxAttempts int
	// BaseDelay is the delay before the first retry (N=1). Must be > 0
	// for any retries to have a non-zero delay.
	BaseDelay time.Duration
	// Multiplier is the geometric growth factor applied per additional
	// retry attempt. Must be >= 1; a value of 1 degenerates to constant
	// (non-exponential) backoff.
	Multiplier float64
	// MaxDelay caps the computed delay regardless of attempt number.
	MaxDelay time.Duration
	// JitterFraction adds up to this fraction of extra random delay on
	// top of the deterministic exponential value, e.g. 0.2 adds between
	// 0% and 20% extra delay. Must be >= 0.
	JitterFraction float64
}

// DefaultExponentialBackoffRetryPolicy returns a sensible production
// default: 4 total attempts (1 initial + 3 retries), starting at 500ms,
// doubling each time, capped at 30s, with 20% jitter.
func DefaultExponentialBackoffRetryPolicy() ExponentialBackoffRetryPolicy {
	return ExponentialBackoffRetryPolicy{
		MaxAttempts:    4,
		BaseDelay:      500 * time.Millisecond,
		Multiplier:     2.0,
		MaxDelay:       30 * time.Second,
		JitterFraction: 0.2,
	}
}

// ComputeBackoffDelayForRetryAttempt returns the delay to wait before
// making retry attempt retryAttemptNumber (1-indexed: 1 is the delay
// before the first retry, i.e. the second overall attempt). Returns 0
// for retryAttemptNumber <= 0.
//
// This is a pure function of its inputs except for the jitter term,
// which is drawn from math/rand's default (global) source; math/rand's
// top-level functions are documented as safe for concurrent use by
// multiple goroutines, so this method is likewise safe to call
// concurrently.
func (policy ExponentialBackoffRetryPolicy) ComputeBackoffDelayForRetryAttempt(retryAttemptNumber int) time.Duration {
	if retryAttemptNumber <= 0 {
		return 0
	}

	multiplier := policy.Multiplier
	if multiplier < 1 {
		multiplier = 1
	}

	rawDelaySeconds := policy.BaseDelay.Seconds() * math.Pow(multiplier, float64(retryAttemptNumber-1))

	cappedDelaySeconds := rawDelaySeconds
	if policy.MaxDelay > 0 && cappedDelaySeconds > policy.MaxDelay.Seconds() {
		cappedDelaySeconds = policy.MaxDelay.Seconds()
	}

	if policy.JitterFraction > 0 {
		jitterMultiplier := 1.0 + rand.Float64()*policy.JitterFraction //nolint:gosec // non-cryptographic backoff jitter
		cappedDelaySeconds *= jitterMultiplier
	}

	return time.Duration(cappedDelaySeconds * float64(time.Second))
}

// EffectiveMaxAttempts returns policy.MaxAttempts, clamped to a minimum
// of 1, for callers that want a guaranteed-valid attempt count without
// re-implementing the clamp themselves.
func (policy ExponentialBackoffRetryPolicy) EffectiveMaxAttempts() int {
	if policy.MaxAttempts < 1 {
		return 1
	}
	return policy.MaxAttempts
}
