"""Centralized configuration constants and tunable thresholds.

This module exists so that every "magic number" used across the scene
detection strategies, black-frame heuristics, color-space heuristics and
loudness estimation has a single, documented, overridable home. Values
here are defaults; callers (CLI flags, FastAPI request body fields) may
override them per-invocation without touching this module.

Algorithmic notes
------------------
The thresholds below are empirically reasonable starting points drawn
from common broadcast/OTT QC tooling conventions (e.g. black level
checks around luma < 16/255 on a full-range 8-bit frame, EBU R128
target loudness of -23 LUFS for broadcast, -14 to -16 LUFS commonly
used as streaming platform normalization targets). They are not derived
from a single canonical spec because different platforms target
different loudness levels; the constants make the assumed target
explicit and easy to change.
"""

from __future__ import annotations

import dataclasses


@dataclasses.dataclass(frozen=True)
class FrameSamplingConfiguration:
    """Controls how densely the source video is decoded for analysis.

    Decoding every single frame of a multi-hour 4K mezzanine file is
    prohibitively expensive for a pre-transcode inspection pass (see the
    README performance section for the full rate-distortion-style
    tradeoff discussion). Instead we decode at a fixed sampling rate
    (frames-per-second of *analysis*, not of playback) and treat the
    sampled frames as a discrete-time approximation of the continuous
    video signal for the purposes of scene-cut, black-frame and motion
    analysis.

    Attributes:
        target_analysis_frames_per_second: How many frames per second of
            source-timeline duration to decode and analyze. A value of
            1.0 means "decode one frame per second of video". Lower
            values trade temporal resolution (risk of missing very short
            shots or transient black flashes) for large speedups on long
            assets.
        maximum_frames_to_sample: Hard cap on the number of frames ever
            pulled into memory for a single inspection run, to bound
            worst-case memory usage regardless of asset duration.
    """

    target_analysis_frames_per_second: float = 1.0
    maximum_frames_to_sample: int = 20_000


@dataclasses.dataclass(frozen=True)
class SceneCutDetectionThresholds:
    """Thresholds governing when a frame-to-frame delta is a scene cut.

    Attributes:
        histogram_correlation_cut_threshold: Below this normalized
            correlation coefficient (Pearson-style, range [-1, 1], where
            1.0 means identical histograms) between two consecutive
            sampled frames' luma histograms, a cut is declared.
        structural_similarity_cut_threshold: Below this SSIM index
            (range [0, 1], where 1.0 means structurally identical) a cut
            is declared.
        adaptive_threshold_window_size: Number of preceding deltas used
            to compute a rolling mean/std for the adaptive threshold
            strategy (mean - k * std crossing detection).
        adaptive_threshold_sensitivity_multiplier: The "k" in
            mean - k * std used by the adaptive thresholding pass.
        minimum_scene_duration_seconds: Minimum allowed duration between
            two consecutive detected cuts; candidate cuts closer than
            this are merged to suppress flicker/false positives.
    """

    histogram_correlation_cut_threshold: float = 0.65
    structural_similarity_cut_threshold: float = 0.60
    adaptive_threshold_window_size: int = 15
    adaptive_threshold_sensitivity_multiplier: float = 2.5
    minimum_scene_duration_seconds: float = 0.5


@dataclasses.dataclass(frozen=True)
class BlackFrameDetectionThresholds:
    """Thresholds governing black-frame / fade-to-black detection.

    Attributes:
        mean_luminance_black_threshold: A sampled frame is considered
            "black" when its mean luminance (0-255 scale, assuming an
            8-bit luma plane) falls below this value.
        maximum_luminance_black_threshold: A frame is considered "black"
            only if additionally its peak (max) luminance is also below
            this value, which prevents classifying a mostly-black frame
            containing a few bright highlights (e.g. titles, a
            flashlight) as a true black frame.
        minimum_consecutive_black_frames: Minimum number of consecutive
            sampled black frames required before we report a black-frame
            *range* (suppresses single-sample noise).
    """

    mean_luminance_black_threshold: float = 16.0
    maximum_luminance_black_threshold: float = 32.0
    minimum_consecutive_black_frames: int = 2


@dataclasses.dataclass(frozen=True)
class LoudnessAnalysisConfiguration:
    """Configuration for the simplified/approximate EBU R128 LUFS pass.

    Attributes:
        streaming_platform_target_lufs: The integrated loudness target
            (in LUFS) this platform normalizes delivered audio to. -16.0
            LUFS is a commonly used mobile/streaming target; broadcast
            commonly uses -23.0 LUFS per EBU R128.
        loudness_conformance_tolerance_lufs: How many LUFS of deviation
            from the target is considered "in spec" without flagging the
            asset for renormalization.
        gating_absolute_threshold_lufs: The EBU R128 absolute silence
            gate; blocks quieter than this (in LUFS, approximately -70)
            are excluded from the loudness integration entirely.
        gating_relative_threshold_lu: The EBU R128 relative gate,
            expressed in loudness units (LU) below the ungated mean;
            blocks quieter than (ungated_mean - this value) are excluded
            from the final relative-gated integration pass.
    """

    streaming_platform_target_lufs: float = -16.0
    loudness_conformance_tolerance_lufs: float = 1.0
    gating_absolute_threshold_lufs: float = -70.0
    gating_relative_threshold_lu: float = 10.0


@dataclasses.dataclass(frozen=True)
class ShotTypeClassificationThresholds:
    """Thresholds governing the rule-based close-up/wide/dialogue/action split.

    See ``strategies.shot_type_classification`` for the full algorithm.
    Both signals are normalized to an approximate ``[0, 1]`` range before
    comparison against these thresholds:

      * ``motion_magnitude`` -- mean absolute grayscale frame difference
        between consecutive sampled frames within the scene, divided by
        255 (8-bit dynamic range).
      * ``edge_density`` -- mean fraction of Canny-flagged edge pixels
        across the scene's sampled frames (same signal used by
        ``strategies.scene_detection.compute_motion_complexity_score``,
        recomputed here as a scene-average rather than a per-frame
        score).

    Attributes:
        action_motion_magnitude_threshold: At or above this normalized
            motion magnitude, a scene is classified ACTION regardless of
            edge density -- fast camera/subject movement is the
            strongest, least ambiguous signal of an action shot.
        close_up_edge_density_threshold: At or above this edge density,
            combined with motion below
            ``low_motion_magnitude_threshold``, a scene is classified
            CLOSE_UP -- close framing on a face/subject tends to fill
            the frame with fine, high-frequency detail (skin texture,
            hair, small props) while the camera itself is comparatively
            static.
        low_motion_magnitude_threshold: At or below this normalized
            motion magnitude, a scene is considered "static enough" to
            be a candidate for CLOSE_UP or DIALOGUE rather than WIDE or
            ACTION.

    Rationale for the ordering close-up > dialogue > wide > action is
    documented in
    ``strategies.shot_type_classification.RuleBasedShotTypeClassificationStrategy``.
    """

    action_motion_magnitude_threshold: float = 0.08
    close_up_edge_density_threshold: float = 0.16
    low_motion_magnitude_threshold: float = 0.03


@dataclasses.dataclass(frozen=True)
class PerceptualHashConfiguration:
    """Configuration for the difference-hash (dHash) perceptual hashing pass.

    Attributes:
        hash_grid_size: The hash is computed over a
            ``(hash_grid_size, hash_grid_size + 1)`` downscaled grayscale
            grid (see ``hashing.perceptual_hash`` for the exact
            algorithm), producing a
            ``hash_grid_size * hash_grid_size``-bit hash. 8 yields a
            standard 64-bit dHash, a common tradeoff between
            discriminative power and hash size.
        near_duplicate_hamming_distance_threshold: Two assets/frames are
            considered near-duplicates when the Hamming distance between
            their dHash values is at or below this value (out of
            ``hash_grid_size * hash_grid_size`` total bits). 8 was
            chosen as roughly 12.5% of a 64-bit hash, a commonly cited
            practical threshold in perceptual-hashing literature/tooling
            for "likely the same or a lightly re-encoded/cropped image".
    """

    hash_grid_size: int = 8
    near_duplicate_hamming_distance_threshold: int = 8


@dataclasses.dataclass(frozen=True)
class SpriteSheetGenerationConfiguration:
    """Configuration for scene-thumbnail sprite-sheet generation.

    Attributes:
        thumbnail_width_pixels: Width, in pixels, each per-scene
            representative frame is resized to before compositing.
        thumbnail_height_pixels: Height, in pixels, each per-scene
            representative frame is resized to before compositing.
        sprite_sheet_columns: Number of thumbnail columns per sprite
            sheet row; rows wrap automatically once this is exceeded.
    """

    thumbnail_width_pixels: int = 160
    thumbnail_height_pixels: int = 90
    sprite_sheet_columns: int = 10


DEFAULT_FRAME_SAMPLING_CONFIGURATION = FrameSamplingConfiguration()
DEFAULT_SCENE_CUT_DETECTION_THRESHOLDS = SceneCutDetectionThresholds()
DEFAULT_BLACK_FRAME_DETECTION_THRESHOLDS = BlackFrameDetectionThresholds()
DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION = LoudnessAnalysisConfiguration()
DEFAULT_SHOT_TYPE_CLASSIFICATION_THRESHOLDS = ShotTypeClassificationThresholds()
DEFAULT_PERCEPTUAL_HASH_CONFIGURATION = PerceptualHashConfiguration()
DEFAULT_SPRITE_SHEET_GENERATION_CONFIGURATION = SpriteSheetGenerationConfiguration()
