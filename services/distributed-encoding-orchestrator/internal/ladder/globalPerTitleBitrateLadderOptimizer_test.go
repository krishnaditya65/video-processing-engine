package ladder

import (
	"math"
	"testing"
)

const floatCompareEpsilon = 1e-9

func almostEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= floatCompareEpsilon
}

// twoSceneFixture returns the exact scenario worked out by hand in the
// package doc comment: two scenes with simple two-point hulls chosen so
// the interpolation and weighted-average arithmetic can be verified by
// hand.
func twoSceneFixture() []SceneHullWithDuration {
	return []SceneHullWithDuration{
		{
			SceneIndex:      0,
			DurationSeconds: 10,
			HullRungs: []ParetoOptimalBitrateLadderRung{
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 80}},
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 3000, VmafScore: 95}},
			},
		},
		{
			SceneIndex:      1,
			DurationSeconds: 30,
			HullRungs: []ParetoOptimalBitrateLadderRung{
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 90}},
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 2000, VmafScore: 96}},
			},
		},
	}
}

// TestComputeGlobalPerTitleBitrateLadder_TwoSceneHandComputedFixture
// reproduces, exactly, the worked example from this file's package doc
// comment: two scenes, quality floor 93, hand-computed per-scene
// interpolated bitrates of 2733.333... and 1500, aggregating to an
// effective title bitrate of 1808.333... kbps.
func TestComputeGlobalPerTitleBitrateLadder_TwoSceneHandComputedFixture(t *testing.T) {
	scenes := twoSceneFixture()

	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{93})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rungs) != 1 {
		t.Fatalf("expected 1 rung, got %d", len(rungs))
	}
	rung := rungs[0]

	if len(rung.PerSceneDecisions) != 2 {
		t.Fatalf("expected 2 per-scene decisions, got %d", len(rung.PerSceneDecisions))
	}

	scene0 := rung.PerSceneDecisions[0]
	wantScene0Bitrate := 1000 + (13.0/15.0)*(3000-1000) // == 2733.333...
	if !almostEqual(scene0.SelectedBitrateKbps, wantScene0Bitrate) {
		t.Fatalf("scene 0: expected bitrate≈%v, got %v", wantScene0Bitrate, scene0.SelectedBitrateKbps)
	}
	if !almostEqual(scene0.AchievedVmaf, 93) || !scene0.QualityFloorMet {
		t.Fatalf("scene 0: expected achieved VMAF 93 (floor met), got %v (met=%v)", scene0.AchievedVmaf, scene0.QualityFloorMet)
	}

	scene1 := rung.PerSceneDecisions[1]
	wantScene1Bitrate := 1500.0
	if !almostEqual(scene1.SelectedBitrateKbps, wantScene1Bitrate) {
		t.Fatalf("scene 1: expected bitrate=%v, got %v", wantScene1Bitrate, scene1.SelectedBitrateKbps)
	}

	wantTotalKilobits := wantScene0Bitrate*10 + wantScene1Bitrate*30
	wantEffectiveBitrate := wantTotalKilobits / 40.0 // == 1808.333...
	if !almostEqual(rung.EffectiveBitrateKilobitsPerSecond, wantEffectiveBitrate) {
		t.Fatalf("expected effective bitrate≈%v, got %v", wantEffectiveBitrate, rung.EffectiveBitrateKilobitsPerSecond)
	}
	if rung.TotalTitleDurationSeconds != 40 {
		t.Fatalf("expected TotalTitleDurationSeconds=40, got %v", rung.TotalTitleDurationSeconds)
	}
	if rung.AnyScenesQualityLimited {
		t.Fatal("expected no scenes to be quality-limited for a floor of 93 (both hulls reach 93)")
	}
}

// TestComputeGlobalPerTitleBitrateLadder_LowerThanCheapestPointUsesCheapestPoint
// verifies the boundary case where the requested floor is at or below the
// hull's cheapest point: no interpolation is needed, that point is used
// directly.
func TestComputeGlobalPerTitleBitrateLadder_LowerThanCheapestPointUsesCheapestPoint(t *testing.T) {
	scenes := twoSceneFixture()

	// Scene 0's cheapest point is (1000kbps, 80 VMAF); a floor of 70 is
	// already met by that point.
	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{70})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	scene0 := rungs[0].PerSceneDecisions[0]
	if scene0.SelectedBitrateKbps != 1000 {
		t.Fatalf("expected scene 0 to use its cheapest point (1000kbps) for a floor below it, got %v", scene0.SelectedBitrateKbps)
	}
	if scene0.AchievedVmaf != 80 {
		t.Fatalf("expected achieved VMAF to be the cheapest point's actual VMAF (80), got %v", scene0.AchievedVmaf)
	}
	if !scene0.QualityFloorMet {
		t.Fatal("expected QualityFloorMet=true when the floor is already exceeded by the cheapest point")
	}
}

// TestComputeGlobalPerTitleBitrateLadder_UnreachableFloorFlagsQualityLimited
// verifies the boundary case where a scene's hull cannot reach the
// requested floor at any bitrate: the highest-quality point is used and
// AnyScenesQualityLimited is set.
func TestComputeGlobalPerTitleBitrateLadder_UnreachableFloorFlagsQualityLimited(t *testing.T) {
	scenes := twoSceneFixture()

	// Scene 0's best point tops out at 95 VMAF; a floor of 99 cannot be reached.
	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rung := rungs[0]
	if !rung.AnyScenesQualityLimited {
		t.Fatal("expected AnyScenesQualityLimited=true for an unreachable floor")
	}

	scene0 := rung.PerSceneDecisions[0]
	if scene0.SelectedBitrateKbps != 3000 || scene0.AchievedVmaf != 95 {
		t.Fatalf("expected scene 0 to fall back to its best available point (3000kbps, 95 VMAF), got bitrate=%v vmaf=%v",
			scene0.SelectedBitrateKbps, scene0.AchievedVmaf)
	}
	if scene0.QualityFloorMet {
		t.Fatal("expected QualityFloorMet=false for scene 0 (99 VMAF unreachable)")
	}

	// Scene 1 CAN reach 99? Its best point is 96 VMAF, so it also cannot.
	scene1 := rung.PerSceneDecisions[1]
	if scene1.QualityFloorMet {
		t.Fatal("expected QualityFloorMet=false for scene 1 too (99 VMAF unreachable, best is 96)")
	}
}

// TestComputeGlobalPerTitleBitrateLadder_MultipleFloorsProduceMonotonicallyIncreasingBitrates
// verifies a sanity property that must hold for any correct
// implementation: a higher quality floor can never produce a LOWER
// effective bitrate than a lower floor (since minimum-bitrate-to-reach-Q
// is non-decreasing in Q on a concave, monotonic hull).
func TestComputeGlobalPerTitleBitrateLadder_MultipleFloorsProduceMonotonicallyIncreasingBitrates(t *testing.T) {
	scenes := twoSceneFixture()
	floors := []float64{84, 90, 93, 95}

	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, floors)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rungs) != len(floors) {
		t.Fatalf("expected %d rungs, got %d", len(floors), len(rungs))
	}

	for i := 1; i < len(rungs); i++ {
		if rungs[i].EffectiveBitrateKilobitsPerSecond < rungs[i-1].EffectiveBitrateKilobitsPerSecond-floatCompareEpsilon {
			t.Fatalf("expected non-decreasing effective bitrate as floor increases: rung %d (floor=%v)=%v < rung %d (floor=%v)=%v",
				i, rungs[i].TargetVmafFloor, rungs[i].EffectiveBitrateKilobitsPerSecond,
				i-1, rungs[i-1].TargetVmafFloor, rungs[i-1].EffectiveBitrateKilobitsPerSecond)
		}
		if rungs[i].TargetVmafFloor != floors[i] {
			t.Fatalf("expected rung %d TargetVmafFloor=%v, got %v", i, floors[i], rungs[i].TargetVmafFloor)
		}
	}
}

// TestComputeGlobalPerTitleBitrateLadder_SingleSceneEqualsItsOwnPerSceneBitrate
// verifies the degenerate one-scene case: the whole-title effective
// bitrate must exactly equal that single scene's own selected bitrate
// (the duration-weighted average of one term is that term itself).
func TestComputeGlobalPerTitleBitrateLadder_SingleSceneEqualsItsOwnPerSceneBitrate(t *testing.T) {
	scenes := []SceneHullWithDuration{
		{
			SceneIndex:      0,
			DurationSeconds: 5,
			HullRungs: []ParetoOptimalBitrateLadderRung{
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 500, VmafScore: 60}},
				{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 4000, VmafScore: 98}},
			},
		},
	}
	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{79})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantBitrate := 500 + (19.0/38.0)*(4000-500) // t=(79-60)/(98-60)=19/38=0.5 -> 500+0.5*3500=2250
	if !almostEqual(rungs[0].EffectiveBitrateKilobitsPerSecond, wantBitrate) {
		t.Fatalf("expected effective bitrate≈%v, got %v", wantBitrate, rungs[0].EffectiveBitrateKilobitsPerSecond)
	}
}

// TestComputeGlobalPerTitleBitrateLadder_EmptyScenesReturnsError verifies
// the documented error contract for an empty scenes slice.
func TestComputeGlobalPerTitleBitrateLadder_EmptyScenesReturnsError(t *testing.T) {
	if _, err := ComputeGlobalPerTitleBitrateLadder(nil, []float64{90}); err == nil {
		t.Fatal("expected an error for an empty scenes slice")
	}
}

// TestComputeGlobalPerTitleBitrateLadder_AllZeroDurationScenesReturnsError
// verifies the documented error contract when every scene has a
// non-positive duration (the weighted-average denominator would be zero).
func TestComputeGlobalPerTitleBitrateLadder_AllZeroDurationScenesReturnsError(t *testing.T) {
	scenes := []SceneHullWithDuration{
		{SceneIndex: 0, DurationSeconds: 0, HullRungs: []ParetoOptimalBitrateLadderRung{{SamplePoint: RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 90}}}},
	}
	if _, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{90}); err == nil {
		t.Fatal("expected an error when every scene has non-positive duration")
	}
}

// TestComputeGlobalPerTitleBitrateLadder_IntegratesWithPerSceneConvexHull
// proves the whole per-scene-hull -> whole-title aggregation pipeline
// works end to end starting from raw RateDistortionSamplePoint scatter
// data run through ComputeParetoOptimalBitrateLadderForScene, exactly as
// a real caller would use it (rather than hand-constructing
// SceneHullWithDuration.HullRungs directly as the other tests in this
// file do for arithmetic clarity).
func TestComputeGlobalPerTitleBitrateLadder_IntegratesWithPerSceneConvexHull(t *testing.T) {
	scene0Samples := []RateDistortionSamplePoint{
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 8000, VmafScore: 98.0},
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 5000, VmafScore: 95.0},
		{ResolutionLabel: "720p", BitrateKilobitsPerSecond: 3000, VmafScore: 90.0},
		{ResolutionLabel: "480p", BitrateKilobitsPerSecond: 1200, VmafScore: 74.0},
	}
	scene1Samples := []RateDistortionSamplePoint{
		{ResolutionLabel: "1080p", BitrateKilobitsPerSecond: 6000, VmafScore: 97.0},
		{ResolutionLabel: "720p", BitrateKilobitsPerSecond: 2500, VmafScore: 88.0},
	}

	scenes := []SceneHullWithDuration{
		{SceneIndex: 0, DurationSeconds: 20, HullRungs: ComputeParetoOptimalBitrateLadderForScene(scene0Samples)},
		{SceneIndex: 1, DurationSeconds: 20, HullRungs: ComputeParetoOptimalBitrateLadderForScene(scene1Samples)},
	}

	rungs, err := ComputeGlobalPerTitleBitrateLadder(scenes, []float64{92})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rung := rungs[0]

	// Sanity: the aggregate must lie between the cheapest and most
	// expensive per-scene selected bitrates (a weighted average is always
	// within the range of its terms).
	minSelected, maxSelected := math.Inf(1), math.Inf(-1)
	for _, decision := range rung.PerSceneDecisions {
		if decision.SelectedBitrateKbps < minSelected {
			minSelected = decision.SelectedBitrateKbps
		}
		if decision.SelectedBitrateKbps > maxSelected {
			maxSelected = decision.SelectedBitrateKbps
		}
	}
	if rung.EffectiveBitrateKilobitsPerSecond < minSelected-floatCompareEpsilon || rung.EffectiveBitrateKilobitsPerSecond > maxSelected+floatCompareEpsilon {
		t.Fatalf("expected effective bitrate %v within [%v, %v]", rung.EffectiveBitrateKilobitsPerSecond, minSelected, maxSelected)
	}
}
