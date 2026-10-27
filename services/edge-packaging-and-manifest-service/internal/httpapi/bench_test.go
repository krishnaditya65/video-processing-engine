package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkJitPackageChunkEndpoint measures end-to-end HTTP-handler
// latency for GET /package/{assetId}/{chunkId} (mock object-storage fetch
// + fMP4 box construction + HTTP write), the basis for this service's
// documented "JIT chunk packaging p99 < 30ms for a 2-second chunk" latency
// target.
func BenchmarkJitPackageChunkEndpoint(b *testing.B) {
	mux := newTestServeMux()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/package/bench-asset/0?rendition=v1-480p-h264", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", rec.Code)
		}
	}
}

// BenchmarkPersonalizedManifestEndpoint measures end-to-end HTTP-handler
// latency for GET /manifest/{assetId}, the basis for this service's
// documented "personalized manifest generation p99 < 15ms" latency
// target.
func BenchmarkPersonalizedManifestEndpoint(b *testing.B) {
	mux := newTestServeMux()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/manifest/bench-asset?drm=widevine", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", rec.Code)
		}
	}
}
