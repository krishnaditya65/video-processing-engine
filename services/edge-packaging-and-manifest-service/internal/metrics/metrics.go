// Package metrics exposes this service's Prometheus instrumentation: a
// manifest-generation latency histogram (labeled by output format), a
// JIT-packaging latency histogram, and a per-DRM-system request counter,
// all served over the standard /metrics endpoint (package httpapi wires
// promhttp.Handler() there) for scraping by a Prometheus server or any
// OpenMetrics-compatible collector.
//
// Every metric here is registered against prometheus's global default
// registry (via promauto), matching how most Go services wire up
// client_golang: a package-level var block that registers metrics exactly
// once at process start, plus small package-level Observe/Increment
// helpers so callers (package httpapi) never touch the prometheus API
// directly.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricsNamespace prefixes every metric name this package registers
// (yielding e.g. "edge_packaging_manifest_generation_duration_seconds"),
// matching Prometheus's convention of namespacing metrics by the
// originating service to avoid collisions on a shared scrape target.
const metricsNamespace = "edge_packaging"

// manifestGenerationLatencySecondsHistogram observes wall-clock latency, in
// seconds, of the personalized manifest generation pipeline (filter ->
// splice -> DRM-resolve -> render), labeled by output format ("hls" or
// "dash") so the two formats' latency distributions (this service's README
// documents DASH as measurably slower than HLS to render) can be compared
// independently in a dashboard.
var manifestGenerationLatencySecondsHistogram = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "manifest_generation_duration_seconds",
		Help:      "Latency of personalized manifest generation (filter+splice+DRM-resolve+render), labeled by output format.",
		// This service's documented p99 target is 15ms and its measured
		// in-process benchmark average is single-digit microseconds; the
		// bucket boundaries span both that logic-only latency and the
		// realistic end-to-end range once real network/serialization
		// overhead is added in front of it.
		Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.015, 0.05, 0.1, 0.5, 1},
	},
	[]string{"output_format"},
)

// jitPackagingLatencySecondsHistogram observes wall-clock latency, in
// seconds, of the JIT (just-in-time) fMP4 chunk packaging path (mock
// object-storage fetch + fMP4 box construction).
var jitPackagingLatencySecondsHistogram = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "jit_packaging_duration_seconds",
		Help:      "Latency of JIT fMP4 chunk packaging (object-storage fetch + box construction).",
		// This service's documented p99 target is 30ms per ~2s chunk.
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.02, 0.03, 0.05, 0.1, 0.5, 1},
	},
)

// drmRequestsBySystemCounter counts manifest/packaging requests per
// requested DRM system ("none", "widevine", "fairplay", "playready"),
// giving operators a per-DRM-system request-volume breakdown (e.g. to spot
// a sudden shift in client population toward a given DRM system, or a spike
// in unencrypted "none" requests that might indicate a misconfigured
// client).
var drmRequestsBySystemCounter = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "drm_requests_total",
		Help:      "Count of manifest/packaging requests, labeled by requested DRM system.",
	},
	[]string{"drm_system"},
)

// ObserveManifestGenerationLatency records one manifest-generation
// pipeline run's wall-clock duration, labeled by outputFormat ("hls" or
// "dash").
func ObserveManifestGenerationLatency(outputFormat string, durationSeconds float64) {
	manifestGenerationLatencySecondsHistogram.WithLabelValues(outputFormat).Observe(durationSeconds)
}

// ObserveJitPackagingLatency records one JIT-packaging request's wall-clock
// duration.
func ObserveJitPackagingLatency(durationSeconds float64) {
	jitPackagingLatencySecondsHistogram.Observe(durationSeconds)
}

// IncrementDrmRequestCounter increments the per-DRM-system request counter
// for the given drmSystem label (e.g. "none", "widevine", "fairplay",
// "playready").
func IncrementDrmRequestCounter(drmSystem string) {
	drmRequestsBySystemCounter.WithLabelValues(drmSystem).Inc()
}
