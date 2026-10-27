// Package catalog provides a small in-memory mock content catalog: a
// deterministic variant ladder and ad-break plan per asset, and
// deterministically-generated elementary-stream chunk payload bytes
// standing in for what a real deployment would fetch from object storage
// (e.g. S3/GCS) by key. It exists purely so the HTTP layer (package
// httpapi) has something concrete to drive the manifest/packaging engines
// with, without requiring an external dependency for this demo service.
package catalog

import (
	"fmt"
	"time"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
)

// DefaultChunkDurationSeconds is the platform-standard chunk duration this
// mock catalog encodes every asset at, matching the JIT-packaging latency
// target documented in the README ("p99 < 30ms for a 2-second chunk").
const DefaultChunkDurationSeconds = 2.0

// DefaultTrackTimescaleUnitsPerSecond is the fMP4 media timescale every
// mock rendition's chunks are expressed in.
const DefaultTrackTimescaleUnitsPerSecond = 90000

// mockAssetTotalChunkCount is how many chunks (~40s of content at 2s/chunk)
// every mock asset in this catalog is synthesized with, enough to exercise
// multiple DRM key-rotation epochs (RotationPeriodInSegments=8) and a
// mid-roll ad break.
const mockAssetTotalChunkCount = 20

// GetVariantLadderForAsset returns a deterministic, synthesized
// domain.VariantLadder for any assetIdentifier: the same six-rung ladder
// (360p/H.264 up through 2160p HDR/AV1) is generated for every asset so
// the demo/test suite has a stable, realistic ladder to filter and
// package without needing a real encoding pipeline behind this service.
func GetVariantLadderForAsset(assetIdentifier string) domain.VariantLadder {
	renditionSpecs := []struct {
		id         string
		bitrate    int64
		width      int
		height     int
		videoCodec domain.VideoCodec
		audioCodec domain.AudioCodec
		hdr        bool
	}{
		{"v0-360p-h264", 700_000, 640, 360, domain.VideoCodecH264, domain.AudioCodecAac, false},
		{"v1-480p-h264", 1_200_000, 854, 480, domain.VideoCodecH264, domain.AudioCodecAac, false},
		{"v2-720p-h264", 2_800_000, 1280, 720, domain.VideoCodecH264, domain.AudioCodecAac, false},
		{"v3-1080p-hevc", 4_500_000, 1920, 1080, domain.VideoCodecHevc, domain.AudioCodecEac3, false},
		{"v4-1080p-hevc-hdr", 6_000_000, 1920, 1080, domain.VideoCodecHevc, domain.AudioCodecEac3, true},
		{"v5-2160p-av1-hdr", 14_000_000, 3840, 2160, domain.VideoCodecAv1, domain.AudioCodecEac3, true},
	}

	renditions := make([]domain.VariantRendition, 0, len(renditionSpecs))
	for _, spec := range renditionSpecs {
		renditions = append(renditions, domain.VariantRendition{
			RenditionIdentifier:     spec.id,
			PeakBitsPerSecond:       spec.bitrate,
			FrameWidthPixels:        spec.width,
			FrameHeightPixels:       spec.height,
			VideoCodec:              spec.videoCodec,
			AudioCodec:              spec.audioCodec,
			IsHighDynamicRange:      spec.hdr,
			TimescaleUnitsPerSecond: DefaultTrackTimescaleUnitsPerSecond,
			Chunks:                  synthesizeChunkReferencesForRendition(assetIdentifier, spec.id, mockAssetTotalChunkCount),
		})
	}

	return domain.VariantLadder{
		AssetIdentifier:     assetIdentifier,
		Renditions:          renditions,
		LiveEdgeGeneratedAt: time.Now().UTC(),
	}
}

// synthesizeChunkReferencesForRendition deterministically builds
// totalChunkCount SourceMediaChunkReference entries for one rendition,
// with sequential object-storage-key naming and correctly-accumulating
// base media decode times.
func synthesizeChunkReferencesForRendition(assetIdentifier string, renditionIdentifier string, totalChunkCount int) []domain.SourceMediaChunkReference {
	chunks := make([]domain.SourceMediaChunkReference, 0, totalChunkCount)
	for i := 0; i < totalChunkCount; i++ {
		chunks = append(chunks, domain.SourceMediaChunkReference{
			ChunkSequenceIndex:                  i,
			VideoTrackObjectStorageKey:          fmt.Sprintf("s3://edge-packaging-mock-bucket/%s/%s/video/chunk-%04d.bin", assetIdentifier, renditionIdentifier, i),
			AudioTrackObjectStorageKey:          fmt.Sprintf("s3://edge-packaging-mock-bucket/%s/%s/audio/chunk-%04d.bin", assetIdentifier, renditionIdentifier, i),
			DurationSeconds:                     DefaultChunkDurationSeconds,
			BaseMediaDecodeTimeInTimescaleUnits: uint64(i) * uint64(DefaultChunkDurationSeconds*DefaultTrackTimescaleUnitsPerSecond),
		})
	}
	return chunks
}

// GetAdBreakPlanForAsset returns a deterministic, synthesized single
// mid-roll ad break for any assetIdentifier: one two-ad-creative break at
// the 20-second mark (i.e. right after chunk index 9 of a 2s-chunk
// asset), used to exercise SSAI splicing end-to-end from the HTTP layer.
func GetAdBreakPlanForAsset(assetIdentifier string) []domain.AdBreakInsertionPoint {
	return []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "midroll-1",
			CueTimeOffsetSeconds: 20.0,
			TotalDurationSeconds: 4.0,
			AdCreativeChunks: []domain.AdCreativeChunkReference{
				{ObjectStorageKey: fmt.Sprintf("s3://edge-packaging-mock-bucket/ads/%s/creative-a.m4s", assetIdentifier), DurationSeconds: 2.0},
				{ObjectStorageKey: fmt.Sprintf("s3://edge-packaging-mock-bucket/ads/%s/creative-b.m4s", assetIdentifier), DurationSeconds: 2.0},
			},
		},
	}
}

// mockChunkFillByteForKey deterministically derives a single repeating
// fill byte for an object-storage key, so different chunks/tracks produce
// visibly different (but reproducible) mock payloads without needing an
// actual media file on disk.
func mockChunkFillByteForKey(objectStorageKey string) byte {
	var sum uint32
	for i, r := range objectStorageKey {
		sum += uint32(r) * uint32(i+1)
	}
	return byte(sum % 251)
}

// mockChunkPayloadSizeBytes is the synthesized payload size for every mock
// chunk, roughly representative of a compressed 2-second video/audio
// elementary-stream chunk at low bitrate.
const mockChunkPayloadSizeBytes = 4096

// FetchMockObjectStoragePayloadForKey simulates fetching one
// elementary-stream chunk's raw bytes from object storage by key: it
// returns a deterministic, reproducible byte slice (not real encoded
// media) so the JIT packager's box-writing logic can be exercised and
// tested without bundling actual video fixtures into this service.
func FetchMockObjectStoragePayloadForKey(objectStorageKey string) []byte {
	fill := mockChunkFillByteForKey(objectStorageKey)
	payload := make([]byte, mockChunkPayloadSizeBytes)
	for i := range payload {
		payload[i] = fill ^ byte(i)
	}
	return payload
}
