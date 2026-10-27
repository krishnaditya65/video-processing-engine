// Package httpapi exposes the Edge Routing & Packaging Engine's two public
// endpoints over net/http's standard library router (Go 1.22+ pattern
// matching, no external routing dependency needed):
//
//	GET /manifest/{assetId}   - personalized HLS/DASH manifest generation
//	GET /package/{assetId}/{chunkId} - JIT fMP4 chunk packaging
//
// Device/network/DRM hints are accepted as query parameters (documented on
// parsePlaybackContextFromRequest) so the handlers stay easy to exercise
// with curl or httptest without needing custom client headers.
package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/catalog"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/fmp4"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/manifest"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/metrics"
)

// NewEdgePackagingHttpServeMux builds and returns the complete
// http.ServeMux for this service, wiring both public endpoints and a
// liveness probe. Kept as a standalone constructor (rather than inline in
// main) so httptest-based handler tests can build the same mux the real
// binary serves.
func NewEdgePackagingHttpServeMux(logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest/{assetId}", handleGeneratePersonalizedManifestRequest(logger))
	mux.HandleFunc("GET /manifest/{assetId}/variant/{renditionFile}", handleGenerateHlsMediaPlaylistRequest(logger))
	mux.HandleFunc("GET /package/{assetId}/{chunkId}", handleJitPackageChunkRequest(logger))
	mux.HandleFunc("GET /healthz", handleLivenessProbeRequest)
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

// handleLivenessProbeRequest answers a trivial liveness probe used by
// load balancers/orchestrators; it performs no work beyond confirming the
// process can answer HTTP requests.
func handleLivenessProbeRequest(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// parsePlaybackContextFromRequest builds a domain.PlaybackContext from the
// request's path/query parameters, simulating the device-capability and
// network-quality hints a real edge node would derive from client-supplied
// headers (e.g. CMCD) or a device-capability database keyed by user-agent.
// Recognized query parameters:
//
//	low_end=true|false             - IsLowEndOrLegacyDevice
//	hevc=true|false                - SupportsHevcVideoCodec
//	av1=true|false                 - SupportsAv1VideoCodec
//	hdr=true|false                 - SupportsHighDynamicRangeVideo
//	eac3=true|false                - SupportsDolbyDigitalPlusAudio
//	max_height=<pixels>            - MaximumDecodableVerticalResolutionPixels
//	network=poor|moderate|good|excellent - NetworkQualityHint
//	drm=none|widevine|fairplay|playready - PreferredDrmSystem
//	format=hls|dash                - desired ManifestOutputFormat (default hls)
//	ads=true|false                 - whether to splice the mock ad break (default true)
func parsePlaybackContextFromRequest(r *http.Request) (context domain.PlaybackContext, outputFormat manifest.ManifestOutputFormat) {
	q := r.URL.Query()
	assetIdentifier := r.PathValue("assetId")

	deviceCapabilities := domain.DeviceCapabilityProfile{
		IsLowEndOrLegacyDevice:        parseBoolQueryParam(q, "low_end", false),
		SupportsHevcVideoCodec:        parseBoolQueryParam(q, "hevc", true),
		SupportsAv1VideoCodec:         parseBoolQueryParam(q, "av1", false),
		SupportsHighDynamicRangeVideo: parseBoolQueryParam(q, "hdr", true),
		SupportsDolbyDigitalPlusAudio: parseBoolQueryParam(q, "eac3", true),
	}
	if maxHeight, err := strconv.Atoi(q.Get("max_height")); err == nil {
		deviceCapabilities.MaximumDecodableVerticalResolutionPixels = maxHeight
	}

	networkHint := domain.NetworkQualityHint(q.Get("network"))
	if networkHint == "" {
		networkHint = domain.NetworkQualityGood
	}

	drmSystem := domain.DrmSystemIdentifier(q.Get("drm"))
	if drmSystem == "" {
		drmSystem = domain.DrmSystemWidevine
	}

	var adBreaks []domain.AdBreakInsertionPoint
	if parseBoolQueryParam(q, "ads", true) {
		adBreaks = catalog.GetAdBreakPlanForAsset(assetIdentifier)
	}

	outputFormat = manifest.ManifestOutputFormatHls
	if q.Get("format") == "dash" {
		outputFormat = manifest.ManifestOutputFormatDash
	}

	context = domain.PlaybackContext{
		AssetIdentifier:        assetIdentifier,
		RequestingSessionID:    r.Header.Get("X-Session-Id"),
		DeviceCapabilities:     deviceCapabilities,
		NetworkQualityHint:     networkHint,
		PreferredDrmSystem:     drmSystem,
		AdBreakInsertionPoints: adBreaks,
	}
	return context, outputFormat
}

// parseBoolQueryParam reads a "true"/"false" query parameter, returning
// defaultValue when the parameter is absent or unparsable.
func parseBoolQueryParam(q map[string][]string, key string, defaultValue bool) bool {
	values, present := q[key]
	if !present || len(values) == 0 {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(values[0])
	if err != nil {
		return defaultValue
	}
	return parsed
}

// handleGeneratePersonalizedManifestRequest returns the GET
// /manifest/{assetId} handler: it resolves the asset's variant ladder from
// the mock catalog, builds a PlaybackContext from request hints, runs the
// PersonalizedManifestBuilder pipeline, and writes the resulting manifest
// body with the correct content type. It also supports
// ?rendition={renditionId} to return a single HLS media playlist (rather
// than the master playlist) for that rendition, used by clients following
// a master playlist's variant URIs.
func handleGeneratePersonalizedManifestRequest(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		assetIdentifier := r.PathValue("assetId")
		if assetIdentifier == "" {
			http.Error(w, "assetId path segment is required", http.StatusBadRequest)
			return
		}

		playbackContext, outputFormat := parsePlaybackContextFromRequest(r)
		ladder := catalog.GetVariantLadderForAsset(assetIdentifier)

		output, err := manifest.NewPersonalizedManifestBuilder(playbackContext).
			WithVariantLadder(ladder).
			WithOutputFormat(outputFormat).
			Build()
		if err != nil {
			http.Error(w, fmt.Sprintf("manifest generation failed: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", output.ContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(output.Body))

		elapsed := time.Since(start)
		metrics.ObserveManifestGenerationLatency(string(outputFormat), elapsed.Seconds())
		metrics.IncrementDrmRequestCounter(string(playbackContext.PreferredDrmSystem))

		logger.Info("generated personalized manifest",
			"assetId", assetIdentifier,
			"format", outputFormat,
			"selectedRenditionCount", output.SelectedRenditionCount,
			"elapsed", elapsed.String(),
		)
	}
}

// handleGenerateHlsMediaPlaylistRequest returns the GET
// /manifest/{assetId}/variant/{renditionId}.m3u8 handler: the endpoint a
// real player's HTTP client reaches after resolving one of the master
// playlist's #EXT-X-STREAM-INF variant URIs (see
// generatePersonalizedHlsMasterPlaylistForPlaybackContext). It renders
// that single rendition's SSAI-spliced, DRM-signaled media playlist. The
// mux pattern captures the trailing "{renditionId}.m3u8" path segment as a
// whole (net/http's wildcard matching does not support a literal suffix
// after a wildcard), so the handler strips the ".m3u8" extension itself.
func handleGenerateHlsMediaPlaylistRequest(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		assetIdentifier := r.PathValue("assetId")
		renditionIdentifier := strings.TrimSuffix(r.PathValue("renditionFile"), ".m3u8")

		playbackContext, _ := parsePlaybackContextFromRequest(r)
		ladder := catalog.GetVariantLadderForAsset(assetIdentifier)

		output, err := manifest.BuildHlsMediaPlaylistForRendition(playbackContext, ladder, renditionIdentifier)
		if err != nil {
			http.Error(w, fmt.Sprintf("media playlist generation failed: %v", err), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", output.ContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(output.Body))

		logger.Info("generated HLS media playlist",
			"assetId", assetIdentifier,
			"rendition", renditionIdentifier,
			"elapsed", time.Since(start).String(),
		)
	}
}

// handleJitPackageChunkRequest returns the GET
// /package/{assetId}/{chunkId} handler: it fetches (from the mock
// catalog's simulated object storage) the video and audio track bytes for
// the requested chunk of the requested ?rendition, wraps them into a
// fragmented MP4 byte stream via package fmp4, and writes the resulting
// bytes as a video/mp4 response.
//
// An optional ?drm=widevine|fairplay|playready query parameter (default
// "none", preserving prior clear-content behavior) additionally encrypts
// the packaged chunk's mdat payload bytes with AES-128-CTR under the
// content key/IV package drm deterministically derives for this
// (assetId, chunkId) pair's rotation epoch (see internal/drm/encryption.go
// and internal/fmp4/encryption.go) — the same key a manifest requested
// with the matching ?drm= system would signal via #EXT-X-KEY /
// <ContentProtection>, so a player resolving that signaling and decrypting
// this response's mdat bytes with the resulting key recovers the original
// media bytes exactly (see the round-trip tests in those packages).
func handleJitPackageChunkRequest(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		assetIdentifier := r.PathValue("assetId")
		chunkIDRaw := r.PathValue("chunkId")
		renditionID := r.URL.Query().Get("rendition")
		if renditionID == "" {
			renditionID = "v1-480p-h264"
		}
		drmSystem := domain.DrmSystemIdentifier(r.URL.Query().Get("drm"))
		if drmSystem == "" {
			drmSystem = domain.DrmSystemNone
		}

		chunkSequenceIndex, err := strconv.Atoi(chunkIDRaw)
		if err != nil {
			http.Error(w, "chunkId must be an integer chunk sequence index", http.StatusBadRequest)
			return
		}

		ladder := catalog.GetVariantLadderForAsset(assetIdentifier)
		var targetRendition *domain.VariantRendition
		for i := range ladder.Renditions {
			if ladder.Renditions[i].RenditionIdentifier == renditionID {
				targetRendition = &ladder.Renditions[i]
				break
			}
		}
		if targetRendition == nil {
			http.Error(w, fmt.Sprintf("unknown rendition %q for asset %q", renditionID, assetIdentifier), http.StatusNotFound)
			return
		}
		if chunkSequenceIndex < 0 || chunkSequenceIndex >= len(targetRendition.Chunks) {
			http.Error(w, "chunkId out of range for this asset/rendition", http.StatusNotFound)
			return
		}

		chunkRef := targetRendition.Chunks[chunkSequenceIndex]
		videoBytes := catalog.FetchMockObjectStoragePayloadForKey(chunkRef.VideoTrackObjectStorageKey)
		audioBytes := catalog.FetchMockObjectStoragePayloadForKey(chunkRef.AudioTrackObjectStorageKey)

		packaged, err := fmp4.PackageElementaryStreamChunkAsFragmentedMp4(
			uint32(chunkSequenceIndex),
			targetRendition.TimescaleUnitsPerSecond,
			chunkRef.BaseMediaDecodeTimeInTimescaleUnits,
			videoBytes,
			audioBytes,
		)
		if err != nil {
			http.Error(w, fmt.Sprintf("JIT packaging failed: %v", err), http.StatusInternalServerError)
			return
		}

		if _, ok := drm.NewDrmKeyProviderForSystem(drmSystem); ok {
			plan := drm.ResolveKeyRotationPlanForChunk(assetIdentifier, chunkSequenceIndex)
			encryptedFragment, err := fmp4.EncryptMovieFragmentMediaData(packaged.MovieFragment, plan.ContentKey, plan.ContentInitializationVector)
			if err != nil {
				http.Error(w, fmt.Sprintf("mdat encryption failed: %v", err), http.StatusInternalServerError)
				return
			}
			packaged.MovieFragment = encryptedFragment
		}

		payload := packaged.FullPayload()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)

		elapsed := time.Since(start)
		metrics.ObserveJitPackagingLatency(elapsed.Seconds())
		metrics.IncrementDrmRequestCounter(string(drmSystem))

		logger.Info("JIT-packaged chunk",
			"assetId", assetIdentifier,
			"rendition", renditionID,
			"chunkSequenceIndex", chunkSequenceIndex,
			"payloadBytes", len(payload),
			"drm", drmSystem,
			"elapsed", elapsed.String(),
		)
	}
}
