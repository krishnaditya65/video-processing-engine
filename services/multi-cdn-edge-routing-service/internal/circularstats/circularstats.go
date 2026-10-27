// Package circularstats implements circular (directional) statistics for
// time-of-day values — the mean and standard deviation of a set of clock
// times, computed correctly across the midnight wraparound boundary.
//
// # Why ordinary arithmetic mean is wrong for time-of-day
//
// Time-of-day is a circular quantity: 23:59 and 00:01 are two minutes
// apart, not ~24 hours apart. A naive arithmetic mean of clock times
// expressed as "seconds since midnight" gets this badly wrong. For example,
// averaging 23:50 and 00:10 (expressed as 85800s and 600s) arithmetically
// gives (85800+600)/2 = 43200s = 12:00 noon — the arithmetic mean lands
// exactly twelve hours away from the two actual observations, which are
// clustered tightly around midnight. Any viewing-pattern detector built on
// arithmetic time-of-day statistics would therefore silently produce
// nonsensical predictions for any user whose habitual viewing time happens
// to straddle midnight.
//
// # The circular mean method
//
// This package instead maps each time-of-day onto a point on the unit
// circle (treating the 24-hour clock as one full revolution, 2π radians),
// averages those points as 2D vectors, and maps the resultant vector's
// angle back onto the clock. Concretely, for a time-of-day expressed as a
// fraction of the day f ∈ [0, 1):
//
//	θ = f * 2π                              (angle in radians)
//	x = cos(θ), y = sin(θ)                  (point on the unit circle)
//
// Given n observations, the mean resultant vector is:
//
//	x̄ = (1/n) * Σ cos(θᵢ)
//	ȳ = (1/n) * Σ sin(θᵢ)
//
//	meanAngle = atan2(ȳ, x̄)                 (normalized back into [0, 2π))
//	R = sqrt(x̄² + ȳ²)                       (mean resultant length, 0..1)
//
// R is a measure of concentration: R = 1 means every observation landed on
// exactly the same time-of-day (zero spread); R → 0 means the observations
// are so spread around the clock that they average out to "no clear
// direction" (maximal spread — e.g. observations evenly spaced 6 hours
// apart give R = 0). This is the circular analogue of "how close together
// are these points", and is what makes the midnight-wraparound case work
// correctly: 23:50 and 00:10 map to two angles just a few degrees apart on
// the circle, average to a resultant vector pointing almost exactly at
// midnight, with R very close to 1 — the intuitively correct answer.
//
// The circular standard deviation is derived from R using the standard
// formula (Mardia & Jupp, "Directional Statistics", 2000; also given in
// Fisher, "Statistical Analysis of Circular Data", 1993):
//
//	circularStdDevRadians = sqrt(-2 * ln(R))
//
// which is then converted back into a clock duration the same way the mean
// angle is. This formula is monotonic in R (tighter clustering ⇒ larger R
// ⇒ smaller circular stddev) and, for small spreads, closely approximates
// the ordinary (linear) standard deviation you would compute if the data
// did not wrap around — which is exactly the regime this service's pattern
// detector operates in (a real recurring viewing habit has a spread of
// minutes, not hours).
package circularstats

import (
	"math"
	"time"
)

// secondsPerDay is the number of seconds in a circular "day" for the
// purposes of this package's angle mapping. Deliberately does not account
// for leap seconds or DST — this package operates purely on
// time-of-day-within-a-24-hour-clock, matching how "what time do you
// usually watch" is naturally understood.
const secondsPerDay = 24 * 60 * 60

// TimeOfDayResult is the output of ComputeCircularMeanAndStdDevOfTimeOfDay:
// the circular mean time-of-day and circular standard deviation of a set of
// timestamps, plus the mean resultant length R for callers that want the
// raw concentration measure.
type TimeOfDayResult struct {
	// MeanTimeOfDay is the circular mean, expressed as a duration since
	// midnight (00:00:00) — e.g. 20*time.Hour for 20:00:00. Always in
	// [0, 24h).
	MeanTimeOfDay time.Duration

	// CircularStdDev is the circular standard deviation, expressed as a
	// duration. It has the same units/interpretation as an ordinary
	// standard deviation for small spreads (see package doc), but note it
	// is not simply comparable to a linear stddev for large spreads since
	// it is bounded (R → 0 as spread → maximal, sending circularStdDev
	// toward +Inf mathematically; this package caps it at 12h — see
	// below — since no time-of-day spread can exceed half a day in a
	// meaningful sense).
	CircularStdDev time.Duration

	// MeanResultantLength is R ∈ [0, 1], the concentration measure
	// described in the package doc comment: 1 means all observations
	// share an identical time-of-day, values near 0 mean the observations
	// are scattered with no clear common time-of-day.
	MeanResultantLength float64
}

// timeOfDayAsFractionOfDay extracts the time-of-day component of t (hour,
// minute, second, nanosecond — the calendar date is discarded) and returns
// it as a fraction of a day in [0, 1).
func timeOfDayAsFractionOfDay(t time.Time) float64 {
	secondsSinceMidnight := t.Hour()*3600 + t.Minute()*60 + t.Second()
	fractionalSecond := float64(t.Nanosecond()) / 1e9
	return (float64(secondsSinceMidnight) + fractionalSecond) / secondsPerDay
}

// fractionOfDayToDuration converts a fraction-of-day value in [0, 1) back
// into a time.Duration since midnight.
func fractionOfDayToDuration(fraction float64) time.Duration {
	return time.Duration(fraction * secondsPerDay * float64(time.Second))
}

// ComputeCircularMeanAndStdDevOfTimeOfDay computes the circular mean and
// circular standard deviation of the time-of-day components of times, using
// the method documented at the top of this package. times must contain at
// least one element; ComputeCircularMeanAndStdDevOfTimeOfDay panics if
// given an empty slice (callers are expected to check length first — this
// mirrors how Go's own math/stat-adjacent idioms treat empty-input
// statistics as a programmer error, not a runtime condition to recover
// from).
func ComputeCircularMeanAndStdDevOfTimeOfDay(times []time.Time) TimeOfDayResult {
	if len(times) == 0 {
		panic("circularstats: ComputeCircularMeanAndStdDevOfTimeOfDay called with no observations")
	}

	var sumSin, sumCos float64
	for _, t := range times {
		fraction := timeOfDayAsFractionOfDay(t)
		angle := fraction * 2 * math.Pi
		sumSin += math.Sin(angle)
		sumCos += math.Cos(angle)
	}

	n := float64(len(times))
	meanSin := sumSin / n
	meanCos := sumCos / n

	meanAngle := math.Atan2(meanSin, meanCos)
	if meanAngle < 0 {
		meanAngle += 2 * math.Pi
	}
	meanFraction := meanAngle / (2 * math.Pi)

	meanResultantLength := math.Sqrt(meanSin*meanSin + meanCos*meanCos)
	// Guard tiny floating-point overshoot above 1.0 from accumulated
	// rounding error (mathematically R can never exceed 1).
	if meanResultantLength > 1 {
		meanResultantLength = 1
	}

	var circularStdDevFraction float64
	if meanResultantLength <= 1e-9 {
		// R ≈ 0: observations are maximally spread around the clock with
		// no discernible common time-of-day. sqrt(-2*ln(R)) diverges to
		// +Inf as R→0; we cap the reported spread at exactly half a day
		// (12h), the largest spread that is meaningful for a circular
		// quantity (a "standard deviation" larger than half the circle
		// does not correspond to anything observable on a 24-hour clock).
		circularStdDevFraction = 0.5
	} else {
		circularStdDevRadians := math.Sqrt(-2 * math.Log(meanResultantLength))
		circularStdDevFraction = circularStdDevRadians / (2 * math.Pi)
		if circularStdDevFraction > 0.5 {
			circularStdDevFraction = 0.5
		}
	}

	return TimeOfDayResult{
		MeanTimeOfDay:       fractionOfDayToDuration(meanFraction),
		CircularStdDev:      fractionOfDayToDuration(circularStdDevFraction),
		MeanResultantLength: meanResultantLength,
	}
}
