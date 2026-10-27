"""Pipeline orchestrator that assembles and runs the inspection chain.

Design pattern: **Pipeline / Facade over Chain of Responsibility**.
:class:`InspectionPipeline` wires an ordered list of
:class:`~ingestion_inspection_service.pipeline.stages.InspectionStage`
instances into a linked chain (each stage's ``set_next_stage`` pointing
to the next) and exposes a single ``run`` entry point that hides the
chain-traversal mechanics from callers (the CLI and FastAPI layers).
"""

from __future__ import annotations

import typing

from ingestion_inspection_service.config import DEFAULT_FRAME_SAMPLING_CONFIGURATION
from ingestion_inspection_service.domain.models import StructuralMap
from ingestion_inspection_service.pipeline.context import InspectionContext
from ingestion_inspection_service.pipeline.stages import (
    BlackFrameDetectionStage,
    ColorSpaceExtractionStage,
    InspectionStage,
    LoudnessAnalysisStage,
    SceneCutDetectionStage,
    ShotTypeClassificationStage,
)
from ingestion_inspection_service.strategies.scene_detection import (
    SceneDetectionStrategy,
    StructuralSimilaritySceneDetectionStrategy,
)
from ingestion_inspection_service.strategies.shot_type_classification import (
    RuleBasedShotTypeClassificationStrategy,
    ShotTypeClassificationStrategy,
)


class InspectionPipeline:
    """Assembles and executes an ordered chain of inspection stages.

    Attributes:
        stages: The ordered list of stages composing this pipeline, as
            provided at construction time.
    """

    def __init__(self, stages: typing.Sequence[InspectionStage]) -> None:
        """Wires the given stages into a linked Chain of Responsibility.

        Args:
            stages: Non-empty ordered sequence of stages to execute.

        Raises:
            ValueError: If ``stages`` is empty.
        """
        if not stages:
            raise ValueError("InspectionPipeline requires at least one stage.")

        self.stages: typing.List[InspectionStage] = list(stages)
        for current_stage, following_stage in zip(self.stages, self.stages[1:]):
            current_stage.set_next_stage(following_stage)

    def run(self, context: InspectionContext) -> InspectionContext:
        """Executes the full chain starting from the first stage.

        Args:
            context: The populated (frames sampled, PCM extracted as
                applicable) inspection context to process.

        Returns:
            InspectionContext: The same context instance, fully
            populated by every stage in the chain.
        """
        return self.stages[0].handle(context)

    def build_structural_map(self, context: InspectionContext) -> StructuralMap:
        """Converts a fully-processed context into the final StructuralMap.

        Args:
            context: An :class:`InspectionContext` that has already
                been passed through :meth:`run`.

        Returns:
            StructuralMap: The immutable-shaped (though technically
            mutable-dataclass) output artifact ready for JSON
            serialization.
        """
        return StructuralMap(
            asset_id=context.asset_id,
            source_file_path=context.source_file_path,
            duration_seconds=context.duration_seconds,
            video_stream_profile=context.video_stream_profile,
            audio_stream_profile=context.audio_stream_profile,
            color_space_profile=context.color_space_profile,
            scene_boundaries=context.scene_boundaries,
            black_frame_ranges=context.black_frame_ranges,
            loudness_analysis=context.loudness_analysis,
            frame_sampling_rate_used_fps=context.frame_sampling_rate_used_fps,
            pipeline_stage_names_executed=list(context.executed_stage_names),
        )


def build_default_inspection_pipeline(
    scene_detection_strategy: typing.Optional[SceneDetectionStrategy] = None,
) -> InspectionPipeline:
    """Builds the platform-standard four-stage inspection pipeline.

    Stage order: black-frame detection -> scene-cut detection ->
    color-space extraction -> loudness analysis, per the platform
    specification.

    Args:
        scene_detection_strategy: The Strategy implementation to use
            for scene-cut detection. Defaults to
            :class:`~ingestion_inspection_service.strategies.scene_detection.StructuralSimilaritySceneDetectionStrategy`
            (higher discriminative power) when not provided; pass
            :class:`~ingestion_inspection_service.strategies.scene_detection.HistogramDifferenceSceneDetectionStrategy`
            for a faster, coarser pass on very large batch workloads.

    Returns:
        InspectionPipeline: A ready-to-run pipeline with all four
        default stages wired in order.
    """
    active_strategy = (
        scene_detection_strategy or StructuralSimilaritySceneDetectionStrategy()
    )
    return InspectionPipeline(
        [
            BlackFrameDetectionStage(),
            SceneCutDetectionStage(active_strategy),
            ColorSpaceExtractionStage(),
            LoudnessAnalysisStage(),
        ]
    )


def build_inspection_pipeline_with_shot_type_classification(
    scene_detection_strategy: typing.Optional[SceneDetectionStrategy] = None,
    shot_type_classification_strategy: typing.Optional[
        ShotTypeClassificationStrategy
    ] = None,
) -> InspectionPipeline:
    """Builds the default pipeline plus an opt-in shot-type classification stage.

    Kept separate from :func:`build_default_inspection_pipeline` (rather
    than adding :class:`~ingestion_inspection_service.pipeline.stages.ShotTypeClassificationStage`
    to the default chain) so the platform-standard
    ``pipeline_stage_names_executed`` contract -- ``black_frame_detection``,
    ``scene_cut_detection``, ``color_space_extraction``,
    ``loudness_analysis``, in that order -- remains unchanged for
    existing callers that don't explicitly ask for shot-type
    classification.

    Stage order: black-frame detection -> scene-cut detection ->
    shot-type classification -> color-space extraction -> loudness
    analysis. Shot-type classification runs immediately after scene-cut
    detection because it depends on ``context.scene_boundaries`` being
    populated.

    Args:
        scene_detection_strategy: The scene-cut Strategy to use;
            defaults the same way as
            :func:`build_default_inspection_pipeline`.
        shot_type_classification_strategy: The shot-type Strategy to
            use; defaults to
            :class:`~ingestion_inspection_service.strategies.shot_type_classification.RuleBasedShotTypeClassificationStrategy`
            when not provided.

    Returns:
        InspectionPipeline: A ready-to-run five-stage pipeline.
    """
    active_scene_strategy = (
        scene_detection_strategy or StructuralSimilaritySceneDetectionStrategy()
    )
    active_shot_type_strategy = (
        shot_type_classification_strategy or RuleBasedShotTypeClassificationStrategy()
    )
    return InspectionPipeline(
        [
            BlackFrameDetectionStage(),
            SceneCutDetectionStage(active_scene_strategy),
            ShotTypeClassificationStage(active_shot_type_strategy),
            ColorSpaceExtractionStage(),
            LoudnessAnalysisStage(),
        ]
    )


def create_inspection_context_from_source_file(
    asset_id: str,
    source_file_path: str,
    *,
    analysis_frames_per_second: typing.Optional[float] = None,
    extract_audio_for_loudness_analysis: bool = True,
) -> InspectionContext:
    """Builds a populated context by decoding frames (and optionally audio).

    This is the real, I/O-performing counterpart to constructing an
    :class:`InspectionContext` by hand (which unit tests do directly
    with synthetic data). It is imported lazily inside the function body
    to keep this module importable even in environments where OpenCV's
    video I/O backend has issues unrelated to pure computation, and to
    keep the CLI/API entry points as the only real callers of this path.

    Args:
        asset_id: Opaque identifier for the asset being inspected.
        source_file_path: Filesystem path to the mezzanine file.
        analysis_frames_per_second: Overrides the default analysis
            sampling rate when provided.
        extract_audio_for_loudness_analysis: When True, attempts to
            extract mono PCM audio via ffmpeg for the loudness stage;
            when False (or on extraction failure), the loudness stage
            will simply report no result.

    Returns:
        InspectionContext: A context with ``sampled_frames`` (and,
        best-effort, ``raw_audio_pcm_samples``) populated, ready to be
        passed to :meth:`InspectionPipeline.run`.

    Raises:
        ingestion_inspection_service.adapters.frame_source.VideoDecodeError:
            If the source file cannot be opened/decoded by OpenCV.
    """
    from ingestion_inspection_service.adapters.audio_extraction import (
        extract_mono_pcm_via_ffmpeg,
    )
    from ingestion_inspection_service.adapters.frame_source import (
        SampledFrame,
        probe_video_duration_seconds,
        sample_frames_at_fixed_rate,
    )
    from ingestion_inspection_service.config import FrameSamplingConfiguration
    from ingestion_inspection_service.pipeline.context import SampledFrameRecord

    sampling_configuration = (
        FrameSamplingConfiguration(
            target_analysis_frames_per_second=analysis_frames_per_second
        )
        if analysis_frames_per_second is not None
        else DEFAULT_FRAME_SAMPLING_CONFIGURATION
    )

    sampled_frames: typing.List[SampledFrame] = list(
        sample_frames_at_fixed_rate(source_file_path, sampling_configuration)
    )
    duration_seconds = probe_video_duration_seconds(source_file_path)

    context = InspectionContext(
        asset_id=asset_id,
        source_file_path=source_file_path,
        sampled_frames=[
            SampledFrameRecord(
                frame_bgr=frame.frame_bgr, timestamp_seconds=frame.timestamp_seconds
            )
            for frame in sampled_frames
        ],
        duration_seconds=duration_seconds,
        frame_sampling_rate_used_fps=sampling_configuration.target_analysis_frames_per_second,
    )

    if extract_audio_for_loudness_analysis:
        try:
            pcm_samples, sample_rate_hz = extract_mono_pcm_via_ffmpeg(
                source_file_path
            )
            context.raw_audio_pcm_samples = pcm_samples
            context.raw_audio_sample_rate_hz = sample_rate_hz
        except Exception:
            # Loudness extraction is best-effort: an asset with no audio
            # track, or an environment without a working ffmpeg audio
            # decode path, should not fail the entire inspection run.
            context.raw_audio_pcm_samples = None
            context.raw_audio_sample_rate_hz = None

    return context
