package manifest

import (
	"strings"
	"testing"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/ssai"
)

// buildTestLadder constructs a small three-rung ladder with 6 chunks of 2s
// each (12s of content) per rendition, used across this file's tests.
func buildTestLadder() domain.VariantLadder {
	makeChunks := func() []domain.SourceMediaChunkReference {
		chunks := make([]domain.SourceMediaChunkReference, 6)
		for i := range chunks {
			chunks[i] = domain.SourceMediaChunkReference{ChunkSequenceIndex: i, DurationSeconds: 2.0}
		}
		return chunks
	}
	return domain.VariantLadder{
		AssetIdentifier: "test-asset",
		Renditions: []domain.VariantRendition{
			{RenditionIdentifier: "v0-360p-h264", PeakBitsPerSecond: 700_000, FrameWidthPixels: 640, FrameHeightPixels: 360, VideoCodec: domain.VideoCodecH264, AudioCodec: domain.AudioCodecAac, Chunks: makeChunks()},
			{RenditionIdentifier: "v1-720p-h264", PeakBitsPerSecond: 2_800_000, FrameWidthPixels: 1280, FrameHeightPixels: 720, VideoCodec: domain.VideoCodecH264, AudioCodec: domain.AudioCodecAac, Chunks: makeChunks()},
			{RenditionIdentifier: "v2-1080p-hevc", PeakBitsPerSecond: 4_500_000, FrameWidthPixels: 1920, FrameHeightPixels: 1080, VideoCodec: domain.VideoCodecHevc, AudioCodec: domain.AudioCodecEac3, Chunks: makeChunks()},
		},
	}
}

// basePlaybackContext returns a capable-device, good-network playback
// context for "test-asset", with no ad breaks and no DRM, as a baseline
// tests can selectively override.
func basePlaybackContext() domain.PlaybackContext {
	return domain.PlaybackContext{
		AssetIdentifier: "test-asset",
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         true,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint: domain.NetworkQualityExcellent,
		PreferredDrmSystem: domain.DrmSystemNone,
	}
}

// TestHlsMasterPlaylistStructureListsEverySurvivingVariant is a structural
// snapshot-style test: it verifies the generated HLS master playlist opens
// with #EXTM3U, carries one #EXT-X-STREAM-INF per surviving rendition, and
// that BANDWIDTH/RESOLUTION values match the input ladder.
func TestHlsMasterPlaylistStructureListsEverySurvivingVariant(t *testing.T) {
	output, err := NewPersonalizedManifestBuilder(basePlaybackContext()).
		WithVariantLadder(buildTestLadder()).
		WithOutputFormat(ManifestOutputFormatHls).
		Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if !strings.HasPrefix(output.Body, "#EXTM3U\n") {
		t.Errorf("expected playlist to start with #EXTM3U, got: %q", output.Body[:min(30, len(output.Body))])
	}
	if output.ContentType != "application/vnd.apple.mpegurl" {
		t.Errorf("unexpected content type: %s", output.ContentType)
	}
	if got, want := strings.Count(output.Body, "#EXT-X-STREAM-INF"), 3; got != want {
		t.Errorf("expected %d #EXT-X-STREAM-INF lines, got %d:\n%s", want, got, output.Body)
	}
	for _, want := range []string{"BANDWIDTH=700000", "BANDWIDTH=2800000", "BANDWIDTH=4500000", "RESOLUTION=640x360", "RESOLUTION=1920x1080"} {
		if !strings.Contains(output.Body, want) {
			t.Errorf("expected master playlist to contain %q:\n%s", want, output.Body)
		}
	}
	if output.SelectedRenditionCount != 3 {
		t.Errorf("expected SelectedRenditionCount=3, got %d", output.SelectedRenditionCount)
	}
}

// TestHlsMediaPlaylistSpliceOrderingAndTiming verifies the HLS media
// playlist generator interleaves ad segments at the correct position with
// #EXT-X-DISCONTINUITY markers bracketing the ad break, matching the SSAI
// requirement's "segment ordering/timing correctness".
func TestHlsMediaPlaylistSpliceOrderingAndTiming(t *testing.T) {
	ladder := buildTestLadder()
	context := basePlaybackContext()
	context.AdBreakInsertionPoints = []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "midroll",
			CueTimeOffsetSeconds: 6.0, // after chunk 2 (0,1,2 -> 6s)
			TotalDurationSeconds: 4.0,
			AdCreativeChunks: []domain.AdCreativeChunkReference{
				{ObjectStorageKey: "ad-a", DurationSeconds: 2.0},
				{ObjectStorageKey: "ad-b", DurationSeconds: 2.0},
			},
		},
	}

	rendition := ladder.Renditions[0]
	timeline, err := ssai.SpliceAdBreakSegmentsIntoManifestTimeline(rendition.Chunks, context.AdBreakInsertionPoints)
	if err != nil {
		t.Fatalf("splice error: %v", err)
	}

	body := generateHlsMediaPlaylistForRendition("test-asset", rendition, timeline, nil)

	discontinuityCount := strings.Count(body, "#EXT-X-DISCONTINUITY")
	if discontinuityCount != 2 {
		t.Errorf("expected 2 #EXT-X-DISCONTINUITY markers bracketing the ad break, got %d:\n%s", discontinuityCount, body)
	}

	adAIndex := strings.Index(body, "/ad-creative/ad-a")
	adBIndex := strings.Index(body, "/ad-creative/ad-b")
	chunk2Index := strings.Index(body, "/package/test-asset/2?")
	chunk3Index := strings.Index(body, "/package/test-asset/3?")

	if adAIndex == -1 || adBIndex == -1 || chunk2Index == -1 || chunk3Index == -1 {
		t.Fatalf("expected all of chunk2/adA/adB/chunk3 to appear in playlist:\n%s", body)
	}
	if !(chunk2Index < adAIndex && adAIndex < adBIndex && adBIndex < chunk3Index) {
		t.Errorf("expected ordering chunk2 < ad-a < ad-b < chunk3, got offsets %d, %d, %d, %d", chunk2Index, adAIndex, adBIndex, chunk3Index)
	}
}

// TestHlsKeySignalingPresentWhenDrmActiveAndAbsentWhenClear verifies
// per-format DRM signaling correctness for HLS: an #EXT-X-KEY tag is
// emitted with a DRM provider active, and is entirely absent for a clear
// (DrmSystemNone) playback context.
func TestHlsKeySignalingPresentWhenDrmActiveAndAbsentWhenClear(t *testing.T) {
	ladder := buildTestLadder()

	drmContext := basePlaybackContext()
	drmContext.PreferredDrmSystem = domain.DrmSystemWidevine
	drmOutput, err := NewPersonalizedManifestBuilder(drmContext).WithVariantLadder(ladder).WithOutputFormat(ManifestOutputFormatHls).Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if !strings.Contains(drmOutput.Body, "#EXT-X-SESSION-KEY") {
		t.Errorf("expected #EXT-X-SESSION-KEY when DRM is active:\n%s", drmOutput.Body)
	}

	clearContext := basePlaybackContext()
	clearOutput, err := NewPersonalizedManifestBuilder(clearContext).WithVariantLadder(ladder).WithOutputFormat(ManifestOutputFormatHls).Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if strings.Contains(clearOutput.Body, "#EXT-X-SESSION-KEY") || strings.Contains(clearOutput.Body, "#EXT-X-KEY") {
		t.Errorf("did not expect any key signaling for a clear playback context:\n%s", clearOutput.Body)
	}
}

// TestDashManifestStructureHasMpdRootAndPeriodsPerRendition verifies the
// generated DASH MPD is well-formed at a structural level: it opens with
// an XML declaration, has an <MPD> root, and contains a <Representation>
// per surviving rendition.
func TestDashManifestStructureHasMpdRootAndPeriodsPerRendition(t *testing.T) {
	output, err := NewPersonalizedManifestBuilder(basePlaybackContext()).
		WithVariantLadder(buildTestLadder()).
		WithOutputFormat(ManifestOutputFormatDash).
		Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if !strings.HasPrefix(output.Body, "<?xml") {
		t.Errorf("expected MPD to start with an XML declaration, got: %q", output.Body[:min(40, len(output.Body))])
	}
	if !strings.Contains(output.Body, "<MPD") || !strings.Contains(output.Body, "</MPD>") {
		t.Errorf("expected a well-formed <MPD>...</MPD> root element:\n%s", output.Body)
	}
	for _, id := range []string{"v0-360p-h264", "v1-720p-h264", "v2-1080p-hevc"} {
		if !strings.Contains(output.Body, `Representation id="`+id+`"`) {
			t.Errorf("expected a Representation for %q:\n%s", id, output.Body)
		}
	}
}

// TestDashManifestSplicesAdBreakIntoSeparatePeriod verifies DASH SSAI
// splicing uses the Multi-Period model: an ad break produces a distinct
// <Period id="ad-...">, positioned between two content periods.
func TestDashManifestSplicesAdBreakIntoSeparatePeriod(t *testing.T) {
	context := basePlaybackContext()
	context.AdBreakInsertionPoints = []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "midroll",
			CueTimeOffsetSeconds: 6.0,
			TotalDurationSeconds: 2.0,
			AdCreativeChunks:     []domain.AdCreativeChunkReference{{ObjectStorageKey: "ad-x", DurationSeconds: 2.0}},
		},
	}
	output, err := NewPersonalizedManifestBuilder(context).
		WithVariantLadder(buildTestLadder()).
		WithOutputFormat(ManifestOutputFormatDash).
		Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	adPeriodIndex := strings.Index(output.Body, `Period id="ad-midroll"`)
	if adPeriodIndex == -1 {
		t.Fatalf("expected an ad Period with id=\"ad-midroll\":\n%s", output.Body)
	}
	firstContentPeriodIndex := strings.Index(output.Body, `Period id="content-0"`)
	// Period ids are the raw index of the period within the reference
	// timeline's period sequence (content-0, ad-midroll at index 1, then
	// the next content run at index 2), not a separate "content period"
	// counter, so the period following the ad break is "content-2".
	secondContentPeriodIndex := strings.Index(output.Body, `Period id="content-2"`)
	if firstContentPeriodIndex == -1 || secondContentPeriodIndex == -1 {
		t.Fatalf("expected two content periods bracketing the ad period:\n%s", output.Body)
	}
	if !(firstContentPeriodIndex < adPeriodIndex && adPeriodIndex < secondContentPeriodIndex) {
		t.Errorf("expected ordering content-0 < ad-midroll < content-1, got offsets %d, %d, %d", firstContentPeriodIndex, adPeriodIndex, secondContentPeriodIndex)
	}
}

// TestDashContentProtectionPresentWhenDrmActive verifies per-format DRM
// signaling correctness for DASH: a <ContentProtection> element is present
// when a DRM provider is active.
func TestDashContentProtectionPresentWhenDrmActive(t *testing.T) {
	context := basePlaybackContext()
	context.PreferredDrmSystem = domain.DrmSystemPlayReady
	output, err := NewPersonalizedManifestBuilder(context).
		WithVariantLadder(buildTestLadder()).
		WithOutputFormat(ManifestOutputFormatDash).
		Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if !strings.Contains(output.Body, "<ContentProtection") {
		t.Errorf("expected a <ContentProtection> element when DRM is active:\n%s", output.Body)
	}
	if !strings.Contains(output.Body, "PlayReady") {
		t.Errorf("expected PlayReady-specific signaling:\n%s", output.Body)
	}
}

// TestLowEndDeviceProducesSmallerManifestThanCapableDevice verifies the
// end-to-end personalization effect: a low-end device's generated
// manifest advertises strictly fewer variants than a fully-capable
// device's manifest for the same asset.
func TestLowEndDeviceProducesSmallerManifestThanCapableDevice(t *testing.T) {
	ladder := buildTestLadder()

	capableOutput, err := NewPersonalizedManifestBuilder(basePlaybackContext()).WithVariantLadder(ladder).WithOutputFormat(ManifestOutputFormatHls).Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	lowEndContext := basePlaybackContext()
	lowEndContext.DeviceCapabilities.IsLowEndOrLegacyDevice = true
	lowEndOutput, err := NewPersonalizedManifestBuilder(lowEndContext).WithVariantLadder(ladder).WithOutputFormat(ManifestOutputFormatHls).Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	if lowEndOutput.SelectedRenditionCount >= capableOutput.SelectedRenditionCount {
		t.Errorf("expected low-end manifest to select fewer renditions than capable device: low-end=%d, capable=%d",
			lowEndOutput.SelectedRenditionCount, capableOutput.SelectedRenditionCount)
	}
	if strings.Contains(lowEndOutput.Body, "v2-1080p-hevc") {
		t.Errorf("did not expect the HEVC 1080p rendition in the low-end device's manifest:\n%s", lowEndOutput.Body)
	}
}
