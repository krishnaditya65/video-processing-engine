package ffmpeg

import (
	"testing"
	"time"
)

// TestFfmpegFailureCircuitBreaker_StaysClosedBelowThreshold verifies the
// breaker remains Closed and keeps allowing calls while consecutive
// failures stay below the configured threshold.
func TestFfmpegFailureCircuitBreaker_StaysClosedBelowThreshold(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(3, time.Minute)

	breaker.RecordFailure()
	breaker.RecordFailure()

	if breaker.CurrentState() != CircuitBreakerClosed {
		t.Fatalf("expected Closed after 2/3 failures, got %v", breaker.CurrentState())
	}
	if !breaker.AllowCall() {
		t.Fatal("expected AllowCall()==true while breaker is Closed")
	}
}

// TestFfmpegFailureCircuitBreaker_TripsOpenAtThreshold verifies the
// breaker trips to Open exactly upon reaching the configured threshold.
func TestFfmpegFailureCircuitBreaker_TripsOpenAtThreshold(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(3, time.Minute)

	breaker.RecordFailure()
	breaker.RecordFailure()
	breaker.RecordFailure()

	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected Open after 3/3 failures, got %v", breaker.CurrentState())
	}
	if breaker.AllowCall() {
		t.Fatal("expected AllowCall()==false while breaker is Open")
	}
}

// TestFfmpegFailureCircuitBreaker_SuccessResetsFailureCounter verifies
// that a success before hitting the threshold resets the consecutive
// counter, so subsequent failures must again accumulate from zero.
func TestFfmpegFailureCircuitBreaker_SuccessResetsFailureCounter(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(3, time.Minute)

	breaker.RecordFailure()
	breaker.RecordFailure()
	breaker.RecordSuccess()
	breaker.RecordFailure()
	breaker.RecordFailure()

	if breaker.CurrentState() != CircuitBreakerClosed {
		t.Fatalf("expected Closed (counter reset by success), got %v", breaker.CurrentState())
	}
}

// TestFfmpegFailureCircuitBreaker_TransitionsToHalfOpenAfterCooldown
// verifies the breaker moves from Open to HalfOpen once the cooldown
// elapses, using an injected clock to avoid a real sleep in the test.
func TestFfmpegFailureCircuitBreaker_TransitionsToHalfOpenAfterCooldown(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(1, 10*time.Second)

	fakeNow := time.Now()
	breaker.nowFunc = func() time.Time { return fakeNow }

	breaker.RecordFailure() // trips open at fakeNow
	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected Open immediately after tripping, got %v", breaker.CurrentState())
	}

	fakeNow = fakeNow.Add(5 * time.Second)
	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected still Open before cooldown elapses, got %v", breaker.CurrentState())
	}

	fakeNow = fakeNow.Add(6 * time.Second) // total 11s elapsed > 10s cooldown
	if breaker.CurrentState() != CircuitBreakerHalfOpen {
		t.Fatalf("expected HalfOpen after cooldown elapses, got %v", breaker.CurrentState())
	}
	if !breaker.AllowCall() {
		t.Fatal("expected AllowCall()==true in HalfOpen (probe call allowed)")
	}
}

// TestFfmpegFailureCircuitBreaker_HalfOpenSuccessCloses verifies a
// successful probe call closes the breaker.
func TestFfmpegFailureCircuitBreaker_HalfOpenSuccessCloses(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(1, time.Millisecond)

	fakeNow := time.Now()
	breaker.nowFunc = func() time.Time { return fakeNow }
	breaker.RecordFailure() // trips open

	fakeNow = fakeNow.Add(time.Second) // well past 1ms cooldown
	if breaker.CurrentState() != CircuitBreakerHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", breaker.CurrentState())
	}

	breaker.RecordSuccess()
	if breaker.CurrentState() != CircuitBreakerClosed {
		t.Fatalf("expected Closed after successful probe, got %v", breaker.CurrentState())
	}
}

// TestFfmpegFailureCircuitBreaker_HalfOpenFailureReopensAndRestartsCooldown
// verifies a failed probe call re-opens the breaker and restarts the
// cooldown window (rather than, say, immediately going back to HalfOpen).
func TestFfmpegFailureCircuitBreaker_HalfOpenFailureReopensAndRestartsCooldown(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(1, 10*time.Second)

	fakeNow := time.Now()
	breaker.nowFunc = func() time.Time { return fakeNow }
	breaker.RecordFailure() // trips open at t=0

	fakeNow = fakeNow.Add(11 * time.Second) // now HalfOpen
	if breaker.CurrentState() != CircuitBreakerHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", breaker.CurrentState())
	}

	breaker.RecordFailure() // failed probe -> re-open, cooldown restarts at t=11s
	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected Open after failed probe, got %v", breaker.CurrentState())
	}

	fakeNow = fakeNow.Add(5 * time.Second) // t=16s, only 5s since re-open, cooldown is 10s
	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected still Open (cooldown restarted), got %v", breaker.CurrentState())
	}

	fakeNow = fakeNow.Add(6 * time.Second) // t=22s, 11s since re-open at t=11s
	if breaker.CurrentState() != CircuitBreakerHalfOpen {
		t.Fatalf("expected HalfOpen again after restarted cooldown elapses, got %v", breaker.CurrentState())
	}
}

// TestFfmpegFailureCircuitBreaker_ThresholdClampedToMinimumOne verifies
// the documented clamp of consecutiveFailureThreshold < 1 to 1.
func TestFfmpegFailureCircuitBreaker_ThresholdClampedToMinimumOne(t *testing.T) {
	breaker := NewFfmpegFailureCircuitBreaker(0, time.Minute)
	breaker.RecordFailure()
	if breaker.CurrentState() != CircuitBreakerOpen {
		t.Fatalf("expected threshold clamped to 1 (open after 1 failure), got %v", breaker.CurrentState())
	}
}

// TestCircuitBreakerState_StringRendersKnownLabels verifies String().
func TestCircuitBreakerState_StringRendersKnownLabels(t *testing.T) {
	if CircuitBreakerClosed.String() != "closed" {
		t.Errorf("got %q", CircuitBreakerClosed.String())
	}
	if CircuitBreakerOpen.String() != "open" {
		t.Errorf("got %q", CircuitBreakerOpen.String())
	}
	if CircuitBreakerHalfOpen.String() != "half-open" {
		t.Errorf("got %q", CircuitBreakerHalfOpen.String())
	}
	if CircuitBreakerState(999).String() != "unknown" {
		t.Errorf("got %q", CircuitBreakerState(999).String())
	}
}
