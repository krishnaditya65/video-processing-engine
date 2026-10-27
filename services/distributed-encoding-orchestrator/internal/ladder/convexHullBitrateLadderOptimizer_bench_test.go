package ladder

import (
	"math/rand"
	"testing"
)

// generateRandomRateDistortionSamples produces a synthetic but
// realistic-shaped scatter of (bitrate, VMAF) samples across a handful
// of resolutions, for benchmarking computeUpperConvexHullFromRateDistortionSamples
// at scale (e.g. approximating "every scene of a catalog title sampled
// at many bitrate/resolution combinations").
func generateRandomRateDistortionSamples(count int) []RateDistortionSamplePoint {
	resolutions := []string{"3840x2160", "1920x1080", "1280x720", "854x480", "640x360"}
	rng := rand.New(rand.NewSource(42))
	samples := make([]RateDistortionSamplePoint, count)
	for i := 0; i < count; i++ {
		bitrate := 200 + rng.Float64()*19800 // 200..20000 kbps
		// VMAF roughly rises with log(bitrate), with noise, capped at 100.
		vmaf := 40 + 12*logBase2(bitrate) + rng.NormFloat64()*3
		if vmaf > 100 {
			vmaf = 100
		}
		samples[i] = RateDistortionSamplePoint{
			BitrateKilobitsPerSecond: bitrate,
			VmafScore:                vmaf,
			ResolutionLabel:          resolutions[i%len(resolutions)],
		}
	}
	return samples
}

func logBase2(x float64) float64 {
	// Minimal local log2 to avoid pulling in math.Log2 solely for the
	// benchmark's synthetic data generator (math.Log2 is of course fine
	// too; this just keeps the benchmark file dependency-light/obvious).
	result := 0.0
	for x > 1 {
		x /= 2
		result++
	}
	return result
}

// BenchmarkComputeUpperConvexHullFromRateDistortionSamples measures the
// hull computation's throughput across realistic sample-set sizes, from
// a single scene's typical ladder-search sweep (dozens of samples) up to
// a stress size approximating an aggressive multi-resolution,
// multi-bitrate sweep.
func BenchmarkComputeUpperConvexHullFromRateDistortionSamples(b *testing.B) {
	for _, sampleCount := range []int{10, 100, 1000, 10000} {
		samples := generateRandomRateDistortionSamples(sampleCount)
		b.Run(itoaBench(sampleCount), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = computeUpperConvexHullFromRateDistortionSamples(samples)
			}
		})
	}
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	digits := [20]byte{}
	pos := len(digits)
	for n > 0 {
		pos--
		digits[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[pos:])
}
