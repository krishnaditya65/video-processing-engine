"""Unit tests for pipeline composition (Chain of Responsibility) end-to-end.

Builds an InspectionContext directly from synthetic in-memory frames
and PCM samples (no real video file, no ffprobe/ffmpeg subprocess
calls), and runs it through the real stage chain to verify:

  * Stages execute in the documented order.
  * Each stage's output lands on the shared context / final
    StructuralMap.
  * The pipeline degrades gracefully when a stage has nothing to do
    (e.g. no audio samples supplied).
"""

from __future__ import annotations

import numpy as np
import pytest

from ingestion_inspection_service.domain.models import LoudnessAnalysisResult
from ingestion_inspection_service.pipeline.context import (
    InspectionContext,
    SampledFrameRecord,
)
from ingestion_inspection_service.pipeline.pipeline import (
    InspectionPipeline,
    build_default_inspection_pipeline,
)
from ingestion_inspection_service.pipeline.stages import (
    BlackFrameDetectionStage,
    InspectionStage,
    LoudnessAnalysisStage,
    SceneCutDetectionStage,
)
from ingestion_inspection_service.strategies.scene_detection import (
    HistogramDifferenceSceneDetectionStrategy,
)
from tests.conftest import make_checkerboard_frame, make_solid_color_frame


def _build_synthetic_context(asset_id: str = "test-asset") -> InspectionContext:
    """Builds an InspectionContext with a hand-crafted synthetic frame sequence.

    The sequence is: 5 seconds of a bright checkerboard scene, then 2
    seconds of black frames, then 5 seconds of a different checkerboard
    scene -- exercising both the black-frame stage and the scene-cut
    stage's ability to find the shot change at t=7s.

    Args:
        asset_id: Asset identifier to embed in the context.

    Returns:
        InspectionContext: A context with ``sampled_frames``,
        ``duration_seconds``, and synthetic mono PCM audio populated.
    """
    frames = []
    timestamp = 0.0
    scene_one_frame = make_checkerboard_frame(48, 48, 6, seed=11)
    black_frame = make_solid_color_frame(48, 48, (0, 0, 0))
    scene_two_frame = make_checkerboard_frame(48, 48, 6, seed=42)

    for _ in range(5):
        frames.append(SampledFrameRecord(scene_one_frame.copy(), timestamp))
        timestamp += 1.0
    for _ in range(2):
        frames.append(SampledFrameRecord(black_frame.copy(), timestamp))
        timestamp += 1.0
    for _ in range(5):
        frames.append(SampledFrameRecord(scene_two_frame.copy(), timestamp))
        timestamp += 1.0

    sine_wave = 0.3 * np.sin(
        2 * np.pi * 440.0 * np.arange(48_000 * 2) / 48_000.0
    )

    return InspectionContext(
        asset_id=asset_id,
        source_file_path="/synthetic/does-not-exist.mov",
        sampled_frames=frames,
        duration_seconds=timestamp,
        raw_audio_pcm_samples=sine_wave,
        raw_audio_sample_rate_hz=48_000,
    )


class TestInspectionPipelineComposition:
    """Tests the Chain-of-Responsibility wiring and execution order."""

    def test_stages_execute_in_configured_order(self) -> None:
        """executed_stage_names records stages in the exact order they ran."""
        context = _build_synthetic_context()
        pipeline = InspectionPipeline(
            [
                BlackFrameDetectionStage(),
                SceneCutDetectionStage(HistogramDifferenceSceneDetectionStrategy()),
                LoudnessAnalysisStage(),
            ]
        )

        pipeline.run(context)

        assert context.executed_stage_names == [
            "black_frame_detection",
            "scene_cut_detection",
            "loudness_analysis",
        ]

    def test_empty_stage_list_raises_value_error(self) -> None:
        """Constructing a pipeline with zero stages raises ValueError."""
        with pytest.raises(ValueError):
            InspectionPipeline([])

    def test_set_next_stage_returns_the_argument_for_fluent_chaining(self) -> None:
        """set_next_stage returns the passed-in stage, enabling fluent chaining."""
        stage_a = BlackFrameDetectionStage()
        stage_b = LoudnessAnalysisStage()

        returned = stage_a.set_next_stage(stage_b)

        assert returned is stage_b


class TestInspectionPipelineEndToEndOnSyntheticData:
    """End-to-end pipeline runs against fully synthetic in-memory data."""

    def test_black_frame_range_is_detected_between_the_two_scenes(self) -> None:
        """The 2-second black run at t=5..7s is detected as a BlackFrameRange."""
        context = _build_synthetic_context()
        pipeline = InspectionPipeline([BlackFrameDetectionStage()])

        pipeline.run(context)

        assert len(context.black_frame_ranges) == 1
        assert context.black_frame_ranges[0].start_timestamp_seconds == pytest.approx(
            5.0
        )
        assert context.black_frame_ranges[0].end_timestamp_seconds == pytest.approx(
            6.0
        )

    def test_scene_cut_is_detected_at_the_shot_change(self) -> None:
        """A scene cut is detected somewhere around the checkerboard shot change."""
        context = _build_synthetic_context()
        pipeline = InspectionPipeline(
            [SceneCutDetectionStage(HistogramDifferenceSceneDetectionStrategy())]
        )

        pipeline.run(context)

        assert len(context.scene_boundaries) >= 2
        # Boundaries must be contiguous and cover [0, duration).
        assert context.scene_boundaries[0].start_timestamp_seconds == pytest.approx(
            0.0
        )
        assert context.scene_boundaries[-1].end_timestamp_seconds == pytest.approx(
            context.duration_seconds
        )

    def test_loudness_stage_populates_result_from_synthetic_pcm(self) -> None:
        """LoudnessAnalysisStage produces a LoudnessAnalysisResult from PCM samples."""
        context = _build_synthetic_context()
        pipeline = InspectionPipeline([LoudnessAnalysisStage()])

        pipeline.run(context)

        assert isinstance(context.loudness_analysis, LoudnessAnalysisResult)

    def test_loudness_stage_is_none_when_no_audio_samples_present(self) -> None:
        """LoudnessAnalysisStage leaves loudness_analysis as None with no PCM data."""
        context = _build_synthetic_context()
        context.raw_audio_pcm_samples = None
        context.raw_audio_sample_rate_hz = None
        pipeline = InspectionPipeline([LoudnessAnalysisStage()])

        pipeline.run(context)

        assert context.loudness_analysis is None

    def test_build_structural_map_reflects_all_stage_outputs(self) -> None:
        """build_structural_map assembles a StructuralMap from the processed context."""
        context = _build_synthetic_context()
        pipeline = InspectionPipeline(
            [
                BlackFrameDetectionStage(),
                SceneCutDetectionStage(HistogramDifferenceSceneDetectionStrategy()),
                LoudnessAnalysisStage(),
            ]
        )
        pipeline.run(context)

        structural_map = pipeline.build_structural_map(context)

        assert structural_map.asset_id == "test-asset"
        assert structural_map.total_black_frame_range_count == 1
        assert structural_map.total_scene_count >= 2
        assert structural_map.loudness_analysis is not None
        assert structural_map.pipeline_stage_names_executed == [
            "black_frame_detection",
            "scene_cut_detection",
            "loudness_analysis",
        ]
        # Must be JSON-serializable end-to-end.
        json_text = structural_map.to_json_string()
        assert '"asset_id": "test-asset"' in json_text

    def test_build_default_inspection_pipeline_uses_four_stages_in_order(self) -> None:
        """build_default_inspection_pipeline wires exactly the required 4 stages."""
        pipeline = build_default_inspection_pipeline()

        stage_names = [stage.stage_name for stage in pipeline.stages]

        assert stage_names == [
            "black_frame_detection",
            "scene_cut_detection",
            "color_space_extraction",
            "loudness_analysis",
        ]

    def test_scene_cut_stage_handles_fewer_than_two_frames_gracefully(self) -> None:
        """SceneCutDetectionStage yields no boundaries with fewer than 2 frames."""
        context = InspectionContext(
            asset_id="single-frame-asset",
            source_file_path="/synthetic/x.mov",
            sampled_frames=[
                SampledFrameRecord(
                    make_solid_color_frame(32, 32, (50, 50, 50)), 0.0
                )
            ],
            duration_seconds=1.0,
        )
        pipeline = InspectionPipeline(
            [SceneCutDetectionStage(HistogramDifferenceSceneDetectionStrategy())]
        )

        pipeline.run(context)

        assert context.scene_boundaries == []
