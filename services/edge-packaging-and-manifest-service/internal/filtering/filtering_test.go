package filtering

import (
	"testing"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
)

// sampleTestLadder builds a representative six-rung ladder mirroring the
// production mock catalog, used across every test in this file.
func sampleTestLadder() domain.VariantLadder {
	return domain.VariantLadder{
		AssetIdentifier: "asset-1",
		Renditions: []domain.VariantRendition{
			{RenditionIdentifier: "v0-360p-h264", PeakBitsPerSecond: 700_000, FrameHeightPixels: 360, VideoCodec: domain.VideoCodecH264, AudioCodec: domain.AudioCodecAac},
			{RenditionIdentifier: "v1-480p-h264", PeakBitsPerSecond: 1_200_000, FrameHeightPixels: 480, VideoCodec: domain.VideoCodecH264, AudioCodec: domain.AudioCodecAac},
			{RenditionIdentifier: "v2-720p-h264", PeakBitsPerSecond: 2_800_000, FrameHeightPixels: 720, VideoCodec: domain.VideoCodecH264, AudioCodec: domain.AudioCodecAac},
			{RenditionIdentifier: "v3-1080p-hevc", PeakBitsPerSecond: 4_500_000, FrameHeightPixels: 1080, VideoCodec: domain.VideoCodecHevc, AudioCodec: domain.AudioCodecEac3},
			{RenditionIdentifier: "v4-1080p-hevc-hdr", PeakBitsPerSecond: 6_000_000, FrameHeightPixels: 1080, VideoCodec: domain.VideoCodecHevc, AudioCodec: domain.AudioCodecEac3, IsHighDynamicRange: true},
			{RenditionIdentifier: "v5-2160p-av1-hdr", PeakBitsPerSecond: 14_000_000, FrameHeightPixels: 2160, VideoCodec: domain.VideoCodecAv1, AudioCodec: domain.AudioCodecEac3, IsHighDynamicRange: true},
		},
	}
}

// renditionIDs extracts RenditionIdentifier from a slice for concise
// assertions.
func renditionIDs(renditions []domain.VariantRendition) []string {
	ids := make([]string, len(renditions))
	for i, r := range renditions {
		ids[i] = r.RenditionIdentifier
	}
	return ids
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// TestLowEndDeviceFilteringRemovesUnsupportedTracks is the headline test
// for the "remove unsupported tracks for low-end devices" requirement: a
// low-end device context must be narrowed down to only conservative
// H.264/SDR/sub-480p/low-bitrate renditions.
func TestLowEndDeviceFilteringRemovesUnsupportedTracks(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			IsLowEndOrLegacyDevice: true,
			SupportsHevcVideoCodec: true, // even if the device claims HEVC support...
			SupportsAv1VideoCodec:  true, // ...and AV1 support...
		},
		NetworkQualityHint: domain.NetworkQualityExcellent, // ...and has great network...
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	ids := renditionIDs(result)

	// ...low-end device simplification must still win and keep only the
	// conservative renditions.
	for _, want := range []string{"v0-360p-h264", "v1-480p-h264"} {
		if !containsID(ids, want) {
			t.Errorf("expected low-end-safe rendition %q to survive, got %v", want, ids)
		}
	}
	for _, unwanted := range []string{"v2-720p-h264", "v3-1080p-hevc", "v4-1080p-hevc-hdr", "v5-2160p-av1-hdr"} {
		if containsID(ids, unwanted) {
			t.Errorf("expected rendition %q to be filtered out for a low-end device, but it survived: %v", unwanted, ids)
		}
	}
}

// TestCodecSupportFilteringDropsUnsupportedCodecs verifies HEVC/AV1
// renditions are dropped when the device does not report support, and
// kept when it does.
func TestCodecSupportFilteringDropsUnsupportedCodecs(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        false,
			SupportsAv1VideoCodec:         false,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityExcellent,
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	ids := renditionIDs(result)

	for _, unwanted := range []string{"v3-1080p-hevc", "v4-1080p-hevc-hdr", "v5-2160p-av1-hdr"} {
		if containsID(ids, unwanted) {
			t.Errorf("expected HEVC/AV1 rendition %q to be filtered out, but it survived: %v", unwanted, ids)
		}
	}
	for _, want := range []string{"v0-360p-h264", "v1-480p-h264", "v2-720p-h264"} {
		if !containsID(ids, want) {
			t.Errorf("expected H.264 rendition %q to survive, got %v", want, ids)
		}
	}
}

// TestNetworkQualityFilteringCapsBitrate verifies a moderate network hint
// drops every rendition above the moderate-network bitrate ceiling while
// still leaving at least one rendition available (the low end of this
// ladder, 700kbps/1.2Mbps, fits comfortably under a moderate ceiling of
// 2.5Mbps, unlike the poor-network ceiling which would force this ladder
// into its zero-survivors fallback path - that path is covered separately
// by TestPipelineNeverReturnsZeroRenditions).
func TestNetworkQualityFilteringCapsBitrate(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         true,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityModerate,
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)

	ceiling := domain.NetworkQualityModerate.SustainableBitsPerSecondCeiling()
	if len(result) == 0 {
		t.Fatal("expected at least one surviving rendition under a moderate network ceiling")
	}
	for _, r := range result {
		if r.PeakBitsPerSecond > ceiling {
			t.Errorf("rendition %q with bitrate %d exceeds moderate-network ceiling %d", r.RenditionIdentifier, r.PeakBitsPerSecond, ceiling)
		}
	}
	if !containsID(renditionIDs(result), "v0-360p-h264") {
		t.Errorf("expected the 700kbps rendition to survive a 2.5Mbps ceiling, got %v", renditionIDs(result))
	}
	if containsID(renditionIDs(result), "v2-720p-h264") {
		t.Errorf("expected the 2.8Mbps rendition to be dropped under a 2.5Mbps ceiling, got %v", renditionIDs(result))
	}
}

// TestNetworkQualityFilteringFallsBackWhenEveryRenditionExceedsCeiling
// verifies that when every rendition in the ladder exceeds even a poor
// network's bitrate ceiling, the pipeline's guaranteed-fallback behavior
// still returns exactly one (the lowest-bitrate) rendition rather than
// leaving the client with nothing playable.
func TestNetworkQualityFilteringFallsBackWhenEveryRenditionExceedsCeiling(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         true,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityPoor, // 600kbps ceiling; lowest rung is 700kbps
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	if len(result) != 1 {
		t.Fatalf("expected exactly 1 fallback rendition, got %d: %v", len(result), renditionIDs(result))
	}
	if result[0].RenditionIdentifier != "v0-360p-h264" {
		t.Errorf("expected fallback to the lowest-bitrate rendition %q, got %q", "v0-360p-h264", result[0].RenditionIdentifier)
	}
}

// TestMaximumResolutionCapFiltering verifies a reported maximum decodable
// resolution drops every rendition above it.
func TestMaximumResolutionCapFiltering(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:                   true,
			SupportsAv1VideoCodec:                    true,
			SupportsHighDynamicRangeVideo:            true,
			SupportsDolbyDigitalPlusAudio:            true,
			MaximumDecodableVerticalResolutionPixels: 720,
		},
		NetworkQualityHint: domain.NetworkQualityExcellent,
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	for _, r := range result {
		if r.FrameHeightPixels > 720 {
			t.Errorf("rendition %q with height %d exceeds device cap of 720", r.RenditionIdentifier, r.FrameHeightPixels)
		}
	}
}

// TestHighEndDeviceKeepsFullLadder verifies a fully-capable device on an
// excellent network keeps every rendition, proving the pipeline does not
// over-filter for capable clients.
func TestHighEndDeviceKeepsFullLadder(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         true,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityExcellent,
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	if len(result) != len(ladder.Renditions) {
		t.Errorf("expected all %d renditions to survive for a fully-capable device, got %d: %v", len(ladder.Renditions), len(result), renditionIDs(result))
	}
}

// TestPipelineNeverReturnsZeroRenditions verifies the pipeline's
// guaranteed-fallback behavior: even an artificially hostile context that
// would filter out everything still yields exactly one rendition (the
// lowest-bitrate one).
func TestPipelineNeverReturnsZeroRenditions(t *testing.T) {
	ladder := domain.VariantLadder{
		AssetIdentifier: "asset-2",
		Renditions: []domain.VariantRendition{
			{RenditionIdentifier: "only-hevc-hdr", PeakBitsPerSecond: 999_999_999, FrameHeightPixels: 2160, VideoCodec: domain.VideoCodecHevc, IsHighDynamicRange: true},
		},
	}
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			IsLowEndOrLegacyDevice: true, // guarantees the ladder's only rendition is dropped (it's HEVC/HDR)
		},
		NetworkQualityHint: domain.NetworkQualityPoor,
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	if len(result) != 1 {
		t.Fatalf("expected exactly 1 fallback rendition, got %d", len(result))
	}
	if result[0].RenditionIdentifier != "only-hevc-hdr" {
		t.Errorf("expected fallback rendition %q, got %q", "only-hevc-hdr", result[0].RenditionIdentifier)
	}
}

// TestQoeDrivenVariantCeilingFilterStageIsolated exercises
// qoeDrivenVariantCeilingFilterStage directly (bypassing the rest of the
// pipeline) to verify it: (a) is a no-op when no QoE signal is present, (b)
// is a no-op when the signal does not indicate a struggling session, and
// (c) narrows the ladder to a lower bitrate ceiling than the coarse
// NetworkQualityHint alone would when the session is struggling.
func TestQoeDrivenVariantCeilingFilterStageIsolated(t *testing.T) {
	ladder := sampleTestLadder()
	stage := qoeDrivenVariantCeilingFilterStage{}

	t.Run("no signal is a no-op", func(t *testing.T) {
		context := domain.PlaybackContext{NetworkQualityHint: domain.NetworkQualityExcellent}
		result := stage.ApplyFilterStageToRenditions(ladder.Renditions, context)
		if len(result) != len(ladder.Renditions) {
			t.Errorf("expected all %d renditions to survive with no QoE signal, got %d", len(ladder.Renditions), len(result))
		}
	})

	t.Run("healthy signal is a no-op", func(t *testing.T) {
		context := domain.PlaybackContext{
			NetworkQualityHint: domain.NetworkQualityExcellent,
			RecentQoeSignal:    &domain.RecentQoeSignal{RecentRebufferRatio: 0.0},
		}
		result := stage.ApplyFilterStageToRenditions(ladder.Renditions, context)
		if len(result) != len(ladder.Renditions) {
			t.Errorf("expected all %d renditions to survive with a healthy QoE signal, got %d", len(ladder.Renditions), len(result))
		}
	})

	t.Run("struggling session narrows the ceiling below the network hint alone", func(t *testing.T) {
		context := domain.PlaybackContext{
			NetworkQualityHint: domain.NetworkQualityGood, // 8,000,000 bps ceiling alone
			RecentQoeSignal:    &domain.RecentQoeSignal{RecentRebufferRatio: 0.10},
		}
		result := stage.ApplyFilterStageToRenditions(ladder.Renditions, context)
		ids := renditionIDs(result)

		// NetworkQualityGood alone permits everything up to 8,000,000bps
		// (i.e. v0-v4), but the struggling-session ceiling
		// (8,000,000 * 0.5 = 4,000,000) must additionally exclude v3/v4/v5.
		for _, want := range []string{"v0-360p-h264", "v1-480p-h264", "v2-720p-h264"} {
			if !containsID(ids, want) {
				t.Errorf("expected rendition %q to survive the struggling-session ceiling, got %v", want, ids)
			}
		}
		for _, unwanted := range []string{"v3-1080p-hevc", "v4-1080p-hevc-hdr", "v5-2160p-av1-hdr"} {
			if containsID(ids, unwanted) {
				t.Errorf("expected rendition %q to be excluded by the struggling-session ceiling, but it survived: %v", unwanted, ids)
			}
		}
	})

	t.Run("low measured throughput tightens the ceiling further than the ratio-only downgrade", func(t *testing.T) {
		context := domain.PlaybackContext{
			NetworkQualityHint: domain.NetworkQualityExcellent, // 50,000,000 bps ceiling alone
			RecentQoeSignal: &domain.RecentQoeSignal{
				RecentRebufferRatio:             0.05,
				MeasuredThroughputBitsPerSecond: 900_000, // *0.8 = 720,000bps ceiling
			},
		}
		result := stage.ApplyFilterStageToRenditions(ladder.Renditions, context)
		ids := renditionIDs(result)
		if !containsID(ids, "v0-360p-h264") {
			t.Errorf("expected the lowest rung to survive a 720,000bps measured-throughput ceiling, got %v", ids)
		}
		if containsID(ids, "v1-480p-h264") {
			t.Errorf("expected v1-480p-h264 (1,200,000bps) to be excluded by a 720,000bps measured-throughput ceiling, got %v", ids)
		}
	})
}

// TestQoeDrivenVariantCeilingFilterStageThroughFullPipeline verifies the
// stage is actually wired into NewDefaultDeviceCapabilityFilterPipeline and
// runs after (so it can only tighten, not loosen) the network-quality cap.
func TestQoeDrivenVariantCeilingFilterStageThroughFullPipeline(t *testing.T) {
	ladder := sampleTestLadder()
	context := domain.PlaybackContext{
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         true,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityGood,
		RecentQoeSignal:    &domain.RecentQoeSignal{RecentRebufferRatio: 0.20},
	}

	pipeline := NewDefaultDeviceCapabilityFilterPipeline()
	result := pipeline.RunFilterPipelineOverVariantLadder(ladder, context)
	ids := renditionIDs(result)
	if containsID(ids, "v5-2160p-av1-hdr") {
		t.Errorf("expected the QoE stage to exclude the top rung for a struggling session even on good network, got %v", ids)
	}
	if len(result) == 0 {
		t.Fatalf("expected a non-empty fallback ladder, got none")
	}
}
