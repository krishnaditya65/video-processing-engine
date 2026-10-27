"""Dataclass-based domain model for the Ingestion & Inspection Plane.

Design pattern: **Domain Model** (Fowler). Every type in this module is a
plain, immutable-by-default ``@dataclasses.dataclass`` with no behavior
beyond simple derived properties and serialization helpers. Keeping the
domain model free of I/O and third-party video-library dependencies
means:

  * It can be constructed directly in unit tests with synthetic data,
    with no real video file, ffprobe binary, or OpenCV import required.
  * It can be serialized to the "structural map" JSON artifact via
    ``dataclasses.asdict`` plus a small amount of enum/float handling.
  * It forms a stable contract between the inspection pipeline (which
    produces it) and the FastAPI/CLI layers (which serialize it) and
    the downstream Go orchestrator (which consumes the JSON).

All durations/timestamps are expressed in seconds (float) measured from
the start of the asset's presentation timeline, matching the convention
ffprobe and most NLE/QC tools use.
"""

from __future__ import annotations

import dataclasses
import enum
import json
import typing


class ShotType(str, enum.Enum):
    """Enumerates the coarse cinematographic shot categories we classify.

    Values are inferred by
    ``strategies.shot_type_classification.RuleBasedShotTypeClassificationStrategy``
    from a purely rule-based blend of frame-to-frame motion magnitude and
    Canny/Sobel edge density -- no ML model/training is involved. See
    that module's docstring for the exact thresholds and rationale.
    """

    CLOSE_UP = "CLOSE_UP"
    WIDE = "WIDE"
    DIALOGUE = "DIALOGUE"
    ACTION = "ACTION"
    UNKNOWN = "UNKNOWN"


class HdrFormat(str, enum.Enum):
    """Enumerates the dynamic-range / HDR signalling formats we can infer.

    The values are heuristically inferred from ffprobe stream metadata
    (``color_transfer``, ``color_primaries``, side-data list, and codec
    tag/profile fields) rather than from a fully spec-compliant SEI/NAL
    parse. See ``adapters.ffprobe_client`` for the exact field-to-value
    mapping and its documented limitations.
    """

    SDR_BT709 = "SDR_BT709"
    HDR10 = "HDR10"
    HDR10_PLUS = "HDR10_PLUS"
    DOLBY_VISION = "DOLBY_VISION"
    HLG = "HLG"
    UNKNOWN = "UNKNOWN"


@dataclasses.dataclass(frozen=True)
class SceneBoundary:
    """A single detected scene-cut boundary and the shot that follows it.

    Attributes:
        scene_index: Zero-based ordinal position of this scene within
            the asset's ordered scene list.
        start_timestamp_seconds: Presentation timestamp, in seconds,
            where this scene begins (i.e. where the preceding cut was
            detected, or 0.0 for the first scene).
        end_timestamp_seconds: Presentation timestamp, in seconds, where
            this scene ends (i.e. where the next cut was detected, or
            the asset duration for the final scene).
        detection_strategy_name: Name of the
            ``SceneDetectionStrategy`` implementation that produced this
            boundary (e.g. ``"histogram_difference"`` or
            ``"structural_similarity"``), recorded for auditability.
        cut_confidence_score: A [0, 1] heuristic confidence that this is
            a genuine hard cut rather than a fast pan/whip or lighting
            change. Derived from how far the triggering delta exceeded
            the active threshold.
        motion_complexity_score: A non-negative heuristic score
            describing how visually complex/high-motion this scene is,
            combining frame-to-frame pixel delta magnitude and Canny
            edge density. Higher values suggest this scene will be more
            expensive to encode well (informs downstream bitrate ladder
            decisions).
        shot_type: Best-effort :class:`ShotType` classification of this
            scene (close-up / wide / dialogue / action), or ``None``
            when :class:`~ingestion_inspection_service.pipeline.stages.ShotTypeClassificationStage`
            has not run. Optional and defaulted so existing callers
            that construct a ``SceneBoundary`` without shot-type
            information continue to work unchanged.

    Raises:
        ValueError: If constructed with an end timestamp at or before
            the start timestamp, which would describe a zero or
            negative-duration scene.
    """

    scene_index: int
    start_timestamp_seconds: float
    end_timestamp_seconds: float
    detection_strategy_name: str
    cut_confidence_score: float
    motion_complexity_score: float
    shot_type: typing.Optional[ShotType] = None

    def __post_init__(self) -> None:
        """Validates temporal ordering invariants at construction time."""
        if self.end_timestamp_seconds <= self.start_timestamp_seconds:
            raise ValueError(
                "SceneBoundary end_timestamp_seconds "
                f"({self.end_timestamp_seconds}) must be strictly greater "
                f"than start_timestamp_seconds ({self.start_timestamp_seconds}) "
                f"for scene_index={self.scene_index}."
            )

    @property
    def duration_seconds(self) -> float:
        """Returns the scene's duration in seconds.

        Returns:
            float: ``end_timestamp_seconds - start_timestamp_seconds``.
        """
        return self.end_timestamp_seconds - self.start_timestamp_seconds


@dataclasses.dataclass(frozen=True)
class BlackFrameRange:
    """A contiguous time range where sampled frames were judged "black".

    Attributes:
        start_timestamp_seconds: Start of the black range, in seconds.
        end_timestamp_seconds: End of the black range, in seconds
            (inclusive of the last black sample's timestamp).
        mean_luminance_during_range: Average of the per-frame mean luma
            values observed across the black range, useful for
            distinguishing near-black fades from true crush-black.
        sampled_frame_count: How many analysis-sampled frames fell
            within this range and were classified as black.
    """

    start_timestamp_seconds: float
    end_timestamp_seconds: float
    mean_luminance_during_range: float
    sampled_frame_count: int

    @property
    def duration_seconds(self) -> float:
        """Returns the duration, in seconds, of this black-frame range."""
        return self.end_timestamp_seconds - self.start_timestamp_seconds


@dataclasses.dataclass(frozen=True)
class VideoStreamProfile:
    """Typed projection of ffprobe's video stream JSON fields we care about.

    Attributes:
        codec_name: e.g. ``"h264"``, ``"hevc"``, ``"prores"``.
        width_pixels: Frame width in pixels.
        height_pixels: Frame height in pixels.
        frames_per_second: Nominal (average) frame rate, as a float,
            parsed from ffprobe's ``r_frame_rate`` fraction string.
        pixel_format: ffprobe ``pix_fmt`` string, e.g. ``"yuv420p10le"``.
        bit_depth: Inferred sample bit depth (8, 10, or 12), derived
            from ``pixel_format`` when ffprobe doesn't expose it
            directly.
        color_range: ffprobe ``color_range`` (``"tv"``/limited or
            ``"pc"``/full), when present.
    """

    codec_name: str
    width_pixels: int
    height_pixels: int
    frames_per_second: float
    pixel_format: str
    bit_depth: int
    color_range: typing.Optional[str] = None


@dataclasses.dataclass(frozen=True)
class AudioStreamProfile:
    """Typed projection of ffprobe's audio stream JSON fields we care about.

    Attributes:
        codec_name: e.g. ``"pcm_s24le"``, ``"aac"``, ``"eac3"``.
        sample_rate_hz: Audio sample rate in Hz.
        channel_count: Number of discrete audio channels.
        channel_layout: ffprobe channel layout string, e.g.
            ``"5.1"``/``"stereo"``, when present.
    """

    codec_name: str
    sample_rate_hz: int
    channel_count: int
    channel_layout: typing.Optional[str] = None


@dataclasses.dataclass(frozen=True)
class ColorSpaceProfile:
    """Describes the color space / HDR characteristics of the video stream.

    Attributes:
        color_primaries: ffprobe ``color_primaries`` value, e.g.
            ``"bt709"`` or ``"bt2020"``.
        color_transfer_characteristics: ffprobe ``color_transfer``
            value, e.g. ``"bt709"``, ``"smpte2084"`` (PQ), or
            ``"arib-std-b67"`` (HLG).
        color_space_matrix: ffprobe ``color_space`` value describing the
            YCbCr matrix, e.g. ``"bt709"`` or ``"bt2020nc"``.
        is_bt2020: True when primaries indicate the wide-gamut BT.2020
            color space rather than BT.709.
        detected_hdr_format: Best-effort :class:`HdrFormat` inference.
        has_mastering_display_metadata: Whether ffprobe reported SMPTE
            ST 2086 mastering display color volume side data, a strong
            signal of properly authored HDR10 content.
        has_content_light_level_metadata: Whether ffprobe reported
            content light level (MaxCLL/MaxFALL) side data.
    """

    color_primaries: typing.Optional[str]
    color_transfer_characteristics: typing.Optional[str]
    color_space_matrix: typing.Optional[str]
    is_bt2020: bool
    detected_hdr_format: HdrFormat
    has_mastering_display_metadata: bool
    has_content_light_level_metadata: bool


@dataclasses.dataclass(frozen=True)
class LoudnessAnalysisResult:
    """Result of the approximate EBU R128-inspired LUFS loudness pass.

    See ``pipeline.stages.LoudnessAnalysisStage`` for the full algorithm
    description and its documented simplifications relative to the full
    ITU-R BS.1770 / EBU R128 specification.

    Attributes:
        integrated_loudness_lufs: Estimated programme (integrated)
            loudness across the whole analyzed signal, in LUFS.
        loudness_range_lu: Estimated loudness range (LRA-like spread
            between quiet and loud passages), in loudness units.
        true_peak_estimate_dbtp: Estimated true-peak level in dBTP,
            approximated from sample peak without oversampling (see
            stage docstring for the resulting under-estimate risk).
        conforms_to_target: Whether ``integrated_loudness_lufs`` falls
            within the configured tolerance of the platform's target
            loudness (see
            ``config.LoudnessAnalysisConfiguration``).
        target_lufs_used: The target LUFS value this result was
            evaluated against, recorded for auditability.
    """

    integrated_loudness_lufs: float
    loudness_range_lu: float
    true_peak_estimate_dbtp: float
    conforms_to_target: bool
    target_lufs_used: float


@dataclasses.dataclass
class StructuralMap:
    """The complete "structural map" artifact produced for one asset.

    This is the top-level output object serialized to JSON as
    ``<asset_id>.structural_map.json`` and returned by both the CLI tool
    and the ``POST /assets/{assetId}/inspect`` HTTP endpoint.

    Attributes:
        asset_id: Opaque identifier for the ingested asset, as assigned
            by the upstream ingestion/orchestration system.
        source_file_path: Filesystem path (or URI) of the analyzed
            mezzanine file.
        duration_seconds: Total asset duration, in seconds, as reported
            by ffprobe's format-level ``duration`` field.
        video_stream_profile: Parsed primary video stream metadata, or
            ``None`` if the asset has no video stream.
        audio_stream_profile: Parsed primary audio stream metadata, or
            ``None`` if the asset has no audio stream.
        color_space_profile: Parsed color space / HDR profile, or
            ``None`` if there is no video stream to derive it from.
        scene_boundaries: Ordered list of detected scenes, covering
            ``[0, duration_seconds)`` with no gaps or overlaps.
        black_frame_ranges: Ordered list of detected black-frame time
            ranges.
        loudness_analysis: Result of the loudness pass, or ``None`` if
            the asset has no audio stream to analyze.
        frame_sampling_rate_used_fps: The analysis frame-sampling rate
            actually used to produce this map, recorded for
            auditability/reproducibility.
        pipeline_stage_names_executed: Ordered list of pipeline stage
            names that ran, in execution order, useful for debugging
            partial pipelines.
        analysis_schema_version: Semantic version of this JSON schema,
            bumped whenever a breaking field change is made, so the Go
            orchestrator can detect incompatible payloads.
    """

    asset_id: str
    source_file_path: str
    duration_seconds: float
    video_stream_profile: typing.Optional[VideoStreamProfile]
    audio_stream_profile: typing.Optional[AudioStreamProfile]
    color_space_profile: typing.Optional[ColorSpaceProfile]
    scene_boundaries: typing.List[SceneBoundary] = dataclasses.field(
        default_factory=list
    )
    black_frame_ranges: typing.List[BlackFrameRange] = dataclasses.field(
        default_factory=list
    )
    loudness_analysis: typing.Optional[LoudnessAnalysisResult] = None
    frame_sampling_rate_used_fps: float = 1.0
    pipeline_stage_names_executed: typing.List[str] = dataclasses.field(
        default_factory=list
    )
    analysis_schema_version: str = "1.0.0"

    @property
    def total_scene_count(self) -> int:
        """Returns the number of detected scenes in this structural map."""
        return len(self.scene_boundaries)

    @property
    def total_black_frame_range_count(self) -> int:
        """Returns the number of detected black-frame ranges."""
        return len(self.black_frame_ranges)

    def to_json_serializable_dict(self) -> typing.Dict[str, typing.Any]:
        """Converts this structural map to a plain, JSON-serializable dict.

        ``dataclasses.asdict`` alone is insufficient because
        :class:`HdrFormat` is an ``enum.Enum`` subclass; this method
        walks the nested structure and converts enum members to their
        ``.value`` string so the result round-trips cleanly through
        ``json.dumps``/``json.loads`` without a custom ``default=``
        hook.

        Returns:
            dict: A nested dict/list/primitive structure safe to pass
            directly to ``json.dumps``.

        Algorithmic notes:
            O(n) in the total number of scalar fields across all nested
            dataclasses; negligible relative to the analysis work that
            produced this object.
        """
        raw_dict = dataclasses.asdict(self)
        return _convert_enums_to_values_recursively(raw_dict)

    def to_json_string(self, *, indent: int = 2) -> str:
        """Serializes this structural map to a pretty-printed JSON string.

        Args:
            indent: Number of spaces to indent nested JSON structures
                by. Defaults to 2 for human-readable artifact files.

        Returns:
            str: The JSON-encoded structural map.
        """
        return json.dumps(self.to_json_serializable_dict(), indent=indent)


@dataclasses.dataclass(frozen=True)
class InspectionRequest:
    """Parameters requesting an inspection run over a specific asset.

    Used as the shared input contract between the CLI argument parser
    and the FastAPI request schema, so both entry points funnel into
    the exact same pipeline invocation code path.

    Attributes:
        asset_id: Opaque identifier for the asset being inspected.
        source_file_path: Filesystem path to the mezzanine file to
            analyze.
        analysis_frames_per_second: Override for the frame sampling
            rate; ``None`` means "use the configured default".
    """

    asset_id: str
    source_file_path: str
    analysis_frames_per_second: typing.Optional[float] = None


def _convert_enums_to_values_recursively(value: typing.Any) -> typing.Any:
    """Recursively replaces ``enum.Enum`` members with their ``.value``.

    Args:
        value: An arbitrarily nested combination of dicts, lists,
            enums, and JSON-primitive scalars, as produced by
            ``dataclasses.asdict``.

    Returns:
        Any: The same structure with every ``enum.Enum`` instance
        replaced by its underlying value.

    Algorithmic notes:
        Single recursive pass, O(n) in total nested element count.
    """
    if isinstance(value, enum.Enum):
        return value.value
    if isinstance(value, dict):
        return {
            key: _convert_enums_to_values_recursively(nested_value)
            for key, nested_value in value.items()
        }
    if isinstance(value, list):
        return [_convert_enums_to_values_recursively(item) for item in value]
    return value
