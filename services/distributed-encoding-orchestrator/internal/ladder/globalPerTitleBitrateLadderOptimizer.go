// This file extends the per-scene convex-hull optimizer
// (convexHullBitrateLadderOptimizer.go) to whole-title granularity:
// computeGlobalPerTitleBitrateLadder aggregates every scene's
// already-computed per-scene Pareto-optimal hull into a single
// globally-optimal ladder for the ENTIRE title (source asset), following
// the "per-title encode" approach popularized by Netflix's dynamic
// optimizer research (see "Per-Title Encode Optimization", Netflix Tech
// Blog): rather than encoding every scene of a title to the same fixed
// bitrate, each scene is independently encoded to whatever bitrate its
// own rate-distortion curve requires to hit a shared target quality
// floor, and the title-level "bitrate" reported for that ladder rung is
// the duration-weighted average of those per-scene bitrates.
//
// # Problem statement
//
// Given:
//   - A title split into scenes, each with its own duration and its own
//     already-computed per-scene Pareto-optimal (bitrate, VMAF) hull (the
//     output of ComputeParetoOptimalBitrateLadderForScene for that
//     scene).
//   - A target minimum VMAF floor Q that must be met (or, when a scene's
//     hull cannot reach Q at any bitrate, approached as closely as
//     possible) for EVERY scene.
//
// Find, for each scene, the minimum bitrate on that scene's hull that
// achieves VMAF >= Q, and report the whole-title ladder rung as:
//
//	EffectiveBitrateKilobitsPerSecond = sum(bitrate_i * duration_i) / sum(duration_i)
//
// i.e. the duration-weighted average bitrate — equivalently, the total
// number of kilobits the whole title would consume divided by the whole
// title's duration, which is the metric that actually determines
// storage/CDN egress cost, unlike a naive unweighted average across
// scenes of very different lengths.
//
// # Algorithm
//
//  1. For each scene, and each requested quality floor Q, compute the
//     minimum bitrate on that scene's ascending-bitrate, ascending-VMAF
//     hull achieving VMAF >= Q via PIECEWISE-LINEAR INTERPOLATION between
//     the two hull points bracketing Q. This is valid because the upper
//     convex hull, viewed as a function VMAF(bitrate), is concave and
//     monotonically non-decreasing, so linearly interpolating between two
//     adjacent hull vertices strictly UNDER-estimates the true
//     (concave-curve) VMAF at any interpolated bitrate — meaning the
//     bitrate this algorithm reports for a given VMAF floor is always a
//     conservative (>=) requirement, never an over-optimistic one. Given
//     hull points (b0,v0) and (b1,v1) with v0 < Q <= v1:
//
//     t = (Q - v0) / (v1 - v0)
//     bitrate(Q) = b0 + t*(b1 - b0)
//
//  2. Boundary handling: if Q <= the hull's lowest VMAF point, that
//     point's bitrate is already sufficient (using the cheapest available
//     rung is strictly better than or equal to the floor) — return it
//     directly, no interpolation needed. If Q exceeds the hull's highest
//     VMAF point, no encode on this scene's hull can reach Q; return the
//     hull's most expensive (highest-VMAF) point as the best available
//     approximation, and flag the scene as quality-limited (its achieved
//     VMAF will be reported as below the requested floor) rather than
//     silently pretending the floor was met.
//
//  3. Aggregate: multiply each scene's chosen bitrate by its duration to
//     get that scene's total kilobits, sum across all scenes, and divide
//     by total title duration to get the rung's single reported
//     EffectiveBitrateKilobitsPerSecond.
//
// # Worked example (also encoded as
// TestComputeGlobalPerTitleBitrateLadder_TwoSceneHandComputedFixture)
//
// Title with two scenes:
//
//   - Scene 0, duration 10s, hull: (1000kbps, 80 VMAF), (3000kbps, 95 VMAF).
//   - Scene 1, duration 30s, hull: (1000kbps, 90 VMAF), (2000kbps, 96 VMAF).
//
// Target quality floor Q = 93:
//
//   - Scene 0: 80 < 93 <= 95, bracket is (1000,80)-(3000,95).
//     t = (93-80)/(95-80) = 13/15 ≈ 0.86667
//     bitrate = 1000 + 0.86667*(3000-1000) = 1000 + 1733.33 = 2733.33 kbps
//   - Scene 1: 90 < 93 <= 96, bracket is (1000,90)-(2000,96).
//     t = (93-90)/(96-90) = 3/6 = 0.5
//     bitrate = 1000 + 0.5*(2000-1000) = 1500 kbps
//
// Weighted aggregation:
//
//	totalKilobits  = 2733.33*10 + 1500*30 = 27333.33 + 45000 = 72333.33
//	totalDuration  = 10 + 30 = 40s
//	effectiveRate  = 72333.33 / 40 = 1808.33 kbps
//
// Notice this (1808.33 kbps) is meaningfully LOWER than what a naive
// fixed-bitrate-for-the-whole-title approach would need to pick to
// guarantee every scene hits VMAF>=93 (which would have to use scene 0's
// higher per-scene requirement, 2733.33 kbps, for the ENTIRE title,
// wasting bits on scene 1 which needed far less) — this is exactly the
// saving per-title/per-shot encoding captures in production.
package ladder

import "fmt"

// SceneHullWithDuration is the per-scene input to
// ComputeGlobalPerTitleBitrateLadder: one scene's duration paired with
// its already-computed per-scene Pareto-optimal bitrate ladder (the
// output of ComputeParetoOptimalBitrateLadderForScene for that scene).
type SceneHullWithDuration struct {
	// SceneIndex identifies the scene, matching
	// domain.SceneBoundary.SceneIndex.
	SceneIndex int
	// DurationSeconds is the scene's wall-clock duration; used as the
	// aggregation weight. Must be > 0 to contribute meaningfully to the
	// weighted average (a zero-duration scene is skipped with weight 0,
	// which is mathematically correct — it cannot affect a
	// duration-weighted average — but is also almost certainly a data
	// error worth flagging upstream).
	DurationSeconds float64
	// HullRungs is the scene's Pareto-optimal (bitrate, VMAF) hull in
	// ascending-bitrate order, as returned by
	// ComputeParetoOptimalBitrateLadderForScene. Must contain at least
	// one rung to contribute a bitrate to the aggregate.
	HullRungs []ParetoOptimalBitrateLadderRung
}

// PerSceneLadderDecision records, for one scene, the bitrate the global
// per-title algorithm selected to meet (or best-approach) a requested
// VMAF floor, and whether that floor was actually achieved.
type PerSceneLadderDecision struct {
	SceneIndex          int
	SelectedBitrateKbps float64
	AchievedVmaf        float64
	QualityFloorMet     bool
	DurationSeconds     float64
}

// GlobalPerTitleLadderRung is one rung of the aggregated whole-title
// ladder produced by ComputeGlobalPerTitleBitrateLadder for a single
// requested VMAF quality floor.
type GlobalPerTitleLadderRung struct {
	// TargetVmafFloor is the requested minimum VMAF this rung was solved
	// for.
	TargetVmafFloor float64
	// EffectiveBitrateKilobitsPerSecond is the duration-weighted average
	// bitrate across every scene — see the package/function doc comments
	// for the exact formula and a worked example.
	EffectiveBitrateKilobitsPerSecond float64
	// TotalTitleDurationSeconds is the sum of every scene's
	// DurationSeconds, i.e. the whole title's runtime.
	TotalTitleDurationSeconds float64
	// PerSceneDecisions gives full transparency into how the aggregate
	// was derived: the exact bitrate chosen and VMAF achieved for every
	// individual scene, in the same order as the scenes argument that was
	// passed in.
	PerSceneDecisions []PerSceneLadderDecision
	// AnyScenesQualityLimited is true if at least one scene's hull could
	// not reach TargetVmafFloor at any available bitrate (see the
	// boundary-handling case in the algorithm doc comment) — an operator
	// signal that this rung's effective bitrate under-represents the true
	// cost of guaranteeing the floor everywhere, because at least one
	// scene simply cannot get there with the sampled candidate encodes.
	AnyScenesQualityLimited bool
}

// ComputeGlobalPerTitleBitrateLadder is the primary exported entry point
// of this file: given every scene's per-scene Pareto-optimal hull (with
// duration) and a set of target VMAF quality floors, it returns one
// GlobalPerTitleLadderRung per requested floor, aggregating each scene's
// minimum-bitrate-to-reach-the-floor into a single duration-weighted
// whole-title effective bitrate. See the package doc comment for the full
// algorithm description and a worked numeric example.
//
// Params:
//   - scenes: every scene of the title, each with DurationSeconds > 0 and
//     a non-empty HullRungs. Order does not matter for the aggregate
//     computation but is preserved in each rung's PerSceneDecisions.
//   - targetVmafFloors: the VMAF quality floors to solve a rung for, e.g.
//     []float64{99, 95, 90, 84} for a typical four-rung ladder spanning
//     "reference"-to-"acceptable" quality.
//
// Returns one GlobalPerTitleLadderRung per element of targetVmafFloors,
// in the same order, or an error if scenes is empty or every scene has a
// non-positive duration (which would make the weighted average
// undefined — division by a zero total duration).
//
// This function is pure (no I/O, no shared mutable state) and safe for
// concurrent use across multiple titles.
func ComputeGlobalPerTitleBitrateLadder(scenes []SceneHullWithDuration, targetVmafFloors []float64) ([]GlobalPerTitleLadderRung, error) {
	if len(scenes) == 0 {
		return nil, fmt.Errorf("ladder: computeGlobalPerTitleBitrateLadder requires at least one scene")
	}

	totalDuration := 0.0
	for _, scene := range scenes {
		if scene.DurationSeconds > 0 {
			totalDuration += scene.DurationSeconds
		}
	}
	if totalDuration <= 0 {
		return nil, fmt.Errorf("ladder: computeGlobalPerTitleBitrateLadder requires at least one scene with DurationSeconds > 0")
	}

	rungs := make([]GlobalPerTitleLadderRung, len(targetVmafFloors))
	for rungIndex, floor := range targetVmafFloors {
		rung := GlobalPerTitleLadderRung{
			TargetVmafFloor:           floor,
			TotalTitleDurationSeconds: totalDuration,
			PerSceneDecisions:         make([]PerSceneLadderDecision, 0, len(scenes)),
		}

		totalKilobits := 0.0
		for _, scene := range scenes {
			if scene.DurationSeconds <= 0 || len(scene.HullRungs) == 0 {
				continue
			}

			bitrate, achievedVmaf, met := minimumBitrateToReachVmafFloorOnHull(scene.HullRungs, floor)

			rung.PerSceneDecisions = append(rung.PerSceneDecisions, PerSceneLadderDecision{
				SceneIndex:          scene.SceneIndex,
				SelectedBitrateKbps: bitrate,
				AchievedVmaf:        achievedVmaf,
				QualityFloorMet:     met,
				DurationSeconds:     scene.DurationSeconds,
			})
			if !met {
				rung.AnyScenesQualityLimited = true
			}

			totalKilobits += bitrate * scene.DurationSeconds
		}

		rung.EffectiveBitrateKilobitsPerSecond = totalKilobits / totalDuration
		rungs[rungIndex] = rung
	}

	return rungs, nil
}

// minimumBitrateToReachVmafFloorOnHull finds the minimum bitrate on
// hullRungs (assumed ascending-bitrate, ascending-VMAF, as produced by
// ComputeParetoOptimalBitrateLadderForScene) that achieves
// vmafFloor via linear interpolation between hull vertices. Returns
// (bitrate, achievedVmaf, floorWasMet). See the package doc comment's
// "Algorithm" section for the interpolation formula and boundary-handling
// rules.
func minimumBitrateToReachVmafFloorOnHull(hullRungs []ParetoOptimalBitrateLadderRung, vmafFloor float64) (bitrateKbps float64, achievedVmaf float64, floorMet bool) {
	first := hullRungs[0].SamplePoint
	if vmafFloor <= first.VmafScore {
		// The cheapest available point already meets (or exceeds) the
		// floor.
		return first.BitrateKilobitsPerSecond, first.VmafScore, true
	}

	last := hullRungs[len(hullRungs)-1].SamplePoint
	if vmafFloor > last.VmafScore {
		// No sampled encode on this scene's hull can reach the floor;
		// the best available option is the highest-quality hull point.
		return last.BitrateKilobitsPerSecond, last.VmafScore, false
	}

	for i := 0; i < len(hullRungs)-1; i++ {
		lower := hullRungs[i].SamplePoint
		upper := hullRungs[i+1].SamplePoint
		if vmafFloor > upper.VmafScore {
			continue
		}
		// lower.VmafScore < vmafFloor <= upper.VmafScore (the first loop
		// iteration where this holds, since we already handled
		// vmafFloor <= first.VmafScore above and hull VMAF is
		// monotonically non-decreasing).
		vmafSpan := upper.VmafScore - lower.VmafScore
		if vmafSpan <= 0 {
			// Degenerate (should not occur on a valid hull, since
			// deduplication removes same-bitrate points and the hull
			// construction guarantees strictly increasing VMAF between
			// distinct hull vertices) — fall back to the upper point
			// rather than dividing by zero.
			return upper.BitrateKilobitsPerSecond, upper.VmafScore, true
		}
		t := (vmafFloor - lower.VmafScore) / vmafSpan
		interpolatedBitrate := lower.BitrateKilobitsPerSecond + t*(upper.BitrateKilobitsPerSecond-lower.BitrateKilobitsPerSecond)
		return interpolatedBitrate, vmafFloor, true
	}

	// Unreachable given the boundary checks above, but return the last
	// point defensively rather than a zero value.
	return last.BitrateKilobitsPerSecond, last.VmafScore, true
}
