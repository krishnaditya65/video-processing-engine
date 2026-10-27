package circularstats

import (
	"math"
	"testing"
	"time"
)

func atClockTime(hour, minute, second int) time.Time {
	// The calendar date is deliberately varied across fixtures in some
	// tests (see TestComputeCircularMeanAndStdDevOfTimeOfDay_IgnoresCalendarDate)
	// to prove only the time-of-day component is used; here a fixed date
	// is fine since these tests only care about time-of-day.
	return time.Date(2026, 1, 15, hour, minute, second, 0, time.UTC)
}

func durationCloseTo(got, want time.Duration, tolerance time.Duration) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_IdenticalTimesGiveZeroStdDevAndR1
// is the simplest possible fixture: every observation at exactly 20:00:00
// should produce a mean of exactly 20:00:00, R = 1, and stddev ≈ 0.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_IdenticalTimesGiveZeroStdDevAndR1(t *testing.T) {
	times := []time.Time{atClockTime(20, 0, 0), atClockTime(20, 0, 0), atClockTime(20, 0, 0)}
	result := ComputeCircularMeanAndStdDevOfTimeOfDay(times)

	if !durationCloseTo(result.MeanTimeOfDay, 20*time.Hour, time.Millisecond) {
		t.Errorf("expected mean 20:00:00, got %v", result.MeanTimeOfDay)
	}
	if result.MeanResultantLength < 0.999999 {
		t.Errorf("expected R≈1, got %v", result.MeanResultantLength)
	}
	if result.CircularStdDev > time.Second {
		t.Errorf("expected ~zero stddev, got %v", result.CircularStdDev)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_MidnightWraparoundFixture is
// the critical midnight-wraparound correctness test named in this service's
// requirements. Two observations, 23:00:00 and 01:00:00, are each exactly
// one hour from midnight on opposite sides. The circular mean must land on
// midnight (00:00:00) — NOT on the naive arithmetic mean of the two
// "seconds since midnight" values, which would incorrectly land on noon
// (12:00:00), twelve hours away from both actual observations.
//
// Hand-computed expected values (see package doc for the method; verified
// independently with a standalone Go program during development):
//
//	meanTimeOfDay ≈ 00:00:00
//	R ≈ 0.965926
//	circularStdDev ≈ 60.35 minutes (each observation is 60 minutes from the mean)
func TestComputeCircularMeanAndStdDevOfTimeOfDay_MidnightWraparoundFixture(t *testing.T) {
	times := []time.Time{atClockTime(23, 0, 0), atClockTime(1, 0, 0)}
	result := ComputeCircularMeanAndStdDevOfTimeOfDay(times)

	if !durationCloseTo(result.MeanTimeOfDay, 0, time.Second) &&
		!durationCloseTo(result.MeanTimeOfDay, 24*time.Hour, time.Second) {
		t.Errorf("expected mean time-of-day at midnight (00:00:00), got %v — "+
			"a naive arithmetic mean would incorrectly produce 12:00:00", result.MeanTimeOfDay)
	}

	const expectedR = 0.965926
	if math.Abs(result.MeanResultantLength-expectedR) > 0.0001 {
		t.Errorf("expected R≈%.6f, got %.6f", expectedR, result.MeanResultantLength)
	}

	expectedStdDev := time.Duration(60.3481 * float64(time.Minute))
	if !durationCloseTo(result.CircularStdDev, expectedStdDev, 2*time.Second) {
		t.Errorf("expected circular stddev ≈60.35min, got %v", result.CircularStdDev)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_WraparoundClusterNearMidnight
// is a second, denser wraparound fixture (five sessions spread across
// 23:50-00:10) confirming the tight-clustering case also resolves to a
// midnight-adjacent mean with a small stddev, not a nonsensical noon mean.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_WraparoundClusterNearMidnight(t *testing.T) {
	times := []time.Time{
		atClockTime(23, 50, 0),
		atClockTime(23, 55, 0),
		atClockTime(0, 0, 0),
		atClockTime(0, 5, 0),
		atClockTime(0, 10, 0),
	}
	result := ComputeCircularMeanAndStdDevOfTimeOfDay(times)

	if !durationCloseTo(result.MeanTimeOfDay, 0, time.Second) &&
		!durationCloseTo(result.MeanTimeOfDay, 24*time.Hour, time.Second) {
		t.Errorf("expected mean at midnight, got %v", result.MeanTimeOfDay)
	}
	expectedStdDev := time.Duration(7.0714 * float64(time.Minute))
	if !durationCloseTo(result.CircularStdDev, expectedStdDev, 5*time.Second) {
		t.Errorf("expected circular stddev ≈7.07min, got %v", result.CircularStdDev)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_TightNightlyClusterGivesSmallStdDev
// models the realistic "watches around 20:00 nightly with a few minutes of
// jitter" scenario the predictive pre-fetching engine is meant to detect.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_TightNightlyClusterGivesSmallStdDev(t *testing.T) {
	times := []time.Time{
		atClockTime(19, 55, 0),
		atClockTime(20, 0, 0),
		atClockTime(20, 5, 0),
		atClockTime(19, 58, 0),
		atClockTime(20, 2, 0),
	}
	result := ComputeCircularMeanAndStdDevOfTimeOfDay(times)

	if !durationCloseTo(result.MeanTimeOfDay, 20*time.Hour, time.Second) {
		t.Errorf("expected mean ≈20:00:00, got %v", result.MeanTimeOfDay)
	}
	if result.CircularStdDev > 5*time.Minute {
		t.Errorf("expected a tight stddev under 5 minutes, got %v", result.CircularStdDev)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_MaximallySpreadTimesGiveNearZeroR
// verifies the "no discernible pattern" case: four observations evenly
// spaced 6 hours apart around the clock have no common time-of-day, so R
// should be ≈0 and the (capped) circular stddev should be the maximum,
// 12 hours.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_MaximallySpreadTimesGiveNearZeroR(t *testing.T) {
	times := []time.Time{
		atClockTime(0, 0, 0),
		atClockTime(6, 0, 0),
		atClockTime(12, 0, 0),
		atClockTime(18, 0, 0),
	}
	result := ComputeCircularMeanAndStdDevOfTimeOfDay(times)

	if result.MeanResultantLength > 1e-6 {
		t.Errorf("expected R≈0 for maximally-spread observations, got %v", result.MeanResultantLength)
	}
	if result.CircularStdDev != 12*time.Hour {
		t.Errorf("expected the capped maximum stddev of 12h, got %v", result.CircularStdDev)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_IgnoresCalendarDate confirms
// that only the time-of-day component is used — observations on wildly
// different calendar dates but the same clock time produce the same
// result as observations all on the same date.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_IgnoresCalendarDate(t *testing.T) {
	sameDate := []time.Time{atClockTime(20, 0, 0), atClockTime(20, 10, 0)}
	differentDates := []time.Time{
		time.Date(2020, 1, 1, 20, 0, 0, 0, time.UTC),
		time.Date(2031, 12, 25, 20, 10, 0, 0, time.UTC),
	}

	sameDateResult := ComputeCircularMeanAndStdDevOfTimeOfDay(sameDate)
	differentDatesResult := ComputeCircularMeanAndStdDevOfTimeOfDay(differentDates)

	if sameDateResult.MeanTimeOfDay != differentDatesResult.MeanTimeOfDay {
		t.Errorf("expected identical mean time-of-day regardless of calendar date: %v vs %v",
			sameDateResult.MeanTimeOfDay, differentDatesResult.MeanTimeOfDay)
	}
}

// TestComputeCircularMeanAndStdDevOfTimeOfDay_PanicsOnEmptyInput documents
// the documented empty-input behavior.
func TestComputeCircularMeanAndStdDevOfTimeOfDay_PanicsOnEmptyInput(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected a panic for empty input")
		}
	}()
	ComputeCircularMeanAndStdDevOfTimeOfDay(nil)
}
