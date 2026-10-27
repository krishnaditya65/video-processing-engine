package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/cdnhealth"
	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

func newTestServer(t *testing.T, at time.Time) (*Server, http.Handler) {
	t.Helper()
	registry := cdnhealth.NewCdnHealthRegistry()
	registry.IngestHealthReportFeed(BuildSyntheticCdnHealthFeed(at))
	server := NewServer(registry)
	server.Clock = func() time.Time { return at }
	return server, server.NewHandler()
}

func TestHandleRoute_ReturnsChosenProviderAndReasoning(t *testing.T) {
	_, handler := newTestServer(t, time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/route/stranger-things-s5e1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body routeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.ChosenProvider == "" {
		t.Errorf("expected a non-empty chosen provider")
	}
	if body.Reasoning == "" {
		t.Errorf("expected non-empty reasoning")
	}
	if len(body.Candidates) != 3 {
		t.Errorf("expected 3 candidates (CloudFront/Akamai/Fastly), got %d", len(body.Candidates))
	}
}

func TestHandleRoute_MissingHealthDataReturnsServiceUnavailable(t *testing.T) {
	registry := cdnhealth.NewCdnHealthRegistry() // empty — no health data ingested
	server := NewServer(registry)

	req := httptest.NewRequest(http.MethodGet, "/route/asset-1", nil)
	rec := httptest.NewRecorder()
	server.NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRoute_AllUnhealthyReturnsServiceUnavailable(t *testing.T) {
	registry := cdnhealth.NewCdnHealthRegistry()
	registry.IngestHealthReportFeed([]domain.CdnHealthReport{
		{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 999, ErrorRatePercent: 50},
		{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 999, ErrorRatePercent: 50},
		{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 999, ErrorRatePercent: 50},
	})
	server := NewServer(registry)

	req := httptest.NewRequest(http.MethodGet, "/route/asset-1", nil)
	rec := httptest.NewRecorder()
	server.NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePrefetchPredictions_ReturnsAPrediction(t *testing.T) {
	_, handler := newTestServer(t, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))

	req := httptest.NewRequest(http.MethodGet, "/prefetch-predictions/user-42", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body prefetchPredictionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !body.PatternDetected {
		t.Fatalf("expected a pattern to be detected from the synthetic nightly-viewing dataset, reasoning: %s", body.Reasoning)
	}
	if len(body.Predictions) != 1 {
		t.Fatalf("expected exactly one prediction, got %d", len(body.Predictions))
	}
	prediction := body.Predictions[0]
	if prediction.PredictedViewingStartUTC.Before(time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("predicted start must be in the future relative to now")
	}
	gotLeadSeconds := prediction.PredictedViewingStartUTC.Sub(prediction.RecommendedPreWarmAtUTC).Seconds()
	if gotLeadSeconds != prediction.LeadTimeSeconds {
		t.Errorf("expected RecommendedPreWarmAtUTC to be exactly LeadTimeSeconds before PredictedViewingStartUTC")
	}
}

func TestHandleHealthz_ReturnsOk(t *testing.T) {
	_, handler := newTestServer(t, time.Now())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestHandleMetrics_ExposesPrometheusTextFormat(t *testing.T) {
	_, handler := newTestServer(t, time.Now())

	// Generate at least one data point for the counters before scraping.
	routeReq := httptest.NewRequest(http.MethodGet, "/route/asset-1", nil)
	handler.ServeHTTP(httptest.NewRecorder(), routeReq)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Errorf("expected non-empty metrics body")
	}
}
