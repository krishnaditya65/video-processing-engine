package ladder

import (
	"math"
	"testing"
)

// approximatelyEqual reports whether a and b differ by no more than a
// small epsilon, tolerating floating point rounding in test assertions.
func approximatelyEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// TestComputeUpperConvexHullFromRateDistortionSamples_HandComputedTriangleFixture
// uses a minimal, hand-verifiable fixture: three points forming a
// triangle where the middle point is strictly above the chord between
// the outer two, so all three must survive on the hull.
func TestComputeUpperConvexHullFromRateDistortionSamples_HandComputedTriangleFixture(t *testing.T) {
	samples := []RateDistortionSamplePoint{
		{BitrateKilobitsPerSecond: 0, VmafScore: 0, ResolutionLabel: "A"},
		{BitrateKilobitsPerSecond: 1, VmafScore: 2, ResolutionLabel: "B"}, // above chord (0,0)-(2,0)
		{BitrateKilobitsPerSecond: 2, VmafScore: 0, ResolutionLabel: "C"},
	}
	hull := computeUpperConvexHullFromRateDistortionSamples(samples)
	if len(hull) != 3 {
		t.Fatalf("expected all 3 points on hull (triangle peak), got %d: %v", len(hull), hull)
	}
	if hull[0].ResolutionLabel != "A" || hull[1].ResolutionLabel != "B" || hull[2].ResolutionLabel != "C" {
		t.Fatalf("unexpected hull order: %v", hull)
	}
}

// TestComputeUpperConvexHullFromRateDistortionSamples_DominatedInteriorPointIsExcluded
// hand-computes a case where a candidate point lies BELOW the chord
// connecting its neighbors and must therefore be excluded from the
// upper hull (it is dominated: for its bitrate, the chord already
// implies achievable-or-better quality via interpolation between two
// hull points).
func TestComputeUpperConvexHullFromRateDistortionSamples_DominatedInteriorPointIsExcluded(t *testing.T) {
	samples := []RateDistortionSamplePoint{
		{BitrateKilobitsPerSecond: 0, VmafScore: 0, ResolutionLabel: "left"},
		{BitrateKilobitsPerSecond: 1, VmafScore: -5, ResolutionLabel: "dip"}, // strictly below chord (0,0)-(2,0)
		{BitrateKilobitsPerSecond: 2, VmafScore: 0, ResolutionLabel: "right"},
	}
	hull := computeUpperConvexHullFromRateDistortionSamples(samples)
	if len(hull) != 2 {
		t.Fatalf("expected dip point excluded, hull len=2, got %d: %v", len(hull), hull)
	}
	for _, p := range hull {
		if p.ResolutionLabel == "dip" {
			t.Fatalf("dominated 'dip' point should not be on hull: %v", hull)
		}
	}
}

// TestComputeUpperConvexHullFromRateDistortionSamples_NetflixStyleCrossResolutionFixture
// is a realistic hand-checkable fixture spanning three resolutions,
// modeling the classic "resolution cross-over" scenario: at low
// bitrates 480p beats 720p, but 720p and 1080p dominate at higher
// bitrates. The point (720p, 1200kbps, VMAF 70) is dominated by the
// chord between (480p,1200,74) sharing the same bitrate and a
// higher-VMAF sample, and must be excluded via the
// same-bitrate-dedup + hull logic; separately (480p, 600kbps, 55) must
// survive as the cheapest hull point, and the mid VMAF point at
// 2000kbps/84 on 720p must survive as it bulges above the chord from
// (1200,74) to (3000,90).
func TestComputeUpperConvexHullFromRateDistortionSamples_NetflixStyleCrossResolutionFixture(t *testing.T) {
	samples := []RateDistortionSamplePoint{
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 8000, VmafScore: 98.0},
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 5000, VmafScore: 95.0},
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 3000, VmafScore: 88.0},
		{ResolutionLabel: "720p", BitrateKilobitsPerSecond: 3000, VmafScore: 90.0}, // beats 1080p at 3000kbps
		{ResolutionLabel: "720p", BitrateKilobitsPerSecond: 2000, VmafScore: 84.0},
		{ResolutionLabel: "720p", BitrateKilobitsPerSecond: 1200, VmafScore: 70.0}, // dominated: see below
		{ResolutionLabel: "480p", BitrateKilobitsPerSecond: 1200, VmafScore: 74.0}, // beats 720p at 1200kbps
		{ResolutionLabel: "480p", BitrateKilobitsPerSecond: 600, VmafScore: 55.0},
	}

	hull := computeUpperConvexHullFromRateDistortionSamples(samples)

	hullBitrates := make(map[float64]RateDistortionSamplePoint)
	for _, p := range hull {
		hullBitrates[p.BitrateKilobitsPerSecond] = p
	}

	// 1200kbps rung must be the 480p point (74 VMAF), not the dominated 720p (70 VMAF).
	pointAt1200, ok := hullBitrates[1200]
	if !ok {
		t.Fatalf("expected a hull point at 1200kbps, hull=%v", hull)
	}
	if pointAt1200.ResolutionLabel != "480p" {
		t.Fatalf("expected 480p to win at 1200kbps (dedup keeps higher VMAF), got %s", pointAt1200.ResolutionLabel)
	}

	// The cheapest point (600kbps/55 VMAF/480p) must always be on the hull:
	// as the leftmost point, it trivially cannot be dominated by anything
	// to its right in an upper-hull construction restricted to points at
	// bitrate >= its own.
	if _, ok := hullBitrates[600]; !ok {
		t.Fatalf("expected leftmost point (600kbps) to always survive on hull, hull=%v", hull)
	}

	// The most expensive point (8000kbps/98 VMAF/1080p) must always be on
	// the hull for the symmetric reason (rightmost point).
	if _, ok := hullBitrates[8000]; !ok {
		t.Fatalf("expected rightmost point (8000kbps) to always survive on hull, hull=%v", hull)
	}

	// Verify ascending bitrate order of the returned hull.
	for i := 1; i < len(hull); i++ {
		if hull[i].BitrateKilobitsPerSecond <= hull[i-1].BitrateKilobitsPerSecond {
			t.Fatalf("hull not strictly ascending in bitrate at index %d: %v", i, hull)
		}
	}
}

// TestComputeUpperConvexHullFromRateDistortionSamples_EmptyInputReturnsEmptySlice
// verifies the documented empty-input contract.
func TestComputeUpperConvexHullFromRateDistortionSamples_EmptyInputReturnsEmptySlice(t *testing.T) {
	hull := computeUpperConvexHullFromRateDistortionSamples(nil)
	if hull == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(hull) != 0 {
		t.Fatalf("expected empty hull, got %v", hull)
	}
}

// TestComputeUpperConvexHullFromRateDistortionSamples_SinglePointDegenerateHull
// verifies a single input point is returned unchanged as a
// one-point hull.
func TestComputeUpperConvexHullFromRateDistortionSamples_SinglePointDegenerateHull(t *testing.T) {
	only := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 80, ResolutionLabel: "solo"}
	hull := computeUpperConvexHullFromRateDistortionSamples([]RateDistortionSamplePoint{only})
	if len(hull) != 1 || hull[0] != only {
		t.Fatalf("expected single-point hull equal to input, got %v", hull)
	}
}

// TestComputeUpperConvexHullFromRateDistortionSamples_CollinearPointsKeepOnlyEndpoints
// verifies that three exactly-collinear points reduce to just the two
// endpoints, since the middle point offers no marginal quality gain
// over interpolating the chord and is treated as redundant/dominated.
func TestComputeUpperConvexHullFromRateDistortionSamples_CollinearPointsKeepOnlyEndpoints(t *testing.T) {
	samples := []RateDistortionSamplePoint{
		{BitrateKilobitsPerSecond: 0, VmafScore: 0, ResolutionLabel: "start"},
		{BitrateKilobitsPerSecond: 1, VmafScore: 1, ResolutionLabel: "mid"}, // exactly on the line y=x
		{BitrateKilobitsPerSecond: 2, VmafScore: 2, ResolutionLabel: "end"},
	}
	hull := computeUpperConvexHullFromRateDistortionSamples(samples)
	if len(hull) != 2 {
		t.Fatalf("expected collinear midpoint dropped, got %d points: %v", len(hull), hull)
	}
	if hull[0].ResolutionLabel != "start" || hull[1].ResolutionLabel != "end" {
		t.Fatalf("expected [start, end], got %v", hull)
	}
}

// TestComputeParetoOptimalBitrateLadderForScene_MarginalGainComputation
// hand-verifies the marginal-VMAF-per-kilobit annotation on a simple
// two-rung hull: (1000kbps, 80 VMAF) -> (2000kbps, 90 VMAF) has a
// marginal gain of (90-80)/(2000-1000) = 0.01 VMAF per kbps.
func TestComputeParetoOptimalBitrateLadderForScene_MarginalGainComputation(t *testing.T) {
	samples := []RateDistortionSamplePoint{
		{BitrateKilobitsPerSecond: 1000, VmafScore: 80, ResolutionLabel: "low"},
		{BitrateKilobitsPerSecond: 2000, VmafScore: 90, ResolutionLabel: "high"},
	}
	rungs := ComputeParetoOptimalBitrateLadderForScene(samples)
	if len(rungs) != 2 {
		t.Fatalf("expected 2 rungs, got %d: %v", len(rungs), rungs)
	}
	if rungs[0].MarginalVmafGainPerKilobitOverPreviousRung != 0 {
		t.Fatalf("first rung should have zero marginal gain (no predecessor), got %v", rungs[0].MarginalVmafGainPerKilobitOverPreviousRung)
	}
	if !approximatelyEqual(rungs[1].MarginalVmafGainPerKilobitOverPreviousRung, 0.01) {
		t.Fatalf("expected marginal gain 0.01, got %v", rungs[1].MarginalVmafGainPerKilobitOverPreviousRung)
	}
}

// TestComputeParetoOptimalBitrateLadderForScene_EmptyInput verifies the
// exported entry point also handles empty input gracefully.
func TestComputeParetoOptimalBitrateLadderForScene_EmptyInput(t *testing.T) {
	rungs := ComputeParetoOptimalBitrateLadderForScene(nil)
	if rungs == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(rungs) != 0 {
		t.Fatalf("expected empty result, got %v", rungs)
	}
}

// TestFormsClockwiseTurnMaximizingQualityPerBitrate_KnownCrossProductSigns
// hand-verifies the cross-product sign convention directly against
// geometrically obvious cases.
func TestFormsClockwiseTurnMaximizingQualityPerBitrate_KnownCrossProductSigns(t *testing.T) {
	origin := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 0, VmafScore: 0}
	peak := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1, VmafScore: 2}    // above chord
	valley := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1, VmafScore: -2} // below chord
	onLine := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1, VmafScore: 0}  // collinear
	destination := RateDistortionSamplePoint{BitrateKilobitsPerSecond: 2, VmafScore: 0}

	if !formsClockwiseTurnMaximizingQualityPerBitrate(origin, peak, destination) {
		t.Error("expected peak (above chord) to be a clockwise turn (keep)")
	}
	if formsClockwiseTurnMaximizingQualityPerBitrate(origin, valley, destination) {
		t.Error("expected valley (below chord) to NOT be a clockwise turn (pop)")
	}
	if formsClockwiseTurnMaximizingQualityPerBitrate(origin, onLine, destination) {
		t.Error("expected collinear point to NOT be treated as a clockwise turn (pop, redundant)")
	}
}
