// Command gensample is a developer utility (not part of the production
// service) that exercises the manifest package's public API against the
// mock catalog and prints real, generated HLS/DASH manifest output to
// stdout. It exists solely to produce the "real generated output from a
// test run" pasted into this service's README - run it with
// `go run ./cmd/gensample` to reproduce that output yourself.
package main

import (
	"fmt"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/catalog"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/manifest"
)

func main() {
	ladder := catalog.GetVariantLadderForAsset("stranger-things-s5e1")
	adBreaks := catalog.GetAdBreakPlanForAsset("stranger-things-s5e1")

	context := domain.PlaybackContext{
		AssetIdentifier: "stranger-things-s5e1",
		DeviceCapabilities: domain.DeviceCapabilityProfile{
			SupportsHevcVideoCodec:        true,
			SupportsAv1VideoCodec:         false,
			SupportsHighDynamicRangeVideo: true,
			SupportsDolbyDigitalPlusAudio: true,
		},
		NetworkQualityHint:     domain.NetworkQualityGood,
		PreferredDrmSystem:     domain.DrmSystemWidevine,
		AdBreakInsertionPoints: adBreaks,
	}

	hlsOutput, err := manifest.NewPersonalizedManifestBuilder(context).
		WithVariantLadder(ladder).
		WithOutputFormat(manifest.ManifestOutputFormatHls).
		Build()
	if err != nil {
		panic(err)
	}
	fmt.Println("=== HLS MASTER PLAYLIST (good network, HEVC+HDR capable device, Widevine) ===")
	fmt.Println(hlsOutput.Body)

	mediaOutput, err := manifest.BuildHlsMediaPlaylistForRendition(context, ladder, "v0-360p-h264")
	if err != nil {
		panic(err)
	}
	fmt.Println("=== HLS MEDIA PLAYLIST for v0-360p-h264 (with midroll ad break spliced in) ===")
	fmt.Println(mediaOutput.Body)

	lowEndContext := context
	lowEndContext.DeviceCapabilities.IsLowEndOrLegacyDevice = true
	lowEndContext.NetworkQualityHint = domain.NetworkQualityPoor
	lowEndOutput, err := manifest.NewPersonalizedManifestBuilder(lowEndContext).
		WithVariantLadder(ladder).
		WithOutputFormat(manifest.ManifestOutputFormatHls).
		Build()
	if err != nil {
		panic(err)
	}
	fmt.Println("=== HLS MASTER PLAYLIST (low-end device, poor network) ===")
	fmt.Println(lowEndOutput.Body)

	dashOutput, err := manifest.NewPersonalizedManifestBuilder(context).
		WithVariantLadder(ladder).
		WithOutputFormat(manifest.ManifestOutputFormatDash).
		Build()
	if err != nil {
		panic(err)
	}
	fmt.Println("=== DASH MPD (same context, truncated to first two Periods for README brevity) ===")
	fmt.Println(dashOutput.Body)
}
