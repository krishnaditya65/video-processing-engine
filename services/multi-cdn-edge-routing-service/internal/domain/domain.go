// Package domain holds the dependency-free shared data model for the
// multi-cdn-edge-routing-service. Nothing in this package imports any other
// internal package of this service — every other package imports domain,
// never the reverse — which keeps the type graph acyclic and lets domain
// types be shared freely between the CDN-routing engine and the predictive
// pre-fetching engine without creating import cycles.
package domain

import "time"

// CdnProviderIdentifier names one of the (mock) CDN providers this service
// can route playback requests to. In a real deployment these would map to
// actual CDN accounts/configurations; here they are opaque, comparable
// identifiers used purely to key health reports and routing decisions.
type CdnProviderIdentifier string

// The three mock CDN providers this simulation models. Real hyperscale video
// platforms typically multi-source across 3-5 commercial CDNs for exactly
// the redundancy and cost-arbitrage reasons this service demonstrates.
const (
	CdnProviderCloudFront CdnProviderIdentifier = "cloudfront"
	CdnProviderAkamai     CdnProviderIdentifier = "akamai"
	CdnProviderFastly     CdnProviderIdentifier = "fastly"
)

// AllMockCdnProviderIdentifiers returns every CDN provider this simulation
// knows about, in a stable order. Callers that need to enumerate "every
// provider we could possibly route to" (e.g. to seed a health registry, or
// to build a deterministic candidate list for scoring) should use this
// slice rather than hard-coding the three constants above, so that adding a
// fourth mock provider later only requires editing this one place.
func AllMockCdnProviderIdentifiers() []CdnProviderIdentifier {
	return []CdnProviderIdentifier{
		CdnProviderCloudFront,
		CdnProviderAkamai,
		CdnProviderFastly,
	}
}

// PlaybackRequest describes a single client asking to start (or resume)
// playback of a video asset. It carries just enough client-network context
// (geographic region hint and Autonomous System Number) for a routing
// decision engine to reason about which CDN edge would serve the client
// best — mirroring what a real edge-DNS or Anycast-aware router would infer
// from the client's resolver or connection metadata.
type PlaybackRequest struct {
	// AssetIdentifier is the catalog identifier of the video asset the
	// client wants to play (e.g. "stranger-things-s5e1").
	AssetIdentifier string

	// ClientGeographicRegion is a coarse region hint for the requesting
	// client, e.g. "us-east", "eu-west", "ap-south". Real deployments
	// derive this from GeoIP lookups against the client's resolved IP.
	ClientGeographicRegion string

	// ClientAutonomousSystemNumber is the AS number of the network the
	// client is connecting from (e.g. an ISP's ASN). Real multi-CDN
	// routers use ASN-level historical performance data as a routing
	// signal because CDN performance varies materially by eyeball ISP.
	ClientAutonomousSystemNumber int

	// RequestedAtUTC is the timestamp the routing decision is being made
	// for. Exposed explicitly (rather than implicitly using time.Now())
	// so that routing decisions are deterministic and testable.
	RequestedAtUTC time.Time
}

// CdnHealthReport is one (simulated) rolling-metrics sample reported by a
// mock CDN provider. In production this would be the payload of a periodic
// health-check / synthetic-monitoring beacon (e.g. every 10-30 seconds from
// each edge PoP); here it is either hand-constructed in tests or produced by
// an in-memory synthetic feed (see internal/cdnhealth for the ingestion
// side and internal/httpapi for the feed that seeds the demo server).
type CdnHealthReport struct {
	// Provider identifies which CDN this report describes.
	Provider CdnProviderIdentifier

	// EdgeLatencyMilliseconds is the rolling-window average time-to-first-byte
	// latency observed from edge PoPs of this provider, in milliseconds.
	// Lower is better.
	EdgeLatencyMilliseconds float64

	// ErrorRatePercent is the rolling-window percentage (0-100) of requests
	// to this provider that resulted in a 5xx response or a connection
	// failure. Lower is better.
	ErrorRatePercent float64

	// CostPerGigabyteUSD is this provider's current effective egress cost
	// in US dollars per gigabyte delivered, including any volume-discount
	// tier currently in effect. Lower is better.
	CostPerGigabyteUSD float64

	// ReportedAtUTC is when this health sample was produced.
	ReportedAtUTC time.Time
}

// CandidateProviderScore captures the scoring outcome for a single CDN
// provider considered during a routing decision — both providers that were
// ultimately selected and those that were passed over are represented, so
// that callers (and API responses) can show full reasoning, not just the
// winner.
type CandidateProviderScore struct {
	// Provider identifies the CDN provider this score describes.
	Provider CdnProviderIdentifier

	// CompositeScore is the output of the active CdnScoringStrategy for
	// this provider; lower is better (see internal/routing for the
	// documented formula). It is only meaningful relative to other
	// candidates scored in the same decision, since normalization is
	// computed per-decision.
	CompositeScore float64

	// HealthyEnoughToRoute is false if this provider's reported metrics
	// crossed one of the documented unhealthy thresholds (see
	// internal/routing.MaxAcceptableEdgeLatencyMilliseconds and
	// internal/routing.MaxAcceptableErrorRatePercent) and was therefore
	// excluded from being selected, regardless of its composite score.
	HealthyEnoughToRoute bool

	// UnhealthyReason explains why HealthyEnoughToRoute is false. Empty
	// when HealthyEnoughToRoute is true.
	UnhealthyReason string

	// SourceHealthReport is the health report this score was computed
	// from, included so API responses can show the raw metrics alongside
	// the derived score.
	SourceHealthReport CdnHealthReport
}

// RoutingDecision is the fully-explained result of routing one
// PlaybackRequest to a CDN provider: the chosen provider plus the complete
// scored candidate set and a human-readable explanation, so that "why did
// we route here" is always answerable from the response itself — a
// deliberate design choice for operability at hyperscale, where opaque
// routing decisions are extremely costly to debug.
type RoutingDecision struct {
	// Request is the playback request this decision was made for.
	Request PlaybackRequest

	// ChosenProvider is the CDN provider selected to serve this request.
	ChosenProvider CdnProviderIdentifier

	// ScoringStrategyName identifies which CdnScoringStrategy implementation
	// produced this decision (see internal/routing.CdnScoringStrategy).
	ScoringStrategyName string

	// CandidateScores holds the scored outcome for every provider that was
	// considered, sorted by CompositeScore ascending (best first).
	CandidateScores []CandidateProviderScore

	// FailoverTriggered is true when the provider with the numerically
	// best composite score was excluded for being unhealthy, and a
	// different, healthy provider was chosen instead.
	FailoverTriggered bool

	// Reasoning is a human-readable one-paragraph explanation of the
	// decision, suitable for direct display in API responses or logs.
	Reasoning string
}

// ViewingSession represents one historical instance of a user starting
// playback of a particular episode/asset within a series. The predictive
// pre-fetching engine (internal/prediction) consumes sequences of these to
// detect recurring daily viewing-time patterns.
type ViewingSession struct {
	// UserIdentifier identifies the viewing account.
	UserIdentifier string

	// SeriesIdentifier identifies the series being watched across sessions.
	SeriesIdentifier string

	// AssetIdentifier identifies the specific episode/asset played in this
	// session (the "next episode" the predictor should pre-warm caches for
	// is usually, but not necessarily, the same across a short run of
	// sessions — the synthetic dataset in internal/viewinghistory advances
	// episode numbers session-to-session, matching real binge behavior).
	AssetIdentifier string

	// StartedAtUTC is the wall-clock timestamp the session began. Only the
	// time-of-day component (with correct handling of the day boundary) is
	// used by the circular-mean pattern detector; the calendar date is used
	// only to confirm the sessions really are one-per-day samples.
	StartedAtUTC time.Time
}

// CachePreWarmCommand is an actionable instruction emitted by the
// predictive pre-fetching engine: "load this asset into this edge PoP's
// cache before this time, because we predict a viewing session is about to
// start." A real deployment would publish this onto a message bus consumed
// by edge cache-warming agents; here it is returned directly from the HTTP
// API and is also what the unit tests assert lead-time math against.
type CachePreWarmCommand struct {
	// AssetIdentifier is the video asset to pre-warm into cache.
	AssetIdentifier string

	// TargetEdgePointOfPresence identifies which edge PoP's cache should be
	// pre-warmed. In this simulation it is derived from the user's most
	// recently observed geographic region.
	TargetEdgePointOfPresence string

	// PredictedViewingStartUTC is the predicted timestamp the user will
	// begin the viewing session that this pre-warm is protecting.
	PredictedViewingStartUTC time.Time

	// RecommendedPreWarmAtUTC is when the cache-warm operation should be
	// kicked off — PredictedViewingStartUTC minus the configured lead time.
	RecommendedPreWarmAtUTC time.Time

	// LeadTime is how far before the predicted viewing start this command
	// recommends starting the pre-warm (e.g. 5 minutes), included so
	// consumers don't have to re-derive it by subtracting the two
	// timestamps above.
	LeadTime time.Duration

	// PredictionConfidenceReasoning is a human-readable explanation of why
	// this prediction was made (e.g. the circular mean/stddev that
	// triggered it), for observability.
	PredictionConfidenceReasoning string
}
