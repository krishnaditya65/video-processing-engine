"""Shared mutable context object threaded through the inspection pipeline.

Design pattern note: this is the "request" object passed link-to-link
along the Chain of Responsibility. Each ``InspectionStage`` reads
whatever upstream stages have already populated (e.g. the scene-cut
stage reads the sampled frames but not any prior stage's *output*; the
color-space stage doesn't need frames at all) and writes its own
findings onto the shared context, rather than stages returning
bespoke, mutually-incompatible return types.
"""

from __future__ import annotations

import dataclasses
import typing

import numpy as np

from ingestion_inspection_service.domain.models import (
    AudioStreamProfile,
    BlackFrameRange,
    ColorSpaceProfile,
    LoudnessAnalysisResult,
    SceneBoundary,
    VideoStreamProfile,
)


@dataclasses.dataclass
class SampledFrameRecord:
    """A minimal, decode-library-agnostic record of one sampled frame.

    Kept separate from
    ``adapters.frame_source.SampledFrame`` so pipeline stages and their
    unit tests depend only on this lightweight shape (which tests can
    construct directly from synthetic NumPy arrays) rather than on the
    OpenCV-backed adapter type.

    Attributes:
        frame_bgr: The decoded frame, ``(H, W, 3)`` ``uint8`` BGR.
        timestamp_seconds: Presentation timestamp of this frame within
            the asset's timeline, in seconds.
    """

    frame_bgr: np.ndarray
    timestamp_seconds: float


@dataclasses.dataclass
class InspectionContext:
    """Mutable state shared across all stages of one inspection run.

    Attributes:
        asset_id: Opaque identifier for the asset being inspected.
        source_file_path: Filesystem path to the mezzanine file.
        sampled_frames: Frames decoded at the configured analysis
            sampling rate, in ascending timestamp order. Populated
            before the chain begins executing (either by the CLI/API
            entry point via the real OpenCV adapter, or directly by
            tests with synthetic arrays).
        duration_seconds: Total asset duration in seconds. May be
            refined by the color-space stage once ffprobe has run, if
            it was not already known.
        video_stream_profile: Populated by
            :class:`~ingestion_inspection_service.pipeline.stages.ColorSpaceExtractionStage`.
        audio_stream_profile: Populated by the same stage.
        color_space_profile: Populated by the same stage.
        raw_audio_pcm_samples: Optional raw PCM samples (mono,
            normalized float in ``[-1, 1]``) used by the loudness
            stage; ``None`` when no audio track is present or audio
            extraction was skipped.
        raw_audio_sample_rate_hz: Sample rate of
            ``raw_audio_pcm_samples``, when present.
        black_frame_ranges: Populated by
            :class:`~ingestion_inspection_service.pipeline.stages.BlackFrameDetectionStage`.
        scene_boundaries: Populated by
            :class:`~ingestion_inspection_service.pipeline.stages.SceneCutDetectionStage`.
        loudness_analysis: Populated by
            :class:`~ingestion_inspection_service.pipeline.stages.LoudnessAnalysisStage`.
        executed_stage_names: Ordered log of stage names that have run,
            appended to by each stage's ``process`` implementation.
        frame_sampling_rate_used_fps: Recorded analysis frame rate, for
            inclusion in the final :class:`StructuralMap`.
    """

    asset_id: str
    source_file_path: str
    sampled_frames: typing.List[SampledFrameRecord] = dataclasses.field(
        default_factory=list
    )
    duration_seconds: float = 0.0
    video_stream_profile: typing.Optional[VideoStreamProfile] = None
    audio_stream_profile: typing.Optional[AudioStreamProfile] = None
    color_space_profile: typing.Optional[ColorSpaceProfile] = None
    raw_audio_pcm_samples: typing.Optional[np.ndarray] = None
    raw_audio_sample_rate_hz: typing.Optional[int] = None
    black_frame_ranges: typing.List[BlackFrameRange] = dataclasses.field(
        default_factory=list
    )
    scene_boundaries: typing.List[SceneBoundary] = dataclasses.field(
        default_factory=list
    )
    loudness_analysis: typing.Optional[LoudnessAnalysisResult] = None
    executed_stage_names: typing.List[str] = dataclasses.field(default_factory=list)
    frame_sampling_rate_used_fps: float = 1.0
