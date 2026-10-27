package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/fmp4"
)

// newTestServeMux builds a ServeMux backed by a discard logger, so test
// output stays focused on assertions rather than structured log lines.
func newTestServeMux() http.Handler {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewEdgePackagingHttpServeMux(logger)
}

// TestHealthzReturnsOk verifies the liveness probe endpoint.
func TestHealthzReturnsOk(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestManifestEndpointDefaultsToHlsMasterPlaylist verifies GET
// /manifest/{assetId} with no query params returns a valid-looking HLS
// master playlist.
func TestManifestEndpointDefaultsToHlsMasterPlaylist(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/manifest/asset-123", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("unexpected content type: %s", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "#EXTM3U") {
		t.Errorf("expected an HLS master playlist, got: %s", rec.Body.String())
	}
}

// TestManifestEndpointSupportsDashFormat verifies ?format=dash returns a
// DASH MPD instead.
func TestManifestEndpointSupportsDashFormat(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/manifest/asset-123?format=dash", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/dash+xml" {
		t.Errorf("unexpected content type: %s", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "<?xml") {
		t.Errorf("expected a DASH MPD, got: %s", rec.Body.String())
	}
}

// TestManifestEndpointLowEndDeviceNarrowsVariantCount verifies the
// low_end=true query parameter measurably narrows the manifest's variant
// count relative to the default (capable-device) response.
func TestManifestEndpointLowEndDeviceNarrowsVariantCount(t *testing.T) {
	mux := newTestServeMux()

	defaultReq := httptest.NewRequest(http.MethodGet, "/manifest/asset-123", nil)
	defaultRec := httptest.NewRecorder()
	mux.ServeHTTP(defaultRec, defaultReq)
	defaultVariantCount := strings.Count(defaultRec.Body.String(), "#EXT-X-STREAM-INF")

	lowEndReq := httptest.NewRequest(http.MethodGet, "/manifest/asset-123?low_end=true", nil)
	lowEndRec := httptest.NewRecorder()
	mux.ServeHTTP(lowEndRec, lowEndReq)
	lowEndVariantCount := strings.Count(lowEndRec.Body.String(), "#EXT-X-STREAM-INF")

	if lowEndVariantCount >= defaultVariantCount {
		t.Errorf("expected low-end variant count (%d) to be less than default (%d)", lowEndVariantCount, defaultVariantCount)
	}
}

// TestPackageEndpointReturnsValidFmp4Payload verifies GET
// /package/{assetId}/{chunkId} returns a byte stream that parses as a
// valid ftyp/moov/moof/mdat box sequence.
func TestPackageEndpointReturnsValidFmp4Payload(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/package/asset-123/0?rendition=v1-480p-h264", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("unexpected content type: %s", ct)
	}

	headers, err := fmp4.WalkTopLevelBoxes(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("response body did not parse as a valid box sequence: %v", err)
	}
	wantOrder := []string{"ftyp", "moov", "moof", "mdat"}
	if len(headers) != len(wantOrder) {
		t.Fatalf("expected %d top-level boxes, got %d", len(wantOrder), len(headers))
	}
	for i, want := range wantOrder {
		if headers[i].Type != want {
			t.Errorf("box %d: expected %q, got %q", i, want, headers[i].Type)
		}
	}
}

// TestPackageEndpointRejectsOutOfRangeChunk verifies a chunk id beyond the
// asset's chunk count returns 404 rather than panicking or returning
// malformed output.
func TestPackageEndpointRejectsOutOfRangeChunk(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/package/asset-123/9999", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an out-of-range chunk id, got %d", rec.Code)
	}
}

// TestPackageEndpointRejectsNonIntegerChunkId verifies a non-integer
// {chunkId} path segment returns 400.
func TestPackageEndpointRejectsNonIntegerChunkId(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/package/asset-123/not-a-number", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a non-integer chunk id, got %d", rec.Code)
	}
}

// TestHlsMediaPlaylistEndpointServesVariantReferencedByMasterPlaylist
// verifies the full HLS discovery chain works end-to-end over HTTP: a
// variant URI printed in the master playlist actually resolves via GET
// and returns a valid media playlist (#EXT-X-MAP + #EXTINF entries).
func TestHlsMediaPlaylistEndpointServesVariantReferencedByMasterPlaylist(t *testing.T) {
	mux := newTestServeMux()

	masterReq := httptest.NewRequest(http.MethodGet, "/manifest/asset-123?ads=false", nil)
	masterRec := httptest.NewRecorder()
	mux.ServeHTTP(masterRec, masterReq)
	if masterRec.Code != http.StatusOK {
		t.Fatalf("master playlist request failed: %d", masterRec.Code)
	}

	var variantURI string
	for _, line := range strings.Split(masterRec.Body.String(), "\n") {
		if strings.HasPrefix(line, "/manifest/") {
			variantURI = line
			break
		}
	}
	if variantURI == "" {
		t.Fatalf("could not find a variant URI in master playlist:\n%s", masterRec.Body.String())
	}

	variantReq := httptest.NewRequest(http.MethodGet, variantURI, nil)
	variantRec := httptest.NewRecorder()
	mux.ServeHTTP(variantRec, variantReq)
	if variantRec.Code != http.StatusOK {
		t.Fatalf("expected 200 fetching variant URI %q, got %d: %s", variantURI, variantRec.Code, variantRec.Body.String())
	}
	if !strings.Contains(variantRec.Body.String(), "#EXT-X-MAP") || !strings.Contains(variantRec.Body.String(), "#EXTINF") {
		t.Errorf("expected a media playlist with #EXT-X-MAP and #EXTINF, got:\n%s", variantRec.Body.String())
	}
}

// TestManifestEndpointWithDrmQueryParamSignalsKeys verifies ?drm=fairplay
// results in FairPlay-specific key signaling reaching the HTTP response.
func TestManifestEndpointWithDrmQueryParamSignalsKeys(t *testing.T) {
	mux := newTestServeMux()
	req := httptest.NewRequest(http.MethodGet, "/manifest/asset-123?drm=fairplay&ads=false", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "com.apple.streamingkeydelivery") {
		t.Errorf("expected FairPlay key signaling in response, got: %s", rec.Body.String())
	}
}

// TestPackageEndpointWithDrmQueryParamEncryptsMediaData verifies
// ?drm=widevine on the JIT packaging endpoint actually produces a
// different, still box-valid, still-same-length payload than the
// clear-content (?drm unset/none) response for the identical chunk,
// demonstrating the mdat bytes are genuinely encrypted rather than just
// signaled.
func TestPackageEndpointWithDrmQueryParamEncryptsMediaData(t *testing.T) {
	mux := newTestServeMux()

	clearReq := httptest.NewRequest(http.MethodGet, "/package/asset-123/0?rendition=v1-480p-h264", nil)
	clearRec := httptest.NewRecorder()
	mux.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear-content request failed: %d", clearRec.Code)
	}

	encryptedReq := httptest.NewRequest(http.MethodGet, "/package/asset-123/0?rendition=v1-480p-h264&drm=widevine", nil)
	encryptedRec := httptest.NewRecorder()
	mux.ServeHTTP(encryptedRec, encryptedReq)
	if encryptedRec.Code != http.StatusOK {
		t.Fatalf("encrypted request failed: %d", encryptedRec.Code)
	}

	if encryptedRec.Body.Len() != clearRec.Body.Len() {
		t.Fatalf("expected AES-CTR encryption to preserve payload length: clear=%d encrypted=%d", clearRec.Body.Len(), encryptedRec.Body.Len())
	}
	if bytes.Equal(encryptedRec.Body.Bytes(), clearRec.Body.Bytes()) {
		t.Fatal("expected the ?drm=widevine response payload to differ from the clear-content payload")
	}

	headers, err := fmp4.WalkTopLevelBoxes(encryptedRec.Body.Bytes())
	if err != nil {
		t.Fatalf("encrypted response body did not parse as a valid box sequence: %v", err)
	}
	wantOrder := []string{"ftyp", "moov", "moof", "mdat"}
	if len(headers) != len(wantOrder) {
		t.Fatalf("expected %d top-level boxes in the encrypted response, got %d", len(wantOrder), len(headers))
	}
	for i, want := range wantOrder {
		if headers[i].Type != want {
			t.Errorf("box %d: expected %q, got %q", i, want, headers[i].Type)
		}
	}
}

// TestMetricsEndpointExposesPrometheusOutput verifies GET /metrics serves
// Prometheus-format output including this service's custom metric names,
// after driving at least one manifest and one packaging request so the
// metrics have been observed at least once.
func TestMetricsEndpointExposesPrometheusOutput(t *testing.T) {
	mux := newTestServeMux()

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/manifest/asset-123", nil))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/package/asset-123/0", nil))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"edge_packaging_manifest_generation_duration_seconds",
		"edge_packaging_jit_packaging_duration_seconds",
		"edge_packaging_drm_requests_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected /metrics output to contain %q, got:\n%s", want, body)
		}
	}
}
