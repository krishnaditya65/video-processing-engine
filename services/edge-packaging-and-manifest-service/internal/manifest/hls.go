// hls.go implements the HLS (.m3u8) ManifestFormatGenerator strategy: a
// master playlist listing every surviving variant (EXT-X-STREAM-INF), each
// pointing at a per-rendition media playlist URL, plus a standalone media
// playlist generator (used both to serve
// /manifest/{assetId}?rendition=... requests and by the SSAI/DRM tests)
// that emits the SSAI-spliced, DRM-signaled segment list for one
// rendition.
package manifest

import (
	"fmt"
	"strings"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/ssai"
)

// hlsManifestFormatGenerator is the HLS implementation of
// ManifestFormatGenerator.
type hlsManifestFormatGenerator struct{}

// hlsMpegVersionCompatibilityLevel is the #EXT-X-VERSION this service
// declares; version 7 covers fMP4 (EXT-X-MAP), SAMPLE-AES-CTR, and
// EXT-X-DISCONTINUITY usage.
const hlsMpegVersionCompatibilityLevel = 7

// GenerateManifestBody renders the HLS master playlist for the given
// generation input by delegating to
// generatePersonalizedHlsMasterPlaylistForPlaybackContext.
func (hlsManifestFormatGenerator) GenerateManifestBody(input ManifestGenerationInput) (GeneratedManifestOutput, error) {
	body := generatePersonalizedHlsMasterPlaylistForPlaybackContext(input)
	return GeneratedManifestOutput{
		Body:        body,
		ContentType: "application/vnd.apple.mpegurl",
	}, nil
}

// generatePersonalizedHlsMasterPlaylistForPlaybackContext builds the HLS
// master playlist: an #EXT-X-STREAM-INF line per surviving rendition
// (already narrowed by the device-capability filter pipeline upstream)
// naming that rendition's bandwidth, resolution, and codec string, plus an
// #EXT-X-SESSION-KEY line up front when a DRM provider is active so
// players can pre-fetch a license before selecting a variant.
func generatePersonalizedHlsMasterPlaylistForPlaybackContext(input ManifestGenerationInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", hlsMpegVersionCompatibilityLevel)

	if input.DrmProvider != nil {
		// Session key for rotation index 0 is advertised up front so a
		// player can begin a license request before it has selected a
		// variant or fetched any segment; the per-segment #EXT-X-KEY
		// tags in the media playlist take precedence once a variant is
		// selected and rotation advances.
		sessionTag := input.DrmProvider.GenerateHlsKeySignalingTagForChunk(input.PlaybackContext.AssetIdentifier, 0)
		fmt.Fprintf(&b, "#EXT-X-SESSION-KEY:%s\n", sessionTag.AttributeString)
	}

	for _, rendition := range input.SelectedRenditions {
		codecString := buildRfc6381CodecString(rendition)
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\"\n",
			rendition.PeakBitsPerSecond, rendition.FrameWidthPixels, rendition.FrameHeightPixels, codecString)
		fmt.Fprintf(&b, "/manifest/%s/variant/%s.m3u8\n", input.PlaybackContext.AssetIdentifier, rendition.RenditionIdentifier)
	}

	return b.String()
}

// buildRfc6381CodecString renders a simplified RFC 6381 CODECS attribute
// value for a rendition's video+audio codec pair. Real codec strings carry
// profile/level/object-type-indication detail (e.g. "avc1.640028"); this
// simulation emits a representative but not fully spec-accurate string,
// sufficient for client codec-family negotiation in this demo.
func buildRfc6381CodecString(rendition domain.VariantRendition) string {
	videoPart := map[domain.VideoCodec]string{
		domain.VideoCodecH264: "avc1.640028",
		domain.VideoCodecHevc: "hvc1.2.4.L120.90",
		domain.VideoCodecAv1:  "av01.0.08M.08",
	}[rendition.VideoCodec]
	audioPart := map[domain.AudioCodec]string{
		domain.AudioCodecAac:  "mp4a.40.2",
		domain.AudioCodecEac3: "ec-3",
	}[rendition.AudioCodec]
	return videoPart + "," + audioPart
}

// generateHlsMediaPlaylistForRendition builds the per-rendition HLS media
// playlist: an #EXT-X-MAP init-segment reference, the SSAI-spliced
// sequence of content and ad segments (each ad break bracketed by
// #EXT-X-DISCONTINUITY per the HLS spec's requirement that any
// timeline/encoding discontinuity - which an ad creative from a different
// encode certainly is - be signaled), and, when a DRM provider is active,
// an #EXT-X-KEY line preceding the first segment of each key-rotation
// epoch (see package drm's rotation scheme).
func generateHlsMediaPlaylistForRendition(
	assetIdentifier string,
	rendition domain.VariantRendition,
	timeline []ssai.TimelineEntry,
	drmProvider drm.DrmKeyProvider,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", hlsMpegVersionCompatibilityLevel)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", hlsTargetDurationCeilingSeconds(timeline))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"/package/%s/init/%s\"\n", assetIdentifier, rendition.RenditionIdentifier)

	lastEmittedRotationIndex := -1

	for _, entry := range timeline {
		if entry.IsFirstEntryOfAdBreak {
			fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY\n")
		}
		if entry.IsFirstContentEntryAfterAdBreak {
			fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY\n")
		}

		if entry.Kind == ssai.TimelineEntryKindContent && drmProvider != nil {
			rotationIndex := drm.RotationIndexForChunkSequence(entry.SourceContentChunkSequenceIndex)
			if rotationIndex != lastEmittedRotationIndex {
				keyTag := drmProvider.GenerateHlsKeySignalingTagForChunk(assetIdentifier, entry.SourceContentChunkSequenceIndex)
				fmt.Fprintf(&b, "%s\n", keyTag.Render())
				lastEmittedRotationIndex = rotationIndex
			}
		}

		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", entry.DurationSeconds)
		fmt.Fprintf(&b, "%s\n", hlsSegmentUriForTimelineEntry(assetIdentifier, rendition.RenditionIdentifier, entry))
	}

	fmt.Fprintf(&b, "#EXT-X-ENDLIST\n")
	return b.String()
}

// hlsSegmentUriForTimelineEntry renders the segment URI for one spliced
// timeline entry: content entries route through this service's JIT
// packaging endpoint (/package/{assetId}/{chunkId}); ad entries route
// directly to the ad creative's object-storage key as a pass-through URI.
func hlsSegmentUriForTimelineEntry(assetIdentifier string, renditionIdentifier string, entry ssai.TimelineEntry) string {
	if entry.Kind == ssai.TimelineEntryKindAdCreative {
		return "/ad-creative/" + entry.SourceObjectStorageKeyOrChunkIndex
	}
	return fmt.Sprintf("/package/%s/%s?rendition=%s", assetIdentifier, entry.SourceObjectStorageKeyOrChunkIndex, renditionIdentifier)
}

// hlsTargetDurationCeilingSeconds computes the #EXT-X-TARGETDURATION value
// required by the HLS spec: the ceiling of the longest individual segment
// duration in the playlist.
func hlsTargetDurationCeilingSeconds(timeline []ssai.TimelineEntry) int {
	longest := 0.0
	for _, entry := range timeline {
		if entry.DurationSeconds > longest {
			longest = entry.DurationSeconds
		}
	}
	ceiling := int(longest)
	if float64(ceiling) < longest {
		ceiling++
	}
	if ceiling == 0 {
		ceiling = 1
	}
	return ceiling
}
