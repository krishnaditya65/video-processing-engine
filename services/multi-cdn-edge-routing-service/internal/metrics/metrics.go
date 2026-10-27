// Package metrics defines the Prometheus metrics this service exposes on
// GET /metrics, and the small helper functions internal/httpapi calls to
// record them. Kept as its own package (rather than inlined in httpapi) so
// the metric definitions are a single, reviewable surface, matching how a
// real production service isolates "what we measure" from "how we serve
// requests."
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Registry is the Prometheus registry this service publishes metrics to. A
// dedicated registry (rather than the global default) keeps this service's
// metrics surface self-contained and trivially testable in isolation.
var Registry = prometheus.NewRegistry()

var factory = promauto.With(Registry)

// RoutingDecisionsTotal counts every CDN routing decision made via
// GET /route/{assetId}, labeled by the chosen provider and whether
// automatic failover was triggered — the headline operational signal for
// "is our multi-CDN routing behaving as expected," and specifically "how
// often are we failing over," which is the metric an on-call engineer would
// alert on.
var RoutingDecisionsTotal = factory.NewCounterVec(prometheus.CounterOpts{
	Name: "multi_cdn_routing_decisions_total",
	Help: "Total number of CDN routing decisions made, labeled by chosen provider and whether automatic failover was triggered.",
}, []string{"chosen_provider", "failover_triggered"})

// RoutingDecisionUnavailableTotal counts routing decisions that could not
// be made at all because no healthy CDN provider was available — a
// page-worthy signal distinct from an ordinary failover.
var RoutingDecisionUnavailableTotal = factory.NewCounter(prometheus.CounterOpts{
	Name: "multi_cdn_routing_no_healthy_provider_total",
	Help: "Total number of routing decisions that failed because every known CDN provider was unhealthy.",
})

// PrefetchPredictionsEmittedTotal counts every CachePreWarmCommand this
// service has emitted via GET /prefetch-predictions/{userId}, labeled by
// the target edge PoP — lets operators see pre-warm load distribution
// across PoPs.
var PrefetchPredictionsEmittedTotal = factory.NewCounterVec(prometheus.CounterOpts{
	Name: "multi_cdn_prefetch_predictions_emitted_total",
	Help: "Total number of predictive cache pre-warm commands emitted, labeled by target edge PoP.",
}, []string{"target_edge_pop"})

// PrefetchPredictionsSkippedNoPatternTotal counts prediction requests that
// found no confident recurring viewing pattern in the user's history.
var PrefetchPredictionsSkippedNoPatternTotal = factory.NewCounter(prometheus.CounterOpts{
	Name: "multi_cdn_prefetch_predictions_no_pattern_total",
	Help: "Total number of prefetch-prediction requests for which no confident recurring viewing pattern was detected.",
})

// HttpRequestsTotal counts every HTTP request this service handles, labeled
// by route and status code class — the standard baseline RED-method
// (Rate/Errors/Duration — this covers Rate and Errors) metric.
var HttpRequestsTotal = factory.NewCounterVec(prometheus.CounterOpts{
	Name: "multi_cdn_http_requests_total",
	Help: "Total number of HTTP requests handled, labeled by route and status code class.",
}, []string{"route", "status_class"})
