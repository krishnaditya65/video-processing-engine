"""Unit tests for the scene-cut detection strategies and adaptive threshold logic.

All tests operate on small synthetic NumPy frame arrays -- no real
video file or OpenCV video decode is required to exercise the actual
histogram-difference and SSIM math.
"""

from __future__ import annotations

import numpy as np
import pytest

from ingestion_inspection_service.config import SceneCutDetectionThresholds
from ingestion_inspection_service.strategies.scene_detection import (
    HistogramDifferenceSceneDetectionStrategy,
    StructuralSimilaritySceneDetectionStrategy,
    compute_motion_complexity_score,
    detect_scene_boundaries_via_adaptive_thresholding,
)
from tests.conftest import make_checkerboard_frame, make_solid_color_frame


class TestHistogramDifferenceSceneDetectionStrategy:
    """Tests for HistogramDifferenceSceneDetectionStrategy."""

    def test_identical_frames_have_zero_dissimilarity(self) -> None:
        """Comparing a frame to an exact copy of itself yields ~0 dissimilarity."""
        strategy = HistogramDifferenceSceneDetectionStrategy()
        frame = make_solid_color_frame(64, 64, (100, 120, 140))

        dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame, frame.copy()
        )

        assert dissimilarity == pytest.approx(0.0, abs=1e-6)

    def test_drastically_different_frames_have_high_dissimilarity(self) -> None:
        """Two unrelated high-detail frames score far more dissimilar than identical ones."""
        strategy = HistogramDifferenceSceneDetectionStrategy()
        frame_a = make_checkerboard_frame(64, 64, 8, seed=1)
        frame_b = make_checkerboard_frame(64, 64, 8, seed=99)

        identical_dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, frame_a.copy()
        )
        different_dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, frame_b
        )

        assert identical_dissimilarity == pytest.approx(0.0, abs=1e-6)
        assert different_dissimilarity > 0.4
        assert different_dissimilarity > identical_dissimilarity

    def test_mismatched_frame_shapes_raise_value_error(self) -> None:
        """Comparing frames of different shapes raises ValueError."""
        strategy = HistogramDifferenceSceneDetectionStrategy()
        small_frame = make_solid_color_frame(32, 32, (10, 10, 10))
        large_frame = make_solid_color_frame(64, 64, (10, 10, 10))

        with pytest.raises(ValueError):
            strategy.compute_frame_pair_dissimilarity_score(small_frame, large_frame)

    def test_strategy_name_is_stable_identifier(self) -> None:
        """strategy_name returns the documented stable identifier string."""
        strategy = HistogramDifferenceSceneDetectionStrategy()
        assert strategy.strategy_name == "histogram_difference"


class TestStructuralSimilaritySceneDetectionStrategy:
    """Tests for StructuralSimilaritySceneDetectionStrategy (SSIM)."""

    def test_identical_frames_have_near_zero_dissimilarity(self) -> None:
        """SSIM(x, x) is ~1.0, so dissimilarity should be ~0.0."""
        strategy = StructuralSimilaritySceneDetectionStrategy()
        frame = make_checkerboard_frame(64, 64, 8, seed=1)

        dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame, frame.copy()
        )

        assert dissimilarity == pytest.approx(0.0, abs=1e-6)

    def test_structurally_different_frames_have_high_dissimilarity(self) -> None:
        """Two frames with unrelated random structure score highly dissimilar."""
        strategy = StructuralSimilaritySceneDetectionStrategy()
        frame_a = make_checkerboard_frame(64, 64, 8, seed=1)
        frame_b = make_checkerboard_frame(64, 64, 8, seed=99)

        dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, frame_b
        )

        assert dissimilarity > 0.5

    def test_slightly_shifted_frame_has_intermediate_dissimilarity(self) -> None:
        """A small structural change scores between identical and unrelated frames."""
        strategy = StructuralSimilaritySceneDetectionStrategy()
        frame_a = make_checkerboard_frame(64, 64, 8, seed=1)
        # Small perturbation: add mild uniform noise rather than a full
        # scene replacement.
        rng = np.random.default_rng(2)
        noise = rng.integers(-10, 10, size=frame_a.shape, dtype=np.int16)
        frame_a_perturbed = np.clip(
            frame_a.astype(np.int16) + noise, 0, 255
        ).astype(np.uint8)

        identical_dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, frame_a.copy()
        )
        perturbed_dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, frame_a_perturbed
        )
        unrelated_dissimilarity = strategy.compute_frame_pair_dissimilarity_score(
            frame_a, make_checkerboard_frame(64, 64, 8, seed=99)
        )

        assert identical_dissimilarity < perturbed_dissimilarity < unrelated_dissimilarity

    def test_mismatched_frame_shapes_raise_value_error(self) -> None:
        """Comparing frames of different shapes raises ValueError."""
        strategy = StructuralSimilaritySceneDetectionStrategy()
        small_frame = make_solid_color_frame(32, 32, (10, 10, 10))
        large_frame = make_solid_color_frame(64, 64, (10, 10, 10))

        with pytest.raises(ValueError):
            strategy.compute_frame_pair_dissimilarity_score(small_frame, large_frame)

    def test_strategy_name_is_stable_identifier(self) -> None:
        """strategy_name returns the documented stable identifier string."""
        strategy = StructuralSimilaritySceneDetectionStrategy()
        assert strategy.strategy_name == "structural_similarity"


class TestComputeMotionComplexityScore:
    """Tests for compute_motion_complexity_score."""

    def test_flat_solid_frame_has_low_complexity(self) -> None:
        """A solid-color frame (no edges, zero variance) scores near zero."""
        frame = make_solid_color_frame(64, 64, (128, 128, 128))
        score = compute_motion_complexity_score(frame)
        assert score == pytest.approx(0.0, abs=1e-3)

    def test_high_detail_frame_has_higher_complexity_than_flat_frame(self) -> None:
        """A checkerboard pattern scores higher complexity than a flat frame."""
        flat_frame = make_solid_color_frame(64, 64, (128, 128, 128))
        detailed_frame = make_checkerboard_frame(64, 64, 4, seed=7)

        flat_score = compute_motion_complexity_score(flat_frame)
        detailed_score = compute_motion_complexity_score(detailed_frame)

        assert detailed_score > flat_score

    def test_non_three_channel_frame_raises_value_error(self) -> None:
        """A grayscale (2-D) frame array raises ValueError."""
        grayscale_frame = np.zeros((64, 64), dtype=np.uint8)
        with pytest.raises(ValueError):
            compute_motion_complexity_score(grayscale_frame)


class TestDetectSceneBoundariesViaAdaptiveThresholding:
    """Tests for detect_scene_boundaries_via_adaptive_thresholding."""

    def test_no_cuts_detected_yields_single_full_duration_scene(self) -> None:
        """Uniformly low dissimilarity scores yield exactly one full-length scene."""
        timestamps = [float(i) for i in range(20)]
        dissimilarity_scores = [0.01] * (len(timestamps) - 1)
        complexity_scores = [0.1] * len(timestamps)

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexity_scores,
            detection_strategy_name="histogram_difference",
            fixed_cut_dissimilarity_threshold=0.5,
            asset_duration_seconds=20.0,
        )

        assert len(boundaries) == 1
        assert boundaries[0].start_timestamp_seconds == pytest.approx(0.0)
        assert boundaries[0].end_timestamp_seconds == pytest.approx(20.0)

    def test_single_spike_produces_two_scenes_at_expected_boundary(self) -> None:
        """A single dissimilarity spike above threshold produces a 2-scene split."""
        timestamps = [float(i) for i in range(20)]
        dissimilarity_scores = [0.02] * (len(timestamps) - 1)
        # Inject an obvious cut between sample index 9 and 10.
        spike_index = 9
        dissimilarity_scores[spike_index] = 0.95
        complexity_scores = [0.1] * len(timestamps)

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexity_scores,
            detection_strategy_name="histogram_difference",
            fixed_cut_dissimilarity_threshold=0.5,
            asset_duration_seconds=20.0,
        )

        assert len(boundaries) == 2
        assert boundaries[0].end_timestamp_seconds == pytest.approx(
            timestamps[spike_index + 1]
        )
        assert boundaries[1].start_timestamp_seconds == pytest.approx(
            timestamps[spike_index + 1]
        )
        # Scenes must be contiguous and cover the full duration.
        assert boundaries[0].start_timestamp_seconds == pytest.approx(0.0)
        assert boundaries[1].end_timestamp_seconds == pytest.approx(20.0)

    def test_multiple_well_separated_spikes_produce_multiple_scenes(self) -> None:
        """Two well-separated spikes above the minimum scene duration split into 3 scenes."""
        timestamps = [float(i) for i in range(30)]
        dissimilarity_scores = [0.02] * (len(timestamps) - 1)
        dissimilarity_scores[9] = 0.9
        dissimilarity_scores[19] = 0.9
        complexity_scores = [0.1] * len(timestamps)

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexity_scores,
            detection_strategy_name="structural_similarity",
            fixed_cut_dissimilarity_threshold=0.5,
            asset_duration_seconds=30.0,
        )

        assert len(boundaries) == 3
        for index, boundary in enumerate(boundaries):
            assert boundary.scene_index == index

    def test_nearby_spikes_within_minimum_duration_are_merged(self) -> None:
        """Two spikes closer than minimum_scene_duration_seconds merge into one cut."""
        timestamps = [float(i) * 0.1 for i in range(30)]  # 0.0, 0.1, 0.2, ... seconds
        dissimilarity_scores = [0.02] * (len(timestamps) - 1)
        dissimilarity_scores[9] = 0.9
        dissimilarity_scores[10] = 0.9  # Only 0.1s after the first spike.
        complexity_scores = [0.1] * len(timestamps)
        thresholds = SceneCutDetectionThresholds(minimum_scene_duration_seconds=0.5)

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexity_scores,
            detection_strategy_name="structural_similarity",
            fixed_cut_dissimilarity_threshold=0.5,
            asset_duration_seconds=2.9,
            thresholds=thresholds,
        )

        # The two adjacent spikes must merge into a single boundary, not two.
        assert len(boundaries) == 2

    def test_mismatched_input_lengths_raise_value_error(self) -> None:
        """Dissimilarity score count that doesn't match timestamps - 1 raises."""
        with pytest.raises(ValueError):
            detect_scene_boundaries_via_adaptive_thresholding(
                frame_pair_dissimilarity_scores=[0.1, 0.2],
                frame_timestamps_seconds=[0.0, 1.0, 2.0, 3.0],
                per_frame_motion_complexity_scores=[0.1, 0.1, 0.1, 0.1],
                detection_strategy_name="histogram_difference",
                fixed_cut_dissimilarity_threshold=0.5,
                asset_duration_seconds=3.0,
            )

    def test_first_scene_confidence_is_always_full(self) -> None:
        """The very first scene (no preceding cut) always has confidence 1.0."""
        timestamps = [float(i) for i in range(10)]
        dissimilarity_scores = [0.01] * (len(timestamps) - 1)
        complexity_scores = [0.1] * len(timestamps)

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexity_scores,
            detection_strategy_name="histogram_difference",
            fixed_cut_dissimilarity_threshold=0.5,
            asset_duration_seconds=10.0,
        )

        assert boundaries[0].cut_confidence_score == pytest.approx(1.0)
