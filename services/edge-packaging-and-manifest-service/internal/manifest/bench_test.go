package manifest

import "testing"

// BenchmarkPersonalizedHlsManifestGeneration measures end-to-end
// personalized HLS master-playlist generation latency (device-capability
// filtering + SSAI splicing + DRM resolution + rendering), the basis for
// this service's documented "personalized manifest generation" latency
// target.
func BenchmarkPersonalizedHlsManifestGeneration(b *testing.B) {
	ladder := buildTestLadder()
	context := basePlaybackContext()
	context.PreferredDrmSystem = "widevine"

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := NewPersonalizedManifestBuilder(context).
			WithVariantLadder(ladder).
			WithOutputFormat(ManifestOutputFormatHls).
			Build()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPersonalizedDashManifestGeneration is the DASH-format
// equivalent of BenchmarkPersonalizedHlsManifestGeneration.
func BenchmarkPersonalizedDashManifestGeneration(b *testing.B) {
	ladder := buildTestLadder()
	context := basePlaybackContext()
	context.PreferredDrmSystem = "widevine"

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := NewPersonalizedManifestBuilder(context).
			WithVariantLadder(ladder).
			WithOutputFormat(ManifestOutputFormatDash).
			Build()
		if err != nil {
			b.Fatal(err)
		}
	}
}
