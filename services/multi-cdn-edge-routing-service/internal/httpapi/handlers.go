// Package httpapi wires the CDN routing decision engine
// (internal/routing) and the predictive pre-fetching engine
// (internal/prediction) together into a small net/http server exposing
// GET /route/{assetId}, GET /prefetch-predictions/{userId}, GET /metrics
// (Prometheus), and GET /healthz.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/cdnhealth"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/metrics"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/prediction"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/routing"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/viewinghistory"
)

// NumberOfSyntheticNightsPerUser controls how many nights of synthetic
// viewing history internal/viewinghistory generates per user for the
// /prefetch-predictions/{userId} endpoint. Chosen well above
// prediction.MinimumSessionsRequiredForPatternDetection so the demo
// endpoint reliably has enough data to detect a pattern.
const NumberOfSyntheticNightsPerUser = 10

// Server holds the dependencies the HTTP handlers need: the CDN health
// registry, the routing scoring strategy, and the viewing-pattern
// predictor. Constructed once at process startup by cmd/server, and passed
// around as a value (its fields are themselves either safe-for-concurrent-use
// or immutable) rather than via globals, so tests can construct independent
// instances with independent registries.
type Server struct {
	HealthRegistry   *cdnhealth.CdnHealthRegistry
	ScoringStrategy  routing.CdnScoringStrategy
	PatternPredictor prediction.ViewingPatternPredictor
	Clock            func() time.Time
}

// NewServer constructs a Server with the given health registry, the
// service's default weighted scoring strategy, the default viewing-pattern
// predictor, and time.Now as its clock.
func NewServer(healthRegistry *cdnhealth.CdnHealthRegistry) *Server {
	return &Server{
		HealthRegistry:   healthRegistry,
		ScoringStrategy:  routing.NewDefaultWeightedCdnHealthScoringStrategy(),
		PatternPredictor: prediction.NewDefaultViewingPatternPredictor(),
		Clock:            time.Now,
	}
}

// NewHandler builds the complete net/http.Handler (route mux) for this
// service, wired to server's dependencies.
func (server *Server) NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /route/{assetId}", server.handleRoute)
	mux.HandleFunc("GET /prefetch-predictions/{userId}", server.handlePrefetchPredictions)
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", server.handleHealthz)
	return mux
}

// routeResponse is the JSON shape returned by GET /route/{assetId}.
type routeResponse struct {
	AssetIdentifier     string                       `json:"assetIdentifier"`
	ChosenProvider      domain.CdnProviderIdentifier `json:"chosenProvider"`
	ScoringStrategyName string                       `json:"scoringStrategyName"`
	FailoverTriggered   bool                         `json:"failoverTriggered"`
	Reasoning           string                       `json:"reasoning"`
	Candidates          []candidateScoreResponse     `json:"candidates"`
}

type candidateScoreResponse struct {
	Provider                domain.CdnProviderIdentifier `json:"provider"`
	CompositeScore          float64                      `json:"compositeScore"`
	HealthyEnoughToRoute    bool                         `json:"healthyEnoughToRoute"`
	UnhealthyReason         string                       `json:"unhealthyReason,omitempty"`
	EdgeLatencyMilliseconds float64                      `json:"edgeLatencyMilliseconds"`
	ErrorRatePercent        float64                      `json:"errorRatePercent"`
	CostPerGigabyteUSD      float64                      `json:"costPerGigabyteUsd"`
}

// handleRoute implements GET /route/{assetId}[?geo=us-east&asn=7018].
func (server *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("assetId")
	if assetID == "" {
		writeJSONError(w, "route", http.StatusBadRequest, "assetId path parameter is required")
		return
	}

	geo := r.URL.Query().Get("geo")
	if geo == "" {
		geo = "us-east"
	}
	asn := 0
	if asnParam := r.URL.Query().Get("asn"); asnParam != "" {
		parsedASN, err := strconv.Atoi(asnParam)
		if err != nil {
			writeJSONError(w, "route", http.StatusBadRequest, "asn query parameter must be an integer")
			return
		}
		asn = parsedASN
	}

	request := domain.PlaybackRequest{
		AssetIdentifier:              assetID,
		ClientGeographicRegion:       geo,
		ClientAutonomousSystemNumber: asn,
		RequestedAtUTC:               server.Clock(),
	}

	decision, err := routing.SelectOptimalCdnProviderForPlaybackRequest(request, server.HealthRegistry, server.ScoringStrategy)
	if err != nil {
		switch {
		case errors.Is(err, routing.ErrNoHealthyCdnProviderAvailable):
			metrics.RoutingDecisionUnavailableTotal.Inc()
			writeJSONError(w, "route", http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, routing.ErrNoCdnHealthDataAvailable):
			writeJSONError(w, "route", http.StatusServiceUnavailable, err.Error())
		default:
			writeJSONError(w, "route", http.StatusInternalServerError, err.Error())
		}
		return
	}

	metrics.RoutingDecisionsTotal.WithLabelValues(string(decision.ChosenProvider), strconv.FormatBool(decision.FailoverTriggered)).Inc()

	candidates := make([]candidateScoreResponse, 0, len(decision.CandidateScores))
	for _, c := range decision.CandidateScores {
		candidates = append(candidates, candidateScoreResponse{
			Provider:                c.Provider,
			CompositeScore:          c.CompositeScore,
			HealthyEnoughToRoute:    c.HealthyEnoughToRoute,
			UnhealthyReason:         c.UnhealthyReason,
			EdgeLatencyMilliseconds: c.SourceHealthReport.EdgeLatencyMilliseconds,
			ErrorRatePercent:        c.SourceHealthReport.ErrorRatePercent,
			CostPerGigabyteUSD:      c.SourceHealthReport.CostPerGigabyteUSD,
		})
	}

	writeJSON(w, "route", http.StatusOK, routeResponse{
		AssetIdentifier:     assetID,
		ChosenProvider:      decision.ChosenProvider,
		ScoringStrategyName: decision.ScoringStrategyName,
		FailoverTriggered:   decision.FailoverTriggered,
		Reasoning:           decision.Reasoning,
		Candidates:          candidates,
	})
}

// prefetchPredictionsResponse is the JSON shape returned by
// GET /prefetch-predictions/{userId}.
type prefetchPredictionsResponse struct {
	UserIdentifier  string                    `json:"userIdentifier"`
	PatternDetected bool                      `json:"patternDetected"`
	Reasoning       string                    `json:"reasoning"`
	Predictions     []cachePreWarmCommandView `json:"predictions"`
}

type cachePreWarmCommandView struct {
	AssetIdentifier           string    `json:"assetIdentifier"`
	TargetEdgePointOfPresence string    `json:"targetEdgePointOfPresence"`
	PredictedViewingStartUTC  time.Time `json:"predictedViewingStartUtc"`
	RecommendedPreWarmAtUTC   time.Time `json:"recommendedPreWarmAtUtc"`
	LeadTimeSeconds           float64   `json:"leadTimeSeconds"`
}

// handlePrefetchPredictions implements GET /prefetch-predictions/{userId}.
// It builds a small deterministic synthetic viewing history for the user
// (internal/viewinghistory), runs the pattern predictor over it, and
// returns the resulting CachePreWarmCommand (if any pattern was detected).
func (server *Server) handlePrefetchPredictions(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userId")
	if userID == "" {
		writeJSONError(w, "prefetch-predictions", http.StatusBadRequest, "userId path parameter is required")
		return
	}

	now := server.Clock()
	history := viewinghistory.BuildSyntheticNightlyViewingHistoryForUser(userID, NumberOfSyntheticNightsPerUser, now)

	nextEpisodeNumber := len(history) + 1
	nextAssetID := viewinghistory.DefaultSeriesIdentifier + "-e" + zeroPad(nextEpisodeNumber)

	command, detected := server.PatternPredictor.PredictNextCachePreWarmCommand(
		history, nextAssetID, viewinghistory.DefaultTargetEdgePointOfPresence, now,
	)

	response := prefetchPredictionsResponse{
		UserIdentifier:  userID,
		PatternDetected: detected,
		Predictions:     []cachePreWarmCommandView{},
	}

	if !detected {
		metrics.PrefetchPredictionsSkippedNoPatternTotal.Inc()
		pattern := server.PatternPredictor.DetectRecurringDailyViewingPattern(history)
		response.Reasoning = pattern.Reasoning
		writeJSON(w, "prefetch-predictions", http.StatusOK, response)
		return
	}

	metrics.PrefetchPredictionsEmittedTotal.WithLabelValues(command.TargetEdgePointOfPresence).Inc()
	response.Reasoning = command.PredictionConfidenceReasoning
	response.Predictions = append(response.Predictions, cachePreWarmCommandView{
		AssetIdentifier:           command.AssetIdentifier,
		TargetEdgePointOfPresence: command.TargetEdgePointOfPresence,
		PredictedViewingStartUTC:  command.PredictedViewingStartUTC,
		RecommendedPreWarmAtUTC:   command.RecommendedPreWarmAtUTC,
		LeadTimeSeconds:           command.LeadTime.Seconds(),
	})

	writeJSON(w, "prefetch-predictions", http.StatusOK, response)
}

// handleHealthz implements GET /healthz — a liveness probe with no
// dependency on downstream state.
func (server *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	metrics.HttpRequestsTotal.WithLabelValues("healthz", "2xx").Inc()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func zeroPad(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func writeJSON(w http.ResponseWriter, route string, statusCode int, body any) {
	metrics.HttpRequestsTotal.WithLabelValues(route, statusClass(statusCode)).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}

type jsonErrorBody struct {
	Error string `json:"error"`
}

func writeJSONError(w http.ResponseWriter, route string, statusCode int, message string) {
	writeJSON(w, route, statusCode, jsonErrorBody{Error: message})
}

func statusClass(statusCode int) string {
	switch statusCode / 100 {
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "other"
	}
}
