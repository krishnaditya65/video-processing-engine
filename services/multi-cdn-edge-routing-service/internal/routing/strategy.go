// Package routing implements the multi-CDN routing decision engine: given a
// domain.PlaybackRequest and the current health of every candidate CDN
// provider (sourced from an internal/cdnhealth.CdnHealthRegistry), it picks
// the CDN that should serve the request, with automatic failover away from
// unhealthy providers, and a fully documented, swappable scoring formula.
package routing

import (
	"fmt"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// Health thresholds beyond which a CDN provider is considered too unhealthy
// to route new playback requests to, regardless of how favorably it might
// otherwise score. These are the automatic-failover trip wires: a provider
// crossing either threshold is excluded from selection entirely (see
// selectOptimalCdnProviderForPlaybackRequest / SelectOptimalCdnProviderForPlaybackRequest).
//
// The specific numbers are illustrative defaults for this simulation, not a
// claim about any real CDN's SLA — a production deployment would tune these
// per traffic class (e.g. live sports vs. catalog VOD tolerate different
// latency budgets) and likely make them hysteresis-based (require N
// consecutive breaches before failing over, and M consecutive healthy
// samples before failing back) to avoid flapping. This simulation uses a
// simple instantaneous threshold for clarity and testability.
const (
	// MaxAcceptableEdgeLatencyMilliseconds is the rolling-average edge
	// latency above which a provider is considered unhealthy.
	MaxAcceptableEdgeLatencyMilliseconds = 250.0

	// MaxAcceptableErrorRatePercent is the rolling-window error rate (as a
	// percentage, 0-100) above which a provider is considered unhealthy.
	MaxAcceptableErrorRatePercent = 5.0
)

// CdnScoringStrategy is the Strategy-pattern interface for turning a CDN
// provider's health report into a single composite score, lower-is-better,
// given the full set of candidate reports being compared in this decision
// (needed because scoring is normalized relative to the other candidates —
// see WeightedCdnHealthScoringStrategy for why).
//
// Design pattern: Strategy. selectOptimalCdnProviderForPlaybackRequest is
// written entirely against this interface and has no knowledge of how
// scores are computed; swapping in a different weighting scheme, or an
// entirely different scoring philosophy (e.g. a strategy that heavily
// penalizes cost during off-peak hours, or one driven by a trained model
// instead of a hand-tuned formula) requires only implementing this
// interface and passing it to the selector — no changes to the selection
// or failover logic.
type CdnScoringStrategy interface {
	// StrategyName identifies this strategy for inclusion in
	// domain.RoutingDecision.ScoringStrategyName (observability/debugging).
	StrategyName() string

	// ComputeCompositeScore returns the composite score for candidate,
	// given allCandidates (the full set of reports being compared in this
	// decision, including candidate itself) for normalization purposes.
	// Lower scores are better. allCandidates must contain at least one
	// report; ComputeCompositeScore does not itself filter unhealthy
	// candidates — that is the selector's responsibility.
	ComputeCompositeScore(candidate domain.CdnHealthReport, allCandidates []domain.CdnHealthReport) float64
}

// WeightedCdnHealthScoringStrategy is the primary CdnScoringStrategy
// implementation: a linear combination of min-max-normalized latency, error
// rate, and cost.
//
// # Documented scoring formula
//
// For a candidate c among the set of all candidates being compared in one
// routing decision:
//
//	normalizedLatency(c)   = (c.EdgeLatencyMilliseconds - min(latency)) / (max(latency) - min(latency))
//	normalizedErrorRate(c) = (c.ErrorRatePercent        - min(errorRate)) / (max(errorRate) - min(errorRate))
//	normalizedCost(c)      = (c.CostPerGigabyteUSD       - min(cost))      / (max(cost)      - min(cost))
//
//	score(c) = LatencyWeight * normalizedLatency(c)
//	         + ErrorRateWeight * normalizedErrorRate(c)
//	         + CostWeight * normalizedCost(c)
//
// Each normalized term is a value in [0, 1]: 0 for whichever candidate has
// the best (lowest) raw value on that dimension among the current
// candidates, 1 for whichever has the worst. This min-max normalization
// (rather than, say, dividing by a fixed reference constant) is what lets
// three dimensions with wildly different units and scales — milliseconds,
// a percentage, and US dollars — be combined into one meaningful number: it
// makes every decision's scoring relative to the field of candidates
// actually available *right now*, which is also more robust to the
// absolute numbers drifting over time (e.g. all providers getting slower
// during a global network event) than a fixed-threshold normalization would
// be.
//
// Degenerate case: if every candidate has an identical raw value on some
// dimension (max == min for that dimension), that dimension's normalized
// term is defined to be 0 for every candidate — "no information to
// discriminate on this axis, so it doesn't move the score" — rather than
// dividing by zero.
//
// The three weights need not sum to exactly 1 (the selector only compares
// scores relative to each other within one decision), but by convention
// this service's default construction uses weights that do sum to 1 so
// that a returned score can loosely be read as "0 = best possible, 1 =
// worst possible, among today's candidates."
type WeightedCdnHealthScoringStrategy struct {
	// LatencyWeight is w1 in the formula above.
	LatencyWeight float64
	// ErrorRateWeight is w2 in the formula above.
	ErrorRateWeight float64
	// CostWeight is w3 in the formula above.
	CostWeight float64
}

// NewDefaultWeightedCdnHealthScoringStrategy returns the
// WeightedCdnHealthScoringStrategy this service uses by default: latency
// weighted most heavily (0.5), since it dominates perceived playback
// quality (startup delay, rebuffer risk), error rate next (0.3), since
// playback failures are worse than sub-optimal latency but rarer in
// steady-state, and cost weighted least (0.2) — cost still matters at
// hyperscale volume, but this service prioritizes viewer experience over
// margin when the two trade off.
func NewDefaultWeightedCdnHealthScoringStrategy() WeightedCdnHealthScoringStrategy {
	return WeightedCdnHealthScoringStrategy{
		LatencyWeight:   0.5,
		ErrorRateWeight: 0.3,
		CostWeight:      0.2,
	}
}

// StrategyName implements CdnScoringStrategy.
func (strategy WeightedCdnHealthScoringStrategy) StrategyName() string {
	return "weighted-cdn-health-scoring-v1"
}

// ComputeCompositeScore implements CdnScoringStrategy using the formula
// documented on WeightedCdnHealthScoringStrategy.
func (strategy WeightedCdnHealthScoringStrategy) ComputeCompositeScore(candidate domain.CdnHealthReport, allCandidates []domain.CdnHealthReport) float64 {
	minLatency, maxLatency := minMaxLatency(allCandidates)
	minErrorRate, maxErrorRate := minMaxErrorRate(allCandidates)
	minCost, maxCost := minMaxCost(allCandidates)

	normalizedLatency := normalize(candidate.EdgeLatencyMilliseconds, minLatency, maxLatency)
	normalizedErrorRate := normalize(candidate.ErrorRatePercent, minErrorRate, maxErrorRate)
	normalizedCost := normalize(candidate.CostPerGigabyteUSD, minCost, maxCost)

	return strategy.LatencyWeight*normalizedLatency +
		strategy.ErrorRateWeight*normalizedErrorRate +
		strategy.CostWeight*normalizedCost
}

// normalize applies min-max normalization, returning 0 in the degenerate
// case where max == min (see WeightedCdnHealthScoringStrategy's doc
// comment for the rationale).
func normalize(value, min, max float64) float64 {
	if max == min {
		return 0
	}
	return (value - min) / (max - min)
}

func minMaxLatency(reports []domain.CdnHealthReport) (min, max float64) {
	min, max = reports[0].EdgeLatencyMilliseconds, reports[0].EdgeLatencyMilliseconds
	for _, r := range reports[1:] {
		if r.EdgeLatencyMilliseconds < min {
			min = r.EdgeLatencyMilliseconds
		}
		if r.EdgeLatencyMilliseconds > max {
			max = r.EdgeLatencyMilliseconds
		}
	}
	return min, max
}

func minMaxErrorRate(reports []domain.CdnHealthReport) (min, max float64) {
	min, max = reports[0].ErrorRatePercent, reports[0].ErrorRatePercent
	for _, r := range reports[1:] {
		if r.ErrorRatePercent < min {
			min = r.ErrorRatePercent
		}
		if r.ErrorRatePercent > max {
			max = r.ErrorRatePercent
		}
	}
	return min, max
}

func minMaxCost(reports []domain.CdnHealthReport) (min, max float64) {
	min, max = reports[0].CostPerGigabyteUSD, reports[0].CostPerGigabyteUSD
	for _, r := range reports[1:] {
		if r.CostPerGigabyteUSD < min {
			min = r.CostPerGigabyteUSD
		}
		if r.CostPerGigabyteUSD > max {
			max = r.CostPerGigabyteUSD
		}
	}
	return min, max
}

// isHealthyEnoughToRoute reports whether report crosses neither automatic
// failover threshold, and if not, why.
func isHealthyEnoughToRoute(report domain.CdnHealthReport) (healthy bool, reason string) {
	if report.EdgeLatencyMilliseconds > MaxAcceptableEdgeLatencyMilliseconds {
		return false, fmt.Sprintf(
			"edge latency %.1fms exceeds the maximum acceptable %.1fms threshold",
			report.EdgeLatencyMilliseconds, MaxAcceptableEdgeLatencyMilliseconds,
		)
	}
	if report.ErrorRatePercent > MaxAcceptableErrorRatePercent {
		return false, fmt.Sprintf(
			"error rate %.2f%% exceeds the maximum acceptable %.2f%% threshold",
			report.ErrorRatePercent, MaxAcceptableErrorRatePercent,
		)
	}
	return true, ""
}
