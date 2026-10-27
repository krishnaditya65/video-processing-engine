"""Concrete Chain-of-Responsibility stages for the inspection pipeline.

Ordered stage sequence (see ``pipeline.pipeline.build_default_inspection_pipeline``):

    1. :class:`BlackFrameDetectionStage`
    2. :class:`SceneCutDetectionStage`
    3. :class:`ColorSpaceExtractionStage`
    4. :class:`LoudnessAnalysisStage`

Each stage implements :meth:`InspectionStage.process`, mutates the
shared :class:`~ingestion_inspection_service.pipeline.context.InspectionContext`
with its findings, and (via the base class) forwards the context to the
next link in the chain. Pure, decode-library-independent computational
cores are factored into module-level functions
(:func:`compute_luminance_statistics_for_frame`,
:func:`merge_black_frame_samples_into_ranges`,
:func:`compute_approximate_integrated_loudness_lufs`) specifically so
they can be unit-tested directly against synthetic NumPy arrays without
needing a real video/audio file or the ffprobe binary.
"""

from __future__ import annotations

import abc
import dataclasses
import time
import typing

import cv2
import numpy as np

from ingestion_inspection_service.adapters.ffprobe_client import (
    FfprobeExecutionError,
    extract_typed_stream_metadata_via_ffprobe,
)
from ingestion_inspection_service.config import (
    DEFAULT_BLACK_FRAME_DETECTION_THRESHOLDS,
    DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
    BlackFrameDetectionThresholds,
    LoudnessAnalysisConfiguration,
)
from ingestion_inspection_service.domain.models import (
    BlackFrameRange,
    ColorSpaceProfile,
    HdrFormat,
    LoudnessAnalysisResult,
)
from ingestion_inspection_service.pipeline.context import InspectionContext
from ingestion_inspection_service.strategies.scene_detection import (
    SceneDetectionStrategy,
    compute_motion_complexity_score,
    detect_scene_boundaries_via_adaptive_thresholding,
)
from ingestion_inspection_service.strategies.shot_type_classification import (
    ShotTypeClassificationStrategy,
)


class InspectionStage(abc.ABC):
    """Abstract base for one link in the inspection Chain of Responsibility.

    Subclasses implement :meth:`process`, which performs the stage's
    analysis and mutates the given context in place. The base class's
    :meth:`handle` wraps ``process`` with bookkeeping (recording the
    stage name as executed) and forwards to the next stage, if any,
    implementing the actual chaining behavior once and only once.
    """

    _next_stage: typing.Optional["InspectionStage"] = None

    def set_next_stage(self, next_stage: "InspectionStage") -> "InspectionStage":
        """Sets the next stage in the chain and returns it for fluent chaining.

        Args:
            next_stage: The stage to invoke after this one completes.

        Returns:
            InspectionStage: ``next_stage``, enabling calls like
            ``stage_a.set_next_stage(stage_b).set_next_stage(stage_c)``.
        """
        self._next_stage = next_stage
        return next_stage

    def handle(self, context: InspectionContext) -> InspectionContext:
        """Runs this stage's analysis, then forwards to the next stage.

        Args:
            context: The shared inspection context to read from and
                mutate.

        Returns:
            InspectionContext: The same context instance, after this
            stage and every downstream stage in the chain has run.

        Algorithmic notes:
            Wraps :meth:`process` with a wall-clock timer whose duration
            is reported to the module-level Prometheus
            ``pipeline_stage_duration_seconds`` histogram (see
            :mod:`ingestion_inspection_service.metrics`), labeled by
            ``stage_name``. Metrics recording is purely observational
            (a single ``Histogram.observe`` call with no side effects on
            ``context``) and never raises or alters control flow, so it
            cannot change existing stage behavior or test outcomes.
        """
        from ingestion_inspection_service.metrics import (
            PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM,
        )

        stage_start_time = time.monotonic()
        self.process(context)
        PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM.labels(
            stage_name=self.stage_name
        ).observe(time.monotonic() - stage_start_time)
        context.executed_stage_names.append(self.stage_name)
        if self._next_stage is not None:
            return self._next_stage.handle(context)
        return context

    @abc.abstractmethod
    def process(self, context: InspectionContext) -> None:
        """Performs this stage's analysis, mutating ``context`` in place.

        Args:
            context: The shared inspection context.

        Raises:
            NotImplementedError: If not overridden by a subclass.
        """
        raise NotImplementedError

    @property
    @abc.abstractmethod
    def stage_name(self) -> str:
        """Returns a short, stable, machine-readable stage identifier."""
        raise NotImplementedError


class BlackFrameDetectionStage(InspectionStage):
    """Detects black-frame / fade-to-black time ranges via luminance checks.

    Algorithmic notes:
        For each analysis-sampled frame, converts BGR to grayscale and
        computes the mean and max pixel luminance. A frame is
        classified "black" when both its mean luminance is below
        ``mean_luminance_black_threshold`` and its max luminance is
        below ``maximum_luminance_black_threshold`` (the max-luminance
        check avoids misclassifying a mostly-dark frame containing a
        small bright highlight, e.g. on-screen titles over black, as a
        true black frame). Consecutive black samples are merged into
        ranges; ranges shorter than
        ``minimum_consecutive_black_frames`` samples are discarded as
        noise. O(n) in the number of sampled frames, each frame costing
        O(H*W) for the grayscale conversion and reduction.
    """

    def __init__(
        self,
        thresholds: BlackFrameDetectionThresholds = DEFAULT_BLACK_FRAME_DETECTION_THRESHOLDS,
    ) -> None:
        """Initializes the stage with black-frame detection thresholds.

        Args:
            thresholds: Tunable luminance/consecutive-frame thresholds.
        """
        self._thresholds = thresholds

    def process(self, context: InspectionContext) -> None:
        """Computes black-frame ranges and stores them on the context.

        Args:
            context: The shared inspection context; reads
                ``context.sampled_frames`` and writes
                ``context.black_frame_ranges``.
        """
        if not context.sampled_frames:
            context.black_frame_ranges = []
            return

        timestamps = [frame.timestamp_seconds for frame in context.sampled_frames]
        mean_luminances = []
        max_luminances = []
        for frame in context.sampled_frames:
            mean_luminance, max_luminance = compute_luminance_statistics_for_frame(
                frame.frame_bgr
            )
            mean_luminances.append(mean_luminance)
            max_luminances.append(max_luminance)

        context.black_frame_ranges = merge_black_frame_samples_into_ranges(
            timestamps, mean_luminances, max_luminances, self._thresholds
        )

    @property
    def stage_name(self) -> str:
        """Returns ``"black_frame_detection"``."""
        return "black_frame_detection"


class SceneCutDetectionStage(InspectionStage):
    """Detects scene-cut boundaries using an injected detection strategy.

    Design pattern: this stage is the **Strategy** pattern's client --
    it is constructed with any :class:`SceneDetectionStrategy`
    implementation and drives it uniformly, without knowing whether it
    is running histogram-difference or SSIM analysis underneath.

    Algorithmic notes:
        Computes one dissimilarity score per consecutive sampled-frame
        pair (O(n-1) calls to the strategy, each O(H*W) or a small
        constant multiple thereof) plus one motion-complexity score per
        frame (O(n) calls, each O(H*W) for the Canny pass), then
        delegates to
        :func:`~ingestion_inspection_service.strategies.scene_detection.detect_scene_boundaries_via_adaptive_thresholding`
        to convert those raw scores into merged, confidence-scored
        :class:`SceneBoundary` objects.
    """

    def __init__(self, scene_detection_strategy: SceneDetectionStrategy) -> None:
        """Initializes the stage with a pluggable scene detection strategy.

        Args:
            scene_detection_strategy: The Strategy implementation
                (histogram-difference, SSIM, or any future algorithm
                conforming to the interface) to drive.
        """
        self._strategy = scene_detection_strategy

    def process(self, context: InspectionContext) -> None:
        """Computes scene boundaries and stores them on the context.

        Args:
            context: The shared inspection context; reads
                ``context.sampled_frames`` and
                ``context.duration_seconds``, writes
                ``context.scene_boundaries``.
        """
        frames = context.sampled_frames
        if len(frames) < 2 or context.duration_seconds <= 0:
            context.scene_boundaries = []
            return

        dissimilarity_scores = [
            self._strategy.compute_frame_pair_dissimilarity_score(
                frames[index].frame_bgr, frames[index + 1].frame_bgr
            )
            for index in range(len(frames) - 1)
        ]
        motion_complexity_scores = [
            compute_motion_complexity_score(frame.frame_bgr) for frame in frames
        ]
        timestamps = [frame.timestamp_seconds for frame in frames]

        context.scene_boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarity_scores,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=motion_complexity_scores,
            detection_strategy_name=self._strategy.strategy_name,
            fixed_cut_dissimilarity_threshold=self._strategy.fixed_cut_dissimilarity_threshold,
            asset_duration_seconds=context.duration_seconds,
        )

    @property
    def stage_name(self) -> str:
        """Returns ``"scene_cut_detection"``."""
        return "scene_cut_detection"


class ShotTypeClassificationStage(InspectionStage):
    """Classifies each detected scene's shot type (close-up/wide/dialogue/action).

    Design pattern: **Strategy** pattern's client, mirroring
    :class:`SceneCutDetectionStage` -- constructed with any
    :class:`~ingestion_inspection_service.strategies.shot_type_classification.ShotTypeClassificationStrategy`
    implementation and drives it uniformly.

    Not part of :func:`~ingestion_inspection_service.pipeline.pipeline.build_default_inspection_pipeline`'s
    default four-stage chain (to preserve the existing, documented
    ``pipeline_stage_names_executed`` contract for callers that don't
    ask for shot-type classification); opt in via
    :func:`~ingestion_inspection_service.pipeline.pipeline.build_inspection_pipeline_with_shot_type_classification`
    or by constructing an :class:`~ingestion_inspection_service.pipeline.pipeline.InspectionPipeline`
    with this stage included explicitly.

    Algorithmic notes:
        Requires ``context.scene_boundaries`` to already be populated
        (i.e. must run after :class:`SceneCutDetectionStage`). For each
        scene, selects the subset of ``context.sampled_frames`` whose
        timestamps fall within
        ``[scene.start_timestamp_seconds, scene.end_timestamp_seconds)``
        and delegates classification to the injected strategy. Because
        :class:`~ingestion_inspection_service.domain.models.SceneBoundary`
        is a frozen dataclass, the classified scene is produced via
        ``dataclasses.replace`` rather than in-place mutation. O(n) in
        the number of sampled frames to bucket them by scene, plus the
        strategy's own O(scene_frame_count * H * W) classification cost
        per scene.
    """

    def __init__(
        self, shot_type_classification_strategy: ShotTypeClassificationStrategy
    ) -> None:
        """Initializes the stage with a pluggable shot-type classification strategy.

        Args:
            shot_type_classification_strategy: The Strategy
                implementation to drive.
        """
        self._strategy = shot_type_classification_strategy

    def process(self, context: InspectionContext) -> None:
        """Classifies and rewrites each scene boundary with its shot type.

        Args:
            context: The shared inspection context; reads
                ``context.sampled_frames`` and
                ``context.scene_boundaries``, and overwrites
                ``context.scene_boundaries`` with copies carrying a
                populated ``shot_type`` field.
        """
        if not context.scene_boundaries:
            return

        classified_scenes = []
        for scene in context.scene_boundaries:
            scene_frames = [
                frame.frame_bgr
                for frame in context.sampled_frames
                if scene.start_timestamp_seconds
                <= frame.timestamp_seconds
                < scene.end_timestamp_seconds
            ]
            shot_type = self._strategy.classify_scene_frames(scene_frames)
            classified_scenes.append(dataclasses.replace(scene, shot_type=shot_type))

        context.scene_boundaries = classified_scenes

    @property
    def stage_name(self) -> str:
        """Returns ``"shot_type_classification"``."""
        return "shot_type_classification"


class ColorSpaceExtractionStage(InspectionStage):
    """Extracts container/stream metadata and color space/HDR profile.

    Algorithmic notes:
        Delegates entirely to the ffprobe subprocess adapter (see
        ``adapters.ffprobe_client``); no frame decode occurs in this
        stage. If ffprobe is unavailable or fails, the stage degrades
        gracefully by leaving the profiles as ``None`` rather than
        aborting the whole pipeline, so black-frame and scene-cut
        results (which don't depend on ffprobe) are still returned.
    """

    def process(self, context: InspectionContext) -> None:
        """Populates video/audio/color-space profiles on the context.

        Args:
            context: The shared inspection context; writes
                ``context.video_stream_profile``,
                ``context.audio_stream_profile``,
                ``context.color_space_profile``, and refines
                ``context.duration_seconds`` if it was previously
                unknown (0.0).
        """
        try:
            probed_metadata = extract_typed_stream_metadata_via_ffprobe(
                context.source_file_path
            )
        except FfprobeExecutionError:
            return

        context.video_stream_profile = probed_metadata.video_stream_profile
        context.audio_stream_profile = probed_metadata.audio_stream_profile
        context.color_space_profile = probed_metadata.color_space_profile
        if context.duration_seconds <= 0:
            context.duration_seconds = probed_metadata.duration_seconds

    @property
    def stage_name(self) -> str:
        """Returns ``"color_space_extraction"``."""
        return "color_space_extraction"


class LoudnessAnalysisStage(InspectionStage):
    """Runs the approximate EBU R128-inspired integrated loudness pass.

    Algorithmic notes:
        See :func:`compute_approximate_integrated_loudness_lufs` for
        the full documented algorithm and its explicit simplifications
        relative to full ITU-R BS.1770-4 / EBU R128 conformance.
    """

    def __init__(
        self,
        loudness_configuration: LoudnessAnalysisConfiguration = DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
    ) -> None:
        """Initializes the stage with loudness analysis configuration.

        Args:
            loudness_configuration: Target LUFS, tolerance, and gating
                thresholds.
        """
        self._configuration = loudness_configuration

    def process(self, context: InspectionContext) -> None:
        """Computes loudness statistics and stores them on the context.

        Args:
            context: The shared inspection context; reads
                ``context.raw_audio_pcm_samples`` and
                ``context.raw_audio_sample_rate_hz``, writes
                ``context.loudness_analysis``. Leaves
                ``loudness_analysis`` as ``None`` when no PCM samples
                are available (e.g. no audio track, or the caller chose
                not to extract audio).
        """
        if (
            context.raw_audio_pcm_samples is None
            or context.raw_audio_sample_rate_hz is None
            or context.raw_audio_pcm_samples.size == 0
        ):
            context.loudness_analysis = None
            return

        context.loudness_analysis = compute_approximate_integrated_loudness_lufs(
            context.raw_audio_pcm_samples,
            context.raw_audio_sample_rate_hz,
            self._configuration,
        )

    @property
    def stage_name(self) -> str:
        """Returns ``"loudness_analysis"``."""
        return "loudness_analysis"


def compute_luminance_statistics_for_frame(
    frame_bgr: np.ndarray,
) -> typing.Tuple[float, float]:
    """Computes mean and max grayscale luminance for one BGR frame.

    Args:
        frame_bgr: A single decoded frame, ``(H, W, 3)`` BGR ``uint8``.

    Returns:
        tuple[float, float]: ``(mean_luminance, max_luminance)`` on the
        0-255 scale.

    Raises:
        ValueError: If ``frame_bgr`` is not a 3-channel image array.

    Algorithmic notes:
        O(H*W): one grayscale conversion plus two reductions.
    """
    if frame_bgr.ndim != 3 or frame_bgr.shape[2] != 3:
        raise ValueError(
            f"compute_luminance_statistics_for_frame expects a (H, W, 3) BGR "
            f"frame, got shape {frame_bgr.shape}."
        )
    grayscale_frame = cv2.cvtColor(frame_bgr, cv2.COLOR_BGR2GRAY)
    mean_luminance = float(np.mean(grayscale_frame))
    max_luminance = float(np.max(grayscale_frame))
    return mean_luminance, max_luminance


def merge_black_frame_samples_into_ranges(
    frame_timestamps_seconds: typing.Sequence[float],
    per_frame_mean_luminance: typing.Sequence[float],
    per_frame_max_luminance: typing.Sequence[float],
    thresholds: BlackFrameDetectionThresholds = DEFAULT_BLACK_FRAME_DETECTION_THRESHOLDS,
) -> typing.List[BlackFrameRange]:
    """Classifies per-frame luminance samples and merges black runs into ranges.

    Pure function (no OpenCV/decode dependency) so it can be unit
    tested directly against synthetic luminance value lists.

    Args:
        frame_timestamps_seconds: Timestamp of each sampled frame,
            ascending.
        per_frame_mean_luminance: Mean luminance (0-255) of each
            sampled frame, same length/order as
            ``frame_timestamps_seconds``.
        per_frame_max_luminance: Max luminance (0-255) of each sampled
            frame, same length/order.
        thresholds: Black-frame classification thresholds.

    Returns:
        list[BlackFrameRange]: Ordered, merged black-frame ranges, each
        spanning at least ``thresholds.minimum_consecutive_black_frames``
        consecutive black-classified samples.

    Raises:
        ValueError: If the three input sequences have mismatched
            lengths.

    Algorithmic notes:
        Single linear pass, O(n) in the number of sampled frames.
    """
    frame_count = len(frame_timestamps_seconds)
    if len(per_frame_mean_luminance) != frame_count or (
        len(per_frame_max_luminance) != frame_count
    ):
        raise ValueError(
            "frame_timestamps_seconds, per_frame_mean_luminance, and "
            "per_frame_max_luminance must all have the same length; got "
            f"{frame_count}, {len(per_frame_mean_luminance)}, "
            f"{len(per_frame_max_luminance)}."
        )

    is_black_frame_flags = [
        mean_luminance < thresholds.mean_luminance_black_threshold
        and max_luminance < thresholds.maximum_luminance_black_threshold
        for mean_luminance, max_luminance in zip(
            per_frame_mean_luminance, per_frame_max_luminance
        )
    ]

    black_frame_ranges: typing.List[BlackFrameRange] = []
    run_start_index: typing.Optional[int] = None

    def flush_run(run_start: int, run_end_exclusive: int) -> None:
        run_length = run_end_exclusive - run_start
        if run_length < thresholds.minimum_consecutive_black_frames:
            return
        run_mean_luminance = float(
            np.mean(per_frame_mean_luminance[run_start:run_end_exclusive])
        )
        black_frame_ranges.append(
            BlackFrameRange(
                start_timestamp_seconds=frame_timestamps_seconds[run_start],
                end_timestamp_seconds=frame_timestamps_seconds[run_end_exclusive - 1],
                mean_luminance_during_range=run_mean_luminance,
                sampled_frame_count=run_length,
            )
        )

    for index, is_black in enumerate(is_black_frame_flags):
        if is_black and run_start_index is None:
            run_start_index = index
        elif not is_black and run_start_index is not None:
            flush_run(run_start_index, index)
            run_start_index = None

    if run_start_index is not None:
        flush_run(run_start_index, frame_count)

    return black_frame_ranges


def compute_approximate_integrated_loudness_lufs(
    pcm_samples: np.ndarray,
    sample_rate_hz: int,
    configuration: LoudnessAnalysisConfiguration = DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
) -> LoudnessAnalysisResult:
    """Estimates integrated program loudness using a simplified EBU R128 pass.

    This is a documented **approximation** of the ITU-R BS.1770-4
    algorithm that EBU R128 is built on, not a certified/conformant
    implementation. Simplifications relative to the full spec:

      * **No K-weighting pre-filter.** BS.1770 applies a two-stage
        K-weighting filter (a high-shelf stage approximating the
        head's acoustic effect, plus a high-pass stage) before power
        measurement. This implementation measures raw sample power
        directly, which under/over-states loudness for
        heavily bass- or treble-weighted program material relative to
        a conformant meter.
      * **No multi-channel weighting.** BS.1770 sums per-channel mean
        square power with channel-specific weights (e.g. +1.5 dB for
        surround channels). This implementation assumes a single
        (mono, or pre-summed) channel is supplied.
      * **True peak is a sample-peak approximation.** A conformant
        dBTP measurement requires 4x (or greater) oversampling to catch
        inter-sample peaks; this implementation reports the plain
        sample-peak level in dBFS-equivalent terms, which can
        under-estimate true inter-sample peaks by roughly 0.5-3 dB for
        typical program material.

    What IS implemented per the BS.1770/EBU R128 methodology:

      * 400 ms measurement blocks with 75% overlap (100 ms hop), the
        same blocking scheme as the spec.
      * Per-block loudness ``L_block = -0.691 + 10 * log10(mean_square)``
        (identical formula to the spec, minus the K-weighting).
      * The two-stage gating process: an absolute gate at
        ``configuration.gating_absolute_threshold_lufs`` (spec default
        -70 LUFS) followed by a relative gate at
        ``mean(absolute_gated_blocks) - configuration.gating_relative_threshold_lu``
        (spec default 10 LU below the ungated mean), with the final
        integrated loudness computed as the *energy* average (not the
        dB average) of the relative-gated blocks -- exactly as EBU R128
        specifies.
      * A loudness-range-like spread reported as the difference between
        the 95th and 10th percentile of the gated per-block loudness
        values, echoing (in simplified form) EBU Tech 3342's LRA
        methodology.

    Args:
        pcm_samples: 1-D NumPy array of audio samples, normalized to
            ``[-1.0, 1.0]`` float. Multi-channel audio must be
            pre-summed/averaged to mono by the caller before invoking
            this function.
        sample_rate_hz: Sample rate of ``pcm_samples``, in Hz.
        configuration: Target loudness, tolerance, and gating
            configuration.

    Returns:
        LoudnessAnalysisResult: The estimated integrated loudness,
        loudness range, true-peak estimate, and target conformance
        flag.

    Raises:
        ValueError: If ``pcm_samples`` is empty, not 1-D, or
            ``sample_rate_hz`` is not positive.

    Algorithmic notes:
        O(n) in the number of samples to build the blocks (each sample
        participates in at most 4 overlapping 400ms/100ms-hop blocks),
        plus O(b log b) for the percentile computation over ``b``
        blocks (``b << n``), negligible relative to the linear block
        construction pass.
    """
    if pcm_samples.ndim != 1:
        raise ValueError(
            f"pcm_samples must be a 1-D (mono) array, got shape "
            f"{pcm_samples.shape}. Pre-sum/average multi-channel audio first."
        )
    if pcm_samples.size == 0:
        raise ValueError("pcm_samples must not be empty.")
    if sample_rate_hz <= 0:
        raise ValueError(f"sample_rate_hz must be positive, got {sample_rate_hz}.")

    block_length_samples = int(round(0.400 * sample_rate_hz))
    hop_length_samples = int(round(0.100 * sample_rate_hz))
    block_length_samples = max(block_length_samples, 1)
    hop_length_samples = max(hop_length_samples, 1)

    per_block_mean_square: typing.List[float] = []
    for block_start in range(
        0, max(1, pcm_samples.size - block_length_samples + 1), hop_length_samples
    ):
        block = pcm_samples[block_start : block_start + block_length_samples]
        if block.size == 0:
            continue
        mean_square = float(np.mean(np.square(block.astype(np.float64))))
        per_block_mean_square.append(mean_square)

    if not per_block_mean_square:
        # Signal shorter than one block: treat the whole signal as one block.
        mean_square = float(np.mean(np.square(pcm_samples.astype(np.float64))))
        per_block_mean_square = [mean_square]

    per_block_loudness_lufs = [
        _mean_square_to_lufs(mean_square) for mean_square in per_block_mean_square
    ]

    absolute_gated_loudness = [
        loudness
        for loudness in per_block_loudness_lufs
        if loudness > configuration.gating_absolute_threshold_lufs
    ]
    if not absolute_gated_loudness:
        absolute_gated_loudness = per_block_loudness_lufs

    relative_gate_threshold = (
        float(np.mean(absolute_gated_loudness))
        - configuration.gating_relative_threshold_lu
    )
    relative_gated_loudness = [
        loudness
        for loudness in absolute_gated_loudness
        if loudness > relative_gate_threshold
    ]
    if not relative_gated_loudness:
        relative_gated_loudness = absolute_gated_loudness

    relative_gated_mean_square = [
        _lufs_to_mean_square(loudness) for loudness in relative_gated_loudness
    ]
    integrated_loudness_lufs = _mean_square_to_lufs(
        float(np.mean(relative_gated_mean_square))
    )

    loudness_range_lu = float(
        np.percentile(relative_gated_loudness, 95)
        - np.percentile(relative_gated_loudness, 10)
    )

    peak_absolute_sample_value = float(np.max(np.abs(pcm_samples)))
    true_peak_estimate_dbtp = (
        20.0 * np.log10(max(peak_absolute_sample_value, 1e-9))
    )

    conforms_to_target = (
        abs(integrated_loudness_lufs - configuration.streaming_platform_target_lufs)
        <= configuration.loudness_conformance_tolerance_lufs
    )

    return LoudnessAnalysisResult(
        integrated_loudness_lufs=integrated_loudness_lufs,
        loudness_range_lu=loudness_range_lu,
        true_peak_estimate_dbtp=float(true_peak_estimate_dbtp),
        conforms_to_target=conforms_to_target,
        target_lufs_used=configuration.streaming_platform_target_lufs,
    )


def _mean_square_to_lufs(mean_square: float) -> float:
    """Converts a block's mean-square power to a LUFS-style loudness value.

    Args:
        mean_square: Mean of squared sample values over a measurement
            block.

    Returns:
        float: ``-0.691 + 10 * log10(mean_square)``, the BS.1770 block
        loudness formula (absent K-weighting, per this module's
        documented simplification). Silence (``mean_square == 0``) maps
        to a very low but finite floor rather than ``-inf``.
    """
    safe_mean_square = max(mean_square, 1e-12)
    return -0.691 + 10.0 * float(np.log10(safe_mean_square))


def _lufs_to_mean_square(loudness_lufs: float) -> float:
    """Inverts :func:`_mean_square_to_lufs` back to mean-square power.

    Args:
        loudness_lufs: A block loudness value in LUFS.

    Returns:
        float: The mean-square power that would produce this loudness
        value under the BS.1770 block loudness formula.
    """
    return 10.0 ** ((loudness_lufs + 0.691) / 10.0)


def infer_color_space_summary_label(
    color_space_profile: typing.Optional[ColorSpaceProfile],
) -> str:
    """Produces a short human-readable label summarizing a color profile.

    Args:
        color_space_profile: The parsed color space profile, or
            ``None``.

    Returns:
        str: A short label such as ``"BT.2020 / HDR10"``,
        ``"BT.709 / SDR"``, or ``"unknown"`` when no profile is
        available.
    """
    if color_space_profile is None:
        return "unknown"

    gamut_label = "BT.2020" if color_space_profile.is_bt2020 else "BT.709"
    hdr_label = (
        "SDR"
        if color_space_profile.detected_hdr_format == HdrFormat.SDR_BT709
        else color_space_profile.detected_hdr_format.value
    )
    return f"{gamut_label} / {hdr_label}"
