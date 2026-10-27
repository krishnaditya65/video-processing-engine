// Package ladder implements the VMAF convex-hull bitrate-ladder optimizer
// used by the distributed encoding orchestrator to pick, for each scene,
// the smallest set of (resolution, bitrate) encoding rungs that are
// Pareto-optimal on the rate-distortion curve — i.e. no other candidate
// encode achieves both a lower (or equal) bitrate AND a higher (or equal)
// VMAF perceptual-quality score.
//
// # Background
//
// For a fixed scene, encoding at many different (resolution, bitrate)
// combinations and measuring VMAF produces a scatter of points in the
// (bitrate, VMAF) plane. Netflix's per-title / per-shot encoding research
// (see "Optimized Shot-Based Encodes: Now Streaming!", Netflix Tech Blog)
// established that the practically useful subset of that scatter is its
// upper convex hull in rate-distortion space: for every bitrate value on
// the hull, no other sampled point delivers higher quality at equal or
// lower cost, and (crucially, unlike a naive "highest VMAF per
// resolution" filter) the hull also excludes points that are dominated by
// a *combination* of two better points connected by a chord with steeper
// marginal quality-per-bit than the excluded point offers.
//
// This package computes that upper convex hull with an adaptation of
// Andrew's monotone chain algorithm, restricted to the upper chain only
// (because we always want to MAXIMIZE VMAF for a given bitrate, never
// minimize it), and ties are broken deterministically so results are
// reproducible across runs on the same input.
package ladder

import (
	"fmt"
	"sort"
)

// RateDistortionSamplePoint is one measured (or predicted) encode result
// for a single scene: the output bitrate in kilobits-per-second, the
// VMAF perceptual quality score in [0, 100] achieved at that bitrate, and
// the resolution that was encoded to produce this sample. Multiple
// resolutions are frequently sampled at overlapping bitrates because a
// lower resolution can sometimes beat a higher resolution's VMAF at very
// low bitrates (the classic "resolution cross-over" phenomenon), which is
// exactly the situation the convex-hull ladder is designed to resolve
// correctly.
type RateDistortionSamplePoint struct {
	// BitrateKilobitsPerSecond is the target/measured encode bitrate.
	// Must be > 0; the hull algorithm treats bitrate as the x-axis.
	BitrateKilobitsPerSecond float64

	// VmafScore is the measured VMAF quality score in the closed
	// interval [0, 100]; the hull algorithm treats it as the y-axis and
	// always maximizes it.
	VmafScore float64

	// ResolutionLabel identifies which resolution produced this sample,
	// e.g. "1920x1080" or "1280x720". Carried through purely as metadata
	// so the caller can reconstruct a usable ladder rung; it plays no
	// role in the hull geometry itself.
	ResolutionLabel string
}

// String renders a RateDistortionSamplePoint as
// "<resolution>@<bitrate>kbps=<vmaf>vmaf" for compact debug/log output.
func (samplePoint RateDistortionSamplePoint) String() string {
	return fmt.Sprintf("%s@%.0fkbps=%.2fvmaf", samplePoint.ResolutionLabel, samplePoint.BitrateKilobitsPerSecond, samplePoint.VmafScore)
}

// computeUpperConvexHullFromRateDistortionSamples computes the
// Pareto-optimal subset of candidateSamplePoints on the upper convex hull
// of the (bitrate, VMAF) rate-distortion plane, using a monotone-chain
// (Andrew's algorithm) sweep restricted to the upper chain.
//
// Algorithm:
//  1. Sort samples by ascending bitrate, breaking ties by descending
//     VMAF (so that, among equal-bitrate samples, the higher-quality one
//     is considered first and lower-quality duplicates at the same
//     bitrate are naturally rejected by the turn test below).
//  2. Sweep left to right maintaining a stack ("hull") of points. Before
//     pushing the next point, pop from the stack while the last two
//     points on the stack plus the candidate point do NOT form a
//     strictly clockwise (right) turn — i.e. while the turn is a left
//     turn or collinear/non-positive cross product for maximization —
//     because that means the middle point of the three lies on or below
//     the chord between its neighbors and is therefore dominated.
//
// This is the textbook monotone-chain construction; the only
// domain-specific adaptation is that we build ONLY the upper chain
// (because higher VMAF is strictly better, so the lower chain of a full
// convex hull — which would represent worst-case quality — is
// meaningless for a bitrate ladder) and the turn direction is chosen so
// the retained chain bulges upward/outward (i.e. is concave when viewed
// from below), which is what "maximizes quality per marginal bit" means
// geometrically.
//
// Returns the ordered (ascending bitrate) slice of Pareto-optimal
// samples. Returns an empty (non-nil) slice if candidateSamplePoints is
// empty. A single input point is always returned as a degenerate
// one-point hull. This function is pure and allocates a fresh result
// slice on every call; it performs no I/O and holds no package-level
// state, so it is trivially safe for concurrent use by multiple
// goroutines with disjoint inputs.
func computeUpperConvexHullFromRateDistortionSamples(candidateSamplePoints []RateDistortionSamplePoint) []RateDistortionSamplePoint {
	if len(candidateSamplePoints) == 0 {
		return []RateDistortionSamplePoint{}
	}

	sortedByAscendingBitrateThenDescendingVmaf := make([]RateDistortionSamplePoint, len(candidateSamplePoints))
	copy(sortedByAscendingBitrateThenDescendingVmaf, candidateSamplePoints)
	sort.SliceStable(sortedByAscendingBitrateThenDescendingVmaf, func(i, j int) bool {
		left := sortedByAscendingBitrateThenDescendingVmaf[i]
		right := sortedByAscendingBitrateThenDescendingVmaf[j]
		if left.BitrateKilobitsPerSecond != right.BitrateKilobitsPerSecond {
			return left.BitrateKilobitsPerSecond < right.BitrateKilobitsPerSecond
		}
		return left.VmafScore > right.VmafScore
	})

	// Deduplicate exact-bitrate ties, keeping only the first (highest
	// VMAF, due to the sort above) sample at each distinct bitrate. A
	// second sample at an identical bitrate can never be on the upper
	// hull once a higher-VMAF sample at that same bitrate exists,
	// because it would require a vertical hull segment, which the
	// cross-product turn test below cannot express and would otherwise
	// admit spuriously.
	deduplicatedByBitrate := sortedByAscendingBitrateThenDescendingVmaf[:0:0]
	for index, samplePoint := range sortedByAscendingBitrateThenDescendingVmaf {
		if index == 0 || samplePoint.BitrateKilobitsPerSecond != sortedByAscendingBitrateThenDescendingVmaf[index-1].BitrateKilobitsPerSecond {
			deduplicatedByBitrate = append(deduplicatedByBitrate, samplePoint)
		}
	}

	upperHullStack := make([]RateDistortionSamplePoint, 0, len(deduplicatedByBitrate))
	for _, candidatePoint := range deduplicatedByBitrate {
		for len(upperHullStack) >= 2 && !formsClockwiseTurnMaximizingQualityPerBitrate(
			upperHullStack[len(upperHullStack)-2],
			upperHullStack[len(upperHullStack)-1],
			candidatePoint,
		) {
			upperHullStack = upperHullStack[:len(upperHullStack)-1]
		}
		upperHullStack = append(upperHullStack, candidatePoint)
	}

	return upperHullStack
}

// formsClockwiseTurnMaximizingQualityPerBitrate reports whether the three
// points (origin, middle, destination), taken in order, turn clockwise in
// the (bitrate, VMAF) plane — equivalently, whether "middle" lies
// strictly above the straight chord from "origin" to "destination". Such
// a point is a valid, non-dominated vertex of the upper convex hull.
//
// This is computed via the sign of the 2D cross product of vectors
// (middle - origin) and (destination - origin):
//
//	cross = (middle.x - origin.x)*(destination.y - origin.y)
//	      - (middle.y - origin.y)*(destination.x - origin.x)
//
// cross < 0 means a clockwise turn (middle is above the chord: keep it).
// cross > 0 means a counter-clockwise turn (middle is below the chord:
// it is dominated by the chord and must be popped).
// cross == 0 means the three points are collinear; we treat collinear
// middle points as dominated (redundant) and pop them, which keeps the
// hull minimal by excluding points that lie exactly on a chord between
// two other kept points.
func formsClockwiseTurnMaximizingQualityPerBitrate(origin, middle, destination RateDistortionSamplePoint) bool {
	crossProduct := (middle.BitrateKilobitsPerSecond-origin.BitrateKilobitsPerSecond)*(destination.VmafScore-origin.VmafScore) -
		(middle.VmafScore-origin.VmafScore)*(destination.BitrateKilobitsPerSecond-origin.BitrateKilobitsPerSecond)
	return crossProduct < 0
}

// ParetoOptimalBitrateLadderRung is one surviving rung of a per-scene
// bitrate ladder after convex-hull optimization: a resolution/bitrate
// pairing that is not dominated by any other candidate encode, together
// with the marginal VMAF gained per additional kilobit relative to the
// previous (lower-bitrate) rung on the hull, which is useful for
// operators deciding where a ladder's diminishing returns make further
// rungs not worth the storage/CDN cost.
type ParetoOptimalBitrateLadderRung struct {
	// SamplePoint is the underlying rate-distortion sample this rung was
	// derived from.
	SamplePoint RateDistortionSamplePoint

	// MarginalVmafGainPerKilobitOverPreviousRung is
	// (thisVmaf-prevVmaf)/(thisBitrate-prevBitrate) relative to the
	// previous rung in the returned ladder (ascending bitrate order), or
	// 0 for the first rung, which has no predecessor.
	MarginalVmafGainPerKilobitOverPreviousRung float64
}

// ComputeParetoOptimalBitrateLadderForScene is the primary exported entry
// point of this package. Given all measured/predicted rate-distortion
// samples for a single scene (potentially spanning many resolutions and
// bitrates), it returns the Pareto-optimal bitrate ladder: the ordered
// (ascending bitrate), deduplicated subset of samples that lie on the
// upper convex hull of the rate-distortion curve, each annotated with its
// marginal quality-per-bit gain over the previous rung.
//
// Params:
//   - sceneRateDistortionSamples: unordered slice of every candidate
//     (resolution, bitrate) encode result measured for one scene. Must
//     contain at least one element for a non-empty result.
//
// Returns the ladder rungs in ascending-bitrate order. Returns an empty,
// non-nil slice if sceneRateDistortionSamples is empty.
//
// This function is pure (no I/O, no shared mutable state) and therefore
// safe to call concurrently from many worker goroutines processing
// different scenes at once, including from within the WorkerPool's
// per-task goroutines.
func ComputeParetoOptimalBitrateLadderForScene(sceneRateDistortionSamples []RateDistortionSamplePoint) []ParetoOptimalBitrateLadderRung {
	hullPoints := computeUpperConvexHullFromRateDistortionSamples(sceneRateDistortionSamples)

	ladderRungs := make([]ParetoOptimalBitrateLadderRung, len(hullPoints))
	for index, point := range hullPoints {
		rung := ParetoOptimalBitrateLadderRung{SamplePoint: point}
		if index > 0 {
			previous := hullPoints[index-1]
			bitrateDelta := point.BitrateKilobitsPerSecond - previous.BitrateKilobitsPerSecond
			if bitrateDelta > 0 {
				rung.MarginalVmafGainPerKilobitOverPreviousRung = (point.VmafScore - previous.VmafScore) / bitrateDelta
			}
		}
		ladderRungs[index] = rung
	}
	return ladderRungs
}
