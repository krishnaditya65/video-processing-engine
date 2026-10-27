// Package prediction implements the predictive pre-fetching engine: given a
// user's historical viewing-session timestamps for a series, it detects
// whether they have a recurring daily viewing-time habit (using the
// circular time-of-day statistics in internal/circularstats), and if so,
// predicts the next occurrence and emits a domain.CachePreWarmCommand
// telling the edge cache layer when to start pre-warming the next episode.
package prediction

import (
	"fmt"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/circularstats"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// MaxCircularStdDevForPatternDetection is the threshold below which a
// user's historical viewing-session circular standard deviation is
// considered tight enough to call it a genuine recurring daily pattern
// (rather than noise). 45 minutes is a deliberately generous default —
// real viewers rarely start playback at the exact same minute every night,
// but a spread of under three-quarters of an hour is still a strong enough
// signal to justify proactively warming an edge cache, and false positives
// here are cheap (a wasted cache-warm) compared to false negatives (a cold
// cache at the moment of real demand).
const MaxCircularStdDevForPatternDetection = 45 * time.Minute

// DefaultPreWarmLeadTime is how far before a predicted viewing start this
// engine recommends beginning a cache pre-warm, by default. Five minutes is
// chosen to comfortably exceed typical edge cache-fill time for a single
// video asset's initial rendition set while not being so early that the
// cached content risks eviction before it's used.
const DefaultPreWarmLeadTime = 5 * time.Minute

// MinimumSessionsRequiredForPatternDetection is the smallest history size
// this predictor is willing to draw a conclusion from. Circular statistics
// are technically computable from a single observation (R would trivially
// be 1, stddev 0), but that would let one data point masquerade as a
// confident daily habit; three is the minimum needed to distinguish "real
// recurring pattern" from "coincidence."
const MinimumSessionsRequiredForPatternDetection = 3

// ViewingPatternPredictor detects recurring daily viewing-time patterns
// from historical domain.ViewingSession records and predicts the next
// occurrence.
//
// It is a small, stateless struct (rather than a free function) so that
// its two tunable thresholds (MaxCircularStdDev / MinimumSessionsRequired)
// can be overridden per instance — e.g. a stricter predictor for
// low-cost/low-risk pre-warming decisions, a looser one for
// high-value/high-cost content where over-predicting is more acceptable —
// without a global package-level constant forcing one policy on every
// caller.
type ViewingPatternPredictor struct {
	// MaxCircularStdDevForPatternDetection overrides the package-level
	// default of the same name when non-zero.
	MaxCircularStdDevForPatternDetection time.Duration

	// MinimumSessionsRequired overrides
	// MinimumSessionsRequiredForPatternDetection when non-zero.
	MinimumSessionsRequired int

	// PreWarmLeadTime overrides DefaultPreWarmLeadTime when non-zero.
	PreWarmLeadTime time.Duration
}

// NewDefaultViewingPatternPredictor returns a ViewingPatternPredictor
// configured with this package's documented default thresholds.
func NewDefaultViewingPatternPredictor() ViewingPatternPredictor {
	return ViewingPatternPredictor{
		MaxCircularStdDevForPatternDetection: MaxCircularStdDevForPatternDetection,
		MinimumSessionsRequired:              MinimumSessionsRequiredForPatternDetection,
		PreWarmLeadTime:                      DefaultPreWarmLeadTime,
	}
}

// resolvedThresholds fills in package defaults for any zero-valued fields,
// so a caller-constructed ViewingPatternPredictor{} (rather than one built
// via NewDefaultViewingPatternPredictor) still behaves sensibly.
func (predictor ViewingPatternPredictor) resolvedThresholds() (maxStdDev time.Duration, minSessions int, leadTime time.Duration) {
	maxStdDev = predictor.MaxCircularStdDevForPatternDetection
	if maxStdDev == 0 {
		maxStdDev = MaxCircularStdDevForPatternDetection
	}
	minSessions = predictor.MinimumSessionsRequired
	if minSessions == 0 {
		minSessions = MinimumSessionsRequiredForPatternDetection
	}
	leadTime = predictor.PreWarmLeadTime
	if leadTime == 0 {
		leadTime = DefaultPreWarmLeadTime
	}
	return maxStdDev, minSessions, leadTime
}

// DetectedViewingPattern is the outcome of pattern detection: either a
// confidently-detected recurring daily viewing time, or an explanation of
// why one was not detected.
type DetectedViewingPattern struct {
	// PatternDetected is true when the historical sessions' circular
	// standard deviation is below the predictor's threshold and there were
	// enough sessions to draw a conclusion from.
	PatternDetected bool

	// MeanTimeOfDay is the circular mean time-of-day across the input
	// sessions (see internal/circularstats). Meaningful even when
	// PatternDetected is false (it's still the best estimate available),
	// but callers should not act on it in that case.
	MeanTimeOfDay time.Duration

	// CircularStdDev is the circular standard deviation across the input
	// sessions.
	CircularStdDev time.Duration

	// SessionsConsidered is how many historical sessions were used.
	SessionsConsidered int

	// Reasoning is a human-readable explanation of the detection outcome.
	Reasoning string
}

// DetectRecurringDailyViewingPattern analyzes sessions (which must all
// belong to the same user/series — this function does not filter) and
// determines whether they exhibit a recurring daily viewing-time pattern,
// using the circular mean/stddev statistics from internal/circularstats.
func (predictor ViewingPatternPredictor) DetectRecurringDailyViewingPattern(sessions []domain.ViewingSession) DetectedViewingPattern {
	maxStdDev, minSessions, _ := predictor.resolvedThresholds()

	if len(sessions) < minSessions {
		return DetectedViewingPattern{
			SessionsConsidered: len(sessions),
			Reasoning: fmt.Sprintf(
				"only %d historical session(s) available; at least %d are required to distinguish a real recurring pattern from coincidence",
				len(sessions), minSessions,
			),
		}
	}

	timestamps := make([]time.Time, len(sessions))
	for i, s := range sessions {
		timestamps[i] = s.StartedAtUTC
	}

	stats := circularstats.ComputeCircularMeanAndStdDevOfTimeOfDay(timestamps)

	if stats.CircularStdDev > maxStdDev {
		return DetectedViewingPattern{
			MeanTimeOfDay:      stats.MeanTimeOfDay,
			CircularStdDev:     stats.CircularStdDev,
			SessionsConsidered: len(sessions),
			Reasoning: fmt.Sprintf(
				"circular stddev of %v across %d sessions exceeds the %v pattern-detection threshold; viewing times are too irregular to predict",
				stats.CircularStdDev, len(sessions), maxStdDev,
			),
		}
	}

	return DetectedViewingPattern{
		PatternDetected:    true,
		MeanTimeOfDay:      stats.MeanTimeOfDay,
		CircularStdDev:     stats.CircularStdDev,
		SessionsConsidered: len(sessions),
		Reasoning: fmt.Sprintf(
			"detected a recurring daily viewing pattern around %s (circular stddev %v across %d sessions, within the %v threshold)",
			formatTimeOfDay(stats.MeanTimeOfDay), stats.CircularStdDev, len(sessions), maxStdDev,
		),
	}
}

// PredictNextCachePreWarmCommand runs DetectRecurringDailyViewingPattern
// over sessions and, if a pattern is detected, projects it forward to the
// next calendar occurrence strictly after now, returning a
// domain.CachePreWarmCommand recommending when to pre-warm nextAssetIdentifier
// into targetEdgePoP's cache. detected reports false (with a zero-value
// command) when no confident pattern was found.
//
// Next-occurrence projection: given the detected MeanTimeOfDay, this
// function finds the next timestamp, on or after `now`, that has that
// time-of-day. If today's occurrence of that time-of-day is still in the
// future relative to now, that is the prediction; otherwise the prediction
// rolls forward to tomorrow's occurrence. This correctly handles the
// boundary case where `now` is itself past midnight but before a
// wraparound-mean time-of-day (e.g. mean 00:05, now is 23:50 the same
// calendar day — the next occurrence is 15 minutes away, not almost 24
// hours away), because the comparison operates on absolute timestamps
// derived by combining today's date with the mean time-of-day, not on
// duration-since-midnight values compared naively.
func (predictor ViewingPatternPredictor) PredictNextCachePreWarmCommand(
	sessions []domain.ViewingSession,
	nextAssetIdentifier string,
	targetEdgePoP string,
	now time.Time,
) (command domain.CachePreWarmCommand, detected bool) {
	pattern := predictor.DetectRecurringDailyViewingPattern(sessions)
	if !pattern.PatternDetected {
		return domain.CachePreWarmCommand{}, false
	}

	_, _, leadTime := predictor.resolvedThresholds()

	predictedStart := nextOccurrenceOfTimeOfDayOnOrAfter(pattern.MeanTimeOfDay, now)

	return domain.CachePreWarmCommand{
		AssetIdentifier:               nextAssetIdentifier,
		TargetEdgePointOfPresence:     targetEdgePoP,
		PredictedViewingStartUTC:      predictedStart,
		RecommendedPreWarmAtUTC:       predictedStart.Add(-leadTime),
		LeadTime:                      leadTime,
		PredictionConfidenceReasoning: pattern.Reasoning,
	}, true
}

// nextOccurrenceOfTimeOfDayOnOrAfter returns the next absolute timestamp,
// strictly after `after`, whose time-of-day equals timeOfDay. It first
// tries combining `after`'s own calendar date with timeOfDay; if that
// candidate is not strictly after `after` (i.e. that time-of-day has
// already passed today, or is exactly now), it rolls forward one calendar
// day.
func nextOccurrenceOfTimeOfDayOnOrAfter(timeOfDay time.Duration, after time.Time) time.Time {
	todayMidnight := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, after.Location())
	candidate := todayMidnight.Add(timeOfDay)
	if !candidate.After(after) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// formatTimeOfDay renders a time.Duration since midnight as an HH:MM:SS
// clock string, for human-readable reasoning text.
func formatTimeOfDay(d time.Duration) string {
	totalSeconds := int(d.Round(time.Second).Seconds())
	hours := (totalSeconds / 3600) % 24
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}
