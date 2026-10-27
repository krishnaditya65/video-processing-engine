"""Unit tests for rule-based shot-type classification.

Exercises the pure motion-magnitude/edge-density computations and the
resulting classification decision list against small synthetic NumPy
frame sequences, plus the pipeline stage that wires the strategy into
the Chain of Responsibility.
"""

from __future__ import annotations

import dataclasses

import numpy as np
import pytest

from ingestion_inspection_service.config import ShotTypeClassificationThresholds
from ingestion_inspection_service.domain.models import SceneBoundary, ShotType
from ingestion_inspection_service.pipeline.context import (
    InspectionContext,
    SampledFrameRecord,
)
from ingestion_inspection_service.pipeline.stages import ShotTypeClassificationStage
from ingestion_inspection_service.strategies.shot_type_classification import (
    RuleBasedShotTypeClassificationStrategy,
    compute_average_edge_density,
    compute_average_motion_magnitude,
)
from tests.conftest import make_checkerboard_frame, make_solid_color_frame


class TestMotionMagnitudeAndEdgeDensityHelpers:
    """Tests the pure per-scene signal computations."""

    def test_identical_frames_have_zero_motion_magnitude(self) -> None:
        """Two bit-identical frames produce zero frame-differencing motion."""
        frame = make_checkerboard_frame(48, 48, 6, seed=1)
        motion = compute_average_motion_magnitude([frame.copy(), frame.copy()])
        assert motion == pytest.approx(0.0, abs=1e-9)

    def test_black_to_white_flip_has_high_motion_magnitude(self) -> None:
        """A full black<->white flip between frames produces near-maximal motion."""
        black_frame = make_solid_color_frame(32, 32, (0, 0, 0))
        white_frame = make_solid_color_frame(32, 32, (255, 255, 255))
        motion = compute_average_motion_magnitude(
            [black_frame, white_frame, black_frame, white_frame]
        )
        assert motion == pytest.approx(1.0, abs=1e-6)

    def test_single_frame_motion_magnitude_is_zero(self) -> None:
        """A single frame (no pair to diff) yields zero motion magnitude."""
        frame = make_solid_color_frame(16, 16, (10, 20, 30))
        assert compute_average_motion_magnitude([frame]) == 0.0

    def test_solid_color_frame_has_zero_edge_density(self) -> None:
        """A flat solid-color frame has no Canny-detected edges."""
        frame = make_solid_color_frame(48, 48, (128, 128, 128))
        assert compute_average_edge_density([frame]) == pytest.approx(0.0, abs=1e-6)

    def test_high_contrast_checkerboard_has_nonzero_edge_density(self) -> None:
        """A fine, high-contrast checkerboard produces nontrivial edge density."""
        frame = make_checkerboard_frame(64, 64, 4, seed=7)
        assert compute_average_edge_density([frame]) > 0.05

    def test_empty_frame_list_returns_zero_for_both_helpers(self) -> None:
        """Both helpers degrade to 0.0 rather than raising on empty input."""
        assert compute_average_motion_magnitude([]) == 0.0
        assert compute_average_edge_density([]) == 0.0


class TestRuleBasedShotTypeClassificationStrategy:
    """Tests the end-to-end classification decision list."""

    def test_high_motion_scene_is_classified_action(self) -> None:
        """Rapidly alternating high-contrast frames trigger ACTION classification."""
        black_frame = make_solid_color_frame(32, 32, (0, 0, 0))
        white_frame = make_solid_color_frame(32, 32, (255, 255, 255))
        strategy = RuleBasedShotTypeClassificationStrategy()

        shot_type = strategy.classify_scene_frames(
            [black_frame, white_frame, black_frame, white_frame]
        )
        assert shot_type == ShotType.ACTION

    def test_static_detailed_scene_is_classified_close_up(self) -> None:
        """A static, high-edge-density scene (repeated identical detailed frame) is CLOSE_UP."""
        detailed_frame = make_checkerboard_frame(64, 64, 3, seed=99)
        strategy = RuleBasedShotTypeClassificationStrategy()

        shot_type = strategy.classify_scene_frames(
            [detailed_frame.copy() for _ in range(4)]
        )
        assert shot_type == ShotType.CLOSE_UP

    def test_static_flat_scene_is_classified_dialogue(self) -> None:
        """A static, low-edge-density scene (repeated flat frame) is DIALOGUE."""
        flat_frame = make_solid_color_frame(48, 48, (90, 90, 90))
        strategy = RuleBasedShotTypeClassificationStrategy()

        shot_type = strategy.classify_scene_frames(
            [flat_frame.copy() for _ in range(4)]
        )
        assert shot_type == ShotType.DIALOGUE

    def test_moderate_motion_scene_is_classified_wide(self) -> None:
        """Moderate motion below the action threshold, above the low-motion floor, is WIDE."""
        thresholds = ShotTypeClassificationThresholds(
            action_motion_magnitude_threshold=0.5,
            close_up_edge_density_threshold=0.9,
            low_motion_magnitude_threshold=0.01,
        )
        strategy = RuleBasedShotTypeClassificationStrategy(thresholds)
        flat_frame_a = make_solid_color_frame(32, 32, (80, 80, 80))
        flat_frame_b = make_solid_color_frame(32, 32, (140, 140, 140))

        shot_type = strategy.classify_scene_frames([flat_frame_a, flat_frame_b])
        assert shot_type == ShotType.WIDE

    def test_empty_frame_list_is_classified_unknown(self) -> None:
        """No frames at all yields UNKNOWN rather than a spurious classification."""
        strategy = RuleBasedShotTypeClassificationStrategy()
        assert strategy.classify_scene_frames([]) == ShotType.UNKNOWN

    def test_strategy_name_is_stable(self) -> None:
        """strategy_name returns the documented stable identifier."""
        strategy = RuleBasedShotTypeClassificationStrategy()
        assert strategy.strategy_name == "rule_based_motion_edge_density"


class TestShotTypeClassificationStage:
    """Tests the pipeline stage that classifies each SceneBoundary in place."""

    def test_stage_populates_shot_type_on_every_scene(self) -> None:
        """Running the stage rewrites each scene with a non-None shot_type."""
        detailed_frame = make_checkerboard_frame(48, 48, 3, seed=5)
        black_frame = make_solid_color_frame(48, 48, (0, 0, 0))

        sampled_frames = [
            SampledFrameRecord(detailed_frame.copy(), 0.0),
            SampledFrameRecord(detailed_frame.copy(), 1.0),
            SampledFrameRecord(black_frame.copy(), 2.0),
            SampledFrameRecord(black_frame.copy(), 3.0),
        ]
        scene_boundaries = [
            SceneBoundary(
                scene_index=0,
                start_timestamp_seconds=0.0,
                end_timestamp_seconds=2.0,
                detection_strategy_name="histogram_difference",
                cut_confidence_score=1.0,
                motion_complexity_score=0.1,
            ),
            SceneBoundary(
                scene_index=1,
                start_timestamp_seconds=2.0,
                end_timestamp_seconds=4.0,
                detection_strategy_name="histogram_difference",
                cut_confidence_score=0.8,
                motion_complexity_score=0.0,
            ),
        ]
        context = InspectionContext(
            asset_id="stage-test-asset",
            source_file_path="/synthetic/does-not-exist.mov",
            sampled_frames=sampled_frames,
            duration_seconds=4.0,
            scene_boundaries=scene_boundaries,
        )

        stage = ShotTypeClassificationStage(RuleBasedShotTypeClassificationStrategy())
        stage.process(context)

        assert len(context.scene_boundaries) == 2
        assert all(
            scene.shot_type is not None and scene.shot_type != ShotType.UNKNOWN
            for scene in context.scene_boundaries
        )
        # Original ordering/fields are preserved aside from the added shot_type.
        assert context.scene_boundaries[0].start_timestamp_seconds == 0.0
        assert context.scene_boundaries[1].end_timestamp_seconds == 4.0

    def test_stage_is_a_noop_when_no_scenes_detected(self) -> None:
        """The stage leaves an empty scene_boundaries list untouched."""
        context = InspectionContext(
            asset_id="empty-asset",
            source_file_path="/synthetic/does-not-exist.mov",
            sampled_frames=[],
            duration_seconds=0.0,
            scene_boundaries=[],
        )
        stage = ShotTypeClassificationStage(RuleBasedShotTypeClassificationStrategy())
        stage.process(context)
        assert context.scene_boundaries == []

    def test_stage_name_is_stable(self) -> None:
        """stage_name returns the documented stable identifier."""
        stage = ShotTypeClassificationStage(RuleBasedShotTypeClassificationStrategy())
        assert stage.stage_name == "shot_type_classification"


def test_scene_boundary_shot_type_defaults_to_none() -> None:
    """A SceneBoundary constructed without shot_type defaults to None (backward compatible)."""
    boundary = SceneBoundary(
        scene_index=0,
        start_timestamp_seconds=0.0,
        end_timestamp_seconds=1.0,
        detection_strategy_name="histogram_difference",
        cut_confidence_score=1.0,
        motion_complexity_score=0.2,
    )
    assert boundary.shot_type is None


def test_scene_boundary_shot_type_can_be_replaced() -> None:
    """dataclasses.replace can attach a shot_type to a frozen SceneBoundary."""
    boundary = SceneBoundary(
        scene_index=0,
        start_timestamp_seconds=0.0,
        end_timestamp_seconds=1.0,
        detection_strategy_name="histogram_difference",
        cut_confidence_score=1.0,
        motion_complexity_score=0.2,
    )
    replaced = dataclasses.replace(boundary, shot_type=ShotType.CLOSE_UP)
    assert replaced.shot_type == ShotType.CLOSE_UP
    assert boundary.shot_type is None
