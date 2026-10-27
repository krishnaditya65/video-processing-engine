package prediction

import (
	"testing"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

func sessionAt(userID string, day int, hour, minute int) domain.ViewingSession {
	return domain.ViewingSession{
		UserIdentifier:   userID,
		SeriesIdentifier: "the-edge-cases-s1",
		AssetIdentifier:  "the-edge-cases-s1-e01",
		StartedAtUTC:     time.Date(2026, 8, day, hour, minute, 0, 0, time.UTC),
	}
}

func TestDetectRecurringDailyViewingPattern_TightNightlyClusterIsDetected(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-1", 1, 19, 55),
		sessionAt("user-1", 2, 20, 0),
		sessionAt("user-1", 3, 20, 5),
		sessionAt("user-1", 4, 19, 58),
		sessionAt("user-1", 5, 20, 2),
	}
	predictor := NewDefaultViewingPatternPredictor()
	pattern := predictor.DetectRecurringDailyViewingPattern(sessions)

	if !pattern.PatternDetected {
		t.Fatalf("expected a pattern to be detected, got reasoning: %s", pattern.Reasoning)
	}
	if pattern.CircularStdDev > 5*time.Minute {
		t.Errorf("expected a tight stddev under 5 minutes, got %v", pattern.CircularStdDev)
	}
	wantMean := 20 * time.Hour
	diff := pattern.MeanTimeOfDay - wantMean
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Minute {
		t.Errorf("expected mean time-of-day close to 20:00, got %v", pattern.MeanTimeOfDay)
	}
}

func TestDetectRecurringDailyViewingPattern_ScatteredSessionsAreNotDetected(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-2", 1, 6, 0),
		sessionAt("user-2", 2, 12, 0),
		sessionAt("user-2", 3, 18, 0),
		sessionAt("user-2", 4, 0, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	pattern := predictor.DetectRecurringDailyViewingPattern(sessions)

	if pattern.PatternDetected {
		t.Fatalf("did not expect a pattern to be detected for scattered viewing times")
	}
}

func TestDetectRecurringDailyViewingPattern_TooFewSessionsAreNotDetected(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-3", 1, 20, 0),
		sessionAt("user-3", 2, 20, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	pattern := predictor.DetectRecurringDailyViewingPattern(sessions)

	if pattern.PatternDetected {
		t.Fatalf("expected too-few-sessions to prevent detection regardless of tightness")
	}
	if pattern.SessionsConsidered != 2 {
		t.Errorf("expected SessionsConsidered=2, got %d", pattern.SessionsConsidered)
	}
}

// TestDetectRecurringDailyViewingPattern_MidnightWraparoundClusterIsDetected
// exercises the pattern detector against a habit that straddles midnight —
// confirming the wraparound-safe circular statistics propagate correctly
// into pattern detection, not just the lower-level circularstats package.
func TestDetectRecurringDailyViewingPattern_MidnightWraparoundClusterIsDetected(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-4", 1, 23, 55),
		sessionAt("user-4", 3, 0, 2),
		sessionAt("user-4", 4, 23, 58),
		sessionAt("user-4", 6, 0, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	pattern := predictor.DetectRecurringDailyViewingPattern(sessions)

	if !pattern.PatternDetected {
		t.Fatalf("expected a midnight-straddling pattern to be detected, got reasoning: %s", pattern.Reasoning)
	}
}

// TestPredictNextCachePreWarmCommand_LeadTimeCalculationIsExact verifies
// that RecommendedPreWarmAtUTC = PredictedViewingStartUTC - LeadTime exactly,
// and that the default lead time of 5 minutes is applied.
func TestPredictNextCachePreWarmCommand_LeadTimeCalculationIsExact(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-5", 1, 20, 0),
		sessionAt("user-5", 2, 20, 0),
		sessionAt("user-5", 3, 20, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) // midday, well before tonight's 20:00

	command, detected := predictor.PredictNextCachePreWarmCommand(sessions, "the-edge-cases-s1-e04", "pop-us-east-1", now)
	if !detected {
		t.Fatalf("expected a prediction to be produced")
	}

	wantStart := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	if !command.PredictedViewingStartUTC.Equal(wantStart) {
		t.Errorf("expected predicted start %v, got %v", wantStart, command.PredictedViewingStartUTC)
	}
	if command.LeadTime != DefaultPreWarmLeadTime {
		t.Errorf("expected default lead time %v, got %v", DefaultPreWarmLeadTime, command.LeadTime)
	}
	wantPreWarmAt := wantStart.Add(-5 * time.Minute)
	if !command.RecommendedPreWarmAtUTC.Equal(wantPreWarmAt) {
		t.Errorf("expected pre-warm-at %v, got %v", wantPreWarmAt, command.RecommendedPreWarmAtUTC)
	}
	if command.AssetIdentifier != "the-edge-cases-s1-e04" {
		t.Errorf("expected asset identifier to be passed through, got %s", command.AssetIdentifier)
	}
	if command.TargetEdgePointOfPresence != "pop-us-east-1" {
		t.Errorf("expected target PoP to be passed through, got %s", command.TargetEdgePointOfPresence)
	}
}

// TestPredictNextCachePreWarmCommand_RollsForwardToNextDayWhenTodaysTimeHasPassed
// verifies that when `now` is already past today's predicted time-of-day,
// the prediction rolls forward to tomorrow, not backward/negative.
func TestPredictNextCachePreWarmCommand_RollsForwardToNextDayWhenTodaysTimeHasPassed(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-6", 1, 20, 0),
		sessionAt("user-6", 2, 20, 0),
		sessionAt("user-6", 3, 20, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	now := time.Date(2026, 8, 4, 22, 0, 0, 0, time.UTC) // after tonight's usual 20:00

	command, detected := predictor.PredictNextCachePreWarmCommand(sessions, "asset", "pop", now)
	if !detected {
		t.Fatalf("expected a prediction to be produced")
	}

	wantStart := time.Date(2026, 8, 5, 20, 0, 0, 0, time.UTC) // tomorrow
	if !command.PredictedViewingStartUTC.Equal(wantStart) {
		t.Errorf("expected predicted start to roll forward to %v, got %v", wantStart, command.PredictedViewingStartUTC)
	}
}

// TestPredictNextCachePreWarmCommand_MidnightWraparoundRollForwardIsCorrect
// is the sharpest edge case for next-occurrence projection: a mean
// time-of-day just after midnight (00:05), with `now` late the same
// evening (23:50) — the correct next occurrence is only 15 minutes away
// (tonight rolling into tomorrow's 00:05), not almost a full day away as a
// naive same-day-only comparison might compute.
func TestPredictNextCachePreWarmCommand_MidnightWraparoundRollForwardIsCorrect(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-7", 1, 23, 55),
		sessionAt("user-7", 3, 0, 5),
		sessionAt("user-7", 5, 0, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	now := time.Date(2026, 8, 4, 23, 50, 0, 0, time.UTC)

	command, detected := predictor.PredictNextCachePreWarmCommand(sessions, "asset", "pop", now)
	if !detected {
		t.Fatalf("expected a prediction to be produced")
	}
	if command.PredictedViewingStartUTC.Before(now) {
		t.Fatalf("predicted start %v must not be before now %v", command.PredictedViewingStartUTC, now)
	}
	untilPredicted := command.PredictedViewingStartUTC.Sub(now)
	if untilPredicted > 30*time.Minute {
		t.Errorf("expected the next occurrence to be well under 30 minutes away, got %v", untilPredicted)
	}
}

// TestPredictNextCachePreWarmCommand_NoPatternReturnsNotDetected verifies
// the "no prediction" path returns detected=false and a zero-value command.
func TestPredictNextCachePreWarmCommand_NoPatternReturnsNotDetected(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-8", 1, 6, 0),
		sessionAt("user-8", 2, 12, 0),
		sessionAt("user-8", 3, 18, 0),
	}
	predictor := NewDefaultViewingPatternPredictor()
	command, detected := predictor.PredictNextCachePreWarmCommand(sessions, "asset", "pop", time.Now())

	if detected {
		t.Fatalf("did not expect a prediction for scattered sessions")
	}
	if (command != domain.CachePreWarmCommand{}) {
		t.Errorf("expected a zero-value command when not detected, got %+v", command)
	}
}

// TestViewingPatternPredictor_CustomThresholdsOverrideDefaults confirms a
// caller-configured predictor with a stricter threshold rejects a pattern
// the default predictor would accept.
func TestViewingPatternPredictor_CustomThresholdsOverrideDefaults(t *testing.T) {
	sessions := []domain.ViewingSession{
		sessionAt("user-9", 1, 19, 40),
		sessionAt("user-9", 2, 20, 20),
		sessionAt("user-9", 3, 19, 45),
	}

	lenient := NewDefaultViewingPatternPredictor()
	lenientResult := lenient.DetectRecurringDailyViewingPattern(sessions)

	strict := ViewingPatternPredictor{MaxCircularStdDevForPatternDetection: time.Minute}
	strictResult := strict.DetectRecurringDailyViewingPattern(sessions)

	if !lenientResult.PatternDetected {
		t.Fatalf("expected the lenient (default) predictor to detect this pattern")
	}
	if strictResult.PatternDetected {
		t.Fatalf("expected the strict (1-minute threshold) predictor to reject this pattern")
	}
}
