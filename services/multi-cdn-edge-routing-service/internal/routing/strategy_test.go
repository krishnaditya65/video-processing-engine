package routing

import (
	"math"
	"testing"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/cdnhealth"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

const floatComparisonEpsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < floatComparisonEpsilon
}

// TestWeightedCdnHealthScoringStrategy_MatchesHandComputedFixture verifies
// the composite-score formula against numbers worked out by hand in this
// test's comment, using the default weights (latency 0.5, errorRate 0.3,
// cost 0.2).
//
// Fixture:
//
//	CloudFront: latency=100ms, errorRate=1.0%, cost=$0.05/GB
//	Akamai:     latency=150ms, errorRate=2.0%, cost=$0.03/GB
//	Fastly:     latency=200ms, errorRate=0.5%, cost=$0.04/GB
//
// Normalization ranges: latency [100,200], errorRate [0.5,2.0], cost [0.03,0.05].
//
//	normLatency:  CF=0,        Akamai=0.5,      Fastly=1
//	normError:    CF=1/3,      Akamai=1,        Fastly=0
//	normCost:     CF=1,        Akamai=0,        Fastly=0.5
//
//	score(CF)      = 0.5*0    + 0.3*(1/3) + 0.2*1   = 0.30
//	score(Akamai)  = 0.5*0.5  + 0.3*1     + 0.2*0   = 0.55
//	score(Fastly)  = 0.5*1    + 0.3*0     + 0.2*0.5 = 0.60
func TestWeightedCdnHealthScoringStrategy_MatchesHandComputedFixture(t *testing.T) {
	reports := []domain.CdnHealthReport{
		{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
		{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 150, ErrorRatePercent: 2.0, CostPerGigabyteUSD: 0.03},
		{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 200, ErrorRatePercent: 0.5, CostPerGigabyteUSD: 0.04},
	}
	strategy := NewDefaultWeightedCdnHealthScoringStrategy()

	expected := map[domain.CdnProviderIdentifier]float64{
		domain.CdnProviderCloudFront: 0.30,
		domain.CdnProviderAkamai:     0.55,
		domain.CdnProviderFastly:     0.60,
	}

	for _, report := range reports {
		got := strategy.ComputeCompositeScore(report, reports)
		want := expected[report.Provider]
		if !almostEqual(got, want) {
			t.Errorf("%s: got score %.6f, want %.6f", report.Provider, got, want)
		}
	}
}

// TestWeightedCdnHealthScoringStrategy_DegenerateEqualCandidatesScoreZero
// verifies the documented degenerate-normalization case: when every
// candidate shares the same raw value on a dimension, that dimension
// contributes 0 to every candidate's score rather than dividing by zero.
func TestWeightedCdnHealthScoringStrategy_DegenerateEqualCandidatesScoreZero(t *testing.T) {
	reports := []domain.CdnHealthReport{
		{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
		{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
	}
	strategy := NewDefaultWeightedCdnHealthScoringStrategy()

	for _, report := range reports {
		got := strategy.ComputeCompositeScore(report, reports)
		if !almostEqual(got, 0) {
			t.Errorf("%s: expected score 0 for identical candidates, got %.6f", report.Provider, got)
		}
	}
}

func newRegistryWithReports(reports ...domain.CdnHealthReport) *cdnhealth.CdnHealthRegistry {
	registry := cdnhealth.NewCdnHealthRegistry()
	registry.IngestHealthReportFeed(reports)
	return registry
}

func samplePlaybackRequest() domain.PlaybackRequest {
	return domain.PlaybackRequest{
		AssetIdentifier:              "stranger-things-s5e1",
		ClientGeographicRegion:       "us-east",
		ClientAutonomousSystemNumber: 7018,
		RequestedAtUTC:               time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC),
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_PicksLowestScoringHealthyProvider
// exercises the happy path (no unhealthy providers) using the same fixture
// as the hand-computed scoring test: CloudFront should win with score 0.30.
func TestSelectOptimalCdnProviderForPlaybackRequest_PicksLowestScoringHealthyProvider(t *testing.T) {
	registry := newRegistryWithReports(
		domain.CdnHealthReport{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
		domain.CdnHealthReport{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 150, ErrorRatePercent: 2.0, CostPerGigabyteUSD: 0.03},
		domain.CdnHealthReport{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 200, ErrorRatePercent: 0.5, CostPerGigabyteUSD: 0.04},
	)

	decision, err := SelectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.ChosenProvider != domain.CdnProviderCloudFront {
		t.Fatalf("expected CloudFront to be chosen, got %s", decision.ChosenProvider)
	}
	if decision.FailoverTriggered {
		t.Fatalf("did not expect failover to trigger when all providers are healthy")
	}
	if !almostEqual(decision.CandidateScores[0].CompositeScore, 0.30) {
		t.Fatalf("expected winning score 0.30, got %.6f", decision.CandidateScores[0].CompositeScore)
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_FailsOverWhenBestScoringProviderIsUnhealthy
// is the failover-behavior test: Fastly has the numerically best composite
// score (0.30, hand-computed in the test comment) but is reporting an error
// rate of 10%, which crosses MaxAcceptableErrorRatePercent (5%). The
// selector must skip Fastly and choose CloudFront (score 0.45) instead of
// Akamai (score 0.60), and must report FailoverTriggered=true.
//
// Fixture: CF latency=100/err=1.0/cost=0.05, Akamai latency=150/err=2.0/cost=0.03,
// Fastly latency=50/err=10.0(unhealthy)/cost=0.02.
// Normalization ranges: latency[50,150], err[1,10], cost[0.02,0.05].
//
//	score(CF)     = 0.5*0.5    + 0.3*0        + 0.2*1      = 0.45
//	score(Akamai) = 0.5*1      + 0.3*(1/9)    + 0.2*(1/3)  = 0.6
//	score(Fastly) = 0.5*0      + 0.3*1        + 0.2*0      = 0.30  (best score, but unhealthy)
func TestSelectOptimalCdnProviderForPlaybackRequest_FailsOverWhenBestScoringProviderIsUnhealthy(t *testing.T) {
	registry := newRegistryWithReports(
		domain.CdnHealthReport{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
		domain.CdnHealthReport{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 150, ErrorRatePercent: 2.0, CostPerGigabyteUSD: 0.03},
		domain.CdnHealthReport{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 50, ErrorRatePercent: 10.0, CostPerGigabyteUSD: 0.02},
	)

	decision, err := SelectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.ChosenProvider != domain.CdnProviderCloudFront {
		t.Fatalf("expected failover to CloudFront, got %s", decision.ChosenProvider)
	}
	if !decision.FailoverTriggered {
		t.Fatalf("expected FailoverTriggered=true")
	}

	// Verify Fastly is present, scored best overall, but marked unhealthy.
	var fastlyScore *domain.CandidateProviderScore
	for i := range decision.CandidateScores {
		if decision.CandidateScores[i].Provider == domain.CdnProviderFastly {
			fastlyScore = &decision.CandidateScores[i]
		}
	}
	if fastlyScore == nil {
		t.Fatalf("expected Fastly to be present among candidate scores")
	}
	if fastlyScore.HealthyEnoughToRoute {
		t.Fatalf("expected Fastly to be marked unhealthy")
	}
	if !almostEqual(fastlyScore.CompositeScore, 0.30) {
		t.Fatalf("expected Fastly's raw score to be 0.30, got %.6f", fastlyScore.CompositeScore)
	}
	if decision.CandidateScores[0].Provider != domain.CdnProviderFastly {
		t.Fatalf("expected Fastly to still rank first by raw score despite being unhealthy")
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_LatencyThresholdAlsoTriggersFailover
// exercises the latency-based (as opposed to error-rate-based) automatic
// failover threshold. Akamai reports a latency of 300ms (over the 250ms
// threshold, so unhealthy) but otherwise excellent error rate and cost,
// while CloudFront reports the best possible latency but the worst error
// rate and cost. With the default weights (latency 0.5, errorRate 0.3,
// cost 0.2 — note 0.3+0.2 = 0.5, exactly matching the latency weight),
// this fixture is engineered to make Akamai's and CloudFront's composite
// scores land on an exact tie of 0.5 (worked out by hand below), which
// means the deterministic alphabetical tie-break in the selector picks
// Akamai — an unhealthy provider — as the nominal best-overall score,
// correctly exercising the "exclude the unhealthy top scorer, fail over to
// the best healthy one" path even though the two scores are equal rather
// than Akamai strictly winning outright.
//
// Hand-computed scores (ranges: latency[50,300], errorRate[0.1,5.0], cost[0.01,1.0]):
//
//	score(CloudFront) = 0.5*0 + 0.3*1 + 0.2*1 = 0.5   (best latency, worst error/cost)
//	score(Akamai)     = 0.5*1 + 0.3*0 + 0.2*0 = 0.5   (worst latency — unhealthy — best error/cost)
//	score(Fastly)     = 0.5*0.6 + 0.3*0.4898 + 0.2*0.4949 ≈ 0.546 (healthy, but not competitive)
func TestSelectOptimalCdnProviderForPlaybackRequest_LatencyThresholdAlsoTriggersFailover(t *testing.T) {
	registry := newRegistryWithReports(
		domain.CdnHealthReport{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 50, ErrorRatePercent: 5.0, CostPerGigabyteUSD: 1.0},
		domain.CdnHealthReport{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 300, ErrorRatePercent: 0.1, CostPerGigabyteUSD: 0.01}, // unhealthy: latency > 250ms
		domain.CdnHealthReport{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 200, ErrorRatePercent: 2.5, CostPerGigabyteUSD: 0.5},
	)

	decision, err := SelectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.ChosenProvider != domain.CdnProviderCloudFront {
		t.Fatalf("expected failover to CloudFront due to Akamai's latency breach, got %s", decision.ChosenProvider)
	}
	if !decision.FailoverTriggered {
		t.Fatalf("expected FailoverTriggered=true")
	}

	var akamaiScore *domain.CandidateProviderScore
	for i := range decision.CandidateScores {
		if decision.CandidateScores[i].Provider == domain.CdnProviderAkamai {
			akamaiScore = &decision.CandidateScores[i]
		}
	}
	if akamaiScore == nil || akamaiScore.HealthyEnoughToRoute {
		t.Fatalf("expected Akamai to be present and marked unhealthy due to its latency breach")
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_ReturnsErrorWhenAllProvidersUnhealthy
// covers total multi-CDN collapse: every provider crosses a threshold, so
// there is nowhere safe to route.
func TestSelectOptimalCdnProviderForPlaybackRequest_ReturnsErrorWhenAllProvidersUnhealthy(t *testing.T) {
	registry := newRegistryWithReports(
		domain.CdnHealthReport{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 999, ErrorRatePercent: 20, CostPerGigabyteUSD: 0.05},
		domain.CdnHealthReport{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 999, ErrorRatePercent: 20, CostPerGigabyteUSD: 0.03},
	)

	_, err := SelectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != ErrNoHealthyCdnProviderAvailable {
		t.Fatalf("expected ErrNoHealthyCdnProviderAvailable, got %v", err)
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_ReturnsErrorWhenRegistryEmpty
// covers the "no data at all" case, distinct from "all data is unhealthy".
func TestSelectOptimalCdnProviderForPlaybackRequest_ReturnsErrorWhenRegistryEmpty(t *testing.T) {
	registry := cdnhealth.NewCdnHealthRegistry()
	_, err := SelectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != ErrNoCdnHealthDataAvailable {
		t.Fatalf("expected ErrNoCdnHealthDataAvailable, got %v", err)
	}
}

// TestSelectOptimalCdnProviderForPlaybackRequest_UnexportedAliasBehavesIdentically
// exercises the unexported selectOptimalCdnProviderForPlaybackRequest alias
// named in this service's design brief, confirming it delegates to the same
// logic as the exported function.
func TestSelectOptimalCdnProviderForPlaybackRequest_UnexportedAliasBehavesIdentically(t *testing.T) {
	registry := newRegistryWithReports(
		domain.CdnHealthReport{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100, ErrorRatePercent: 1.0, CostPerGigabyteUSD: 0.05},
		domain.CdnHealthReport{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 150, ErrorRatePercent: 2.0, CostPerGigabyteUSD: 0.03},
	)
	decision, err := selectOptimalCdnProviderForPlaybackRequest(samplePlaybackRequest(), registry, NewDefaultWeightedCdnHealthScoringStrategy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.ChosenProvider != domain.CdnProviderCloudFront {
		t.Fatalf("expected CloudFront, got %s", decision.ChosenProvider)
	}
}
