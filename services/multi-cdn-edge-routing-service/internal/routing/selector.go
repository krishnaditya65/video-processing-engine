package routing

import (
	"errors"
	"fmt"
	"sort"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/cdnhealth"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// ErrNoCdnHealthDataAvailable is returned when the health registry has no
// reports at all to route against.
var ErrNoCdnHealthDataAvailable = errors.New("routing: no CDN health reports are available to route against")

// ErrNoHealthyCdnProviderAvailable is returned when every candidate
// provider's latest health report crosses an automatic-failover threshold —
// i.e. there is nowhere safe to route the request. A real deployment would
// treat this as a page-worthy incident (multi-CDN redundancy has fully
// collapsed); this simulation surfaces it as an error rather than silently
// routing to a known-unhealthy provider.
var ErrNoHealthyCdnProviderAvailable = errors.New("routing: every known CDN provider is currently unhealthy")

// SelectOptimalCdnProviderForPlaybackRequest implements the routing
// decision function described in the package documentation: it scores every
// CDN provider currently known to registry using strategy, excludes any
// provider whose latest health report crosses an automatic-failover
// threshold (see MaxAcceptableEdgeLatencyMilliseconds /
// MaxAcceptableErrorRatePercent), and returns the lowest-scoring healthy
// provider as a fully-explained domain.RoutingDecision.
//
// Failover behavior: scores are computed for *every* candidate (healthy or
// not) so the decision can report what would have won on pure score alone;
// selection, however, is only ever made among healthy candidates. If the
// provider with the single best composite score across all candidates is
// unhealthy, RoutingDecision.FailoverTriggered is set to true and
// Reasoning explains both why that provider was skipped and why the
// actually-chosen provider was picked instead.
//
// This is the long-form name requested for this function; SelectOptimalCdnProviderForPlaybackRequest
// is exported so internal/httpapi (and any other package) can call it
// directly. An unexported alias selectOptimalCdnProviderForPlaybackRequest
// exists below purely to satisfy the exact identifier named in this
// service's design brief; both names resolve to the same logic.
func SelectOptimalCdnProviderForPlaybackRequest(
	request domain.PlaybackRequest,
	registry *cdnhealth.CdnHealthRegistry,
	strategy CdnScoringStrategy,
) (domain.RoutingDecision, error) {
	allReports := registry.LatestReportsForAllProviders()
	if len(allReports) == 0 {
		return domain.RoutingDecision{}, ErrNoCdnHealthDataAvailable
	}

	candidateScores := make([]domain.CandidateProviderScore, 0, len(allReports))
	for _, report := range allReports {
		healthy, reason := isHealthyEnoughToRoute(report)
		candidateScores = append(candidateScores, domain.CandidateProviderScore{
			Provider:             report.Provider,
			CompositeScore:       strategy.ComputeCompositeScore(report, allReports),
			HealthyEnoughToRoute: healthy,
			UnhealthyReason:      reason,
			SourceHealthReport:   report,
		})
	}

	sort.SliceStable(candidateScores, func(i, j int) bool {
		if candidateScores[i].CompositeScore != candidateScores[j].CompositeScore {
			return candidateScores[i].CompositeScore < candidateScores[j].CompositeScore
		}
		// Deterministic tie-break so results are reproducible in tests.
		return candidateScores[i].Provider < candidateScores[j].Provider
	})

	bestOverall := candidateScores[0]

	var bestHealthy *domain.CandidateProviderScore
	for i := range candidateScores {
		if candidateScores[i].HealthyEnoughToRoute {
			bestHealthy = &candidateScores[i]
			break
		}
	}

	if bestHealthy == nil {
		return domain.RoutingDecision{
			Request:             request,
			ScoringStrategyName: strategy.StrategyName(),
			CandidateScores:     candidateScores,
			Reasoning: fmt.Sprintf(
				"no healthy CDN provider available for asset %q: all %d candidate(s) failed automatic health thresholds",
				request.AssetIdentifier, len(candidateScores),
			),
		}, ErrNoHealthyCdnProviderAvailable
	}

	failoverTriggered := bestHealthy.Provider != bestOverall.Provider

	reasoning := fmt.Sprintf(
		"selected %s for asset %q (composite score %.4f via %s: latency=%.1fms, errorRate=%.2f%%, cost=$%.4f/GB).",
		bestHealthy.Provider, request.AssetIdentifier, bestHealthy.CompositeScore, strategy.StrategyName(),
		bestHealthy.SourceHealthReport.EdgeLatencyMilliseconds,
		bestHealthy.SourceHealthReport.ErrorRatePercent,
		bestHealthy.SourceHealthReport.CostPerGigabyteUSD,
	)
	if failoverTriggered {
		reasoning += fmt.Sprintf(
			" Automatic failover engaged: %s had the numerically best composite score (%.4f) but was excluded because %s.",
			bestOverall.Provider, bestOverall.CompositeScore, bestOverall.UnhealthyReason,
		)
	}

	return domain.RoutingDecision{
		Request:             request,
		ChosenProvider:      bestHealthy.Provider,
		ScoringStrategyName: strategy.StrategyName(),
		CandidateScores:     candidateScores,
		FailoverTriggered:   failoverTriggered,
		Reasoning:           reasoning,
	}, nil
}

// selectOptimalCdnProviderForPlaybackRequest is an unexported alias for
// SelectOptimalCdnProviderForPlaybackRequest, kept so the exact decision
// function name from this service's design brief exists verbatim in the
// codebase (Go requires exported identifiers to be capitalized to be usable
// outside this package, hence the exported wrapper above being the one
// actually called by internal/httpapi).
func selectOptimalCdnProviderForPlaybackRequest(
	request domain.PlaybackRequest,
	registry *cdnhealth.CdnHealthRegistry,
	strategy CdnScoringStrategy,
) (domain.RoutingDecision, error) {
	return SelectOptimalCdnProviderForPlaybackRequest(request, registry, strategy)
}
