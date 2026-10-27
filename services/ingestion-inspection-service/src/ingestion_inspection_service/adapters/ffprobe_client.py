"""ffprobe subprocess adapter for container/stream metadata extraction.

Invokes the system ``ffprobe`` binary (part of the FFmpeg project) with
``-print_format json -show_format -show_streams`` and parses its JSON
output into the typed domain dataclasses
(:class:`~ingestion_inspection_service.domain.models.VideoStreamProfile`,
:class:`~ingestion_inspection_service.domain.models.AudioStreamProfile`,
:class:`~ingestion_inspection_service.domain.models.ColorSpaceProfile`).

Why shell out to ffprobe rather than use a Python container-parsing
library: ffprobe is the de-facto industry-standard, battle-tested
demuxer/metadata-probe for essentially every professional mezzanine
container and codec (MXF, MOV/ProRes, MP4/H.264/HEVC, side-data such as
Dolby Vision RPU and HDR10+ dynamic metadata, etc.), and matching its
container-format coverage in pure Python would be both redundant and a
correctness risk. This adapter's job is narrowly to (a) invoke it
safely, (b) parse its documented JSON schema, and (c) apply a small,
explicitly-documented set of heuristics to classify HDR format from the
fields ffprobe exposes.

HDR detection heuristic (documented limitation)
------------------------------------------------
ffprobe's ``-show_streams`` JSON does not always expose a single
authoritative "this is Dolby Vision" boolean; detection here is
heuristic, based on:

  * ``color_transfer == "smpte2084"`` (PQ transfer function) plus the
    presence of ``side_data_list`` entries of type
    ``"Mastering display metadata"`` and/or
    ``"Content light level metadata"`` => classified as HDR10.
  * A ``side_data_list`` entry (or codec tag / stream disposition) whose
    type/string mentions ``"DOVI"`` or ``"Dolby Vision"`` => classified
    as DOLBY_VISION. (Full RPU/EL parsing is out of scope; this is a
    presence heuristic, not a conformance validator.)
  * A ``side_data_list`` entry mentioning ``"HDR10+"`` or
    ``"hdr10plus"`` (as exposed by newer ffprobe builds via
    ``-show_frames`` extraction, not always present at the stream
    level) => classified as HDR10_PLUS. Because this metadata is
    frequently frame-level rather than stream-level, the absence of
    this signal at the stream level does NOT prove the asset lacks
    HDR10+; it is a best-effort upgrade over plain HDR10 when present.
  * ``color_transfer == "arib-std-b67"`` => classified as HLG.
  * Otherwise, if ``color_primaries``/``color_space`` indicate BT.2020
    with an unrecognized transfer function => still SDR_BT709 fallback
    is inappropriate, so this is reported as HDR10 only when PQ is also
    present; a BT.2020 stream with BT.709-like transfer is reported as
    UNKNOWN rather than guessed, to avoid false precision.

This module deliberately reports what it can determine and encodes
uncertainty as :data:`HdrFormat.UNKNOWN` rather than guessing, per the
project's "no unverified claims" documentation standard.
"""

from __future__ import annotations

import dataclasses
import json
import subprocess
import typing

from ingestion_inspection_service.domain.models import (
    AudioStreamProfile,
    ColorSpaceProfile,
    HdrFormat,
    VideoStreamProfile,
)


class FfprobeExecutionError(RuntimeError):
    """Raised when the ``ffprobe`` subprocess fails or is unavailable.

    Attributes:
        source_file_path: The media file path that was being probed.
        underlying_error: The original exception (e.g.
            ``FileNotFoundError`` if ffprobe is not installed, or
            ``subprocess.CalledProcessError`` if it exited non-zero).
    """

    def __init__(self, source_file_path: str, underlying_error: Exception) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file being probed.
            underlying_error: The original exception raised during
                subprocess execution or JSON parsing.
        """
        super().__init__(
            f"ffprobe failed while probing '{source_file_path}': "
            f"{underlying_error!r}"
        )
        self.source_file_path = source_file_path
        self.underlying_error = underlying_error


@dataclasses.dataclass(frozen=True)
class ProbedContainerMetadata:
    """Container-level result bundle returned by the ffprobe adapter.

    Attributes:
        duration_seconds: Total container duration in seconds, parsed
            from ``format.duration``.
        video_stream_profile: Parsed primary video stream, or ``None``.
        audio_stream_profile: Parsed primary audio stream, or ``None``.
        color_space_profile: Parsed color space/HDR profile, or
            ``None`` if there is no video stream.
        raw_ffprobe_json: The full, unmodified parsed ffprobe JSON
            document, retained for debugging/forward-compatibility so
            callers needing a field this adapter doesn't yet surface
            can still access it.
    """

    duration_seconds: float
    video_stream_profile: typing.Optional[VideoStreamProfile]
    audio_stream_profile: typing.Optional[AudioStreamProfile]
    color_space_profile: typing.Optional[ColorSpaceProfile]
    raw_ffprobe_json: typing.Dict[str, typing.Any]


def extract_typed_stream_metadata_via_ffprobe(
    source_file_path: str,
    *,
    ffprobe_executable_path: str = "ffprobe",
    subprocess_timeout_seconds: float = 60.0,
) -> ProbedContainerMetadata:
    """Runs ffprobe on a media file and parses it into typed dataclasses.

    Args:
        source_file_path: Filesystem path to the media file to probe.
        ffprobe_executable_path: Path/name of the ffprobe binary to
            invoke; defaults to ``"ffprobe"`` (resolved via ``PATH``).
        subprocess_timeout_seconds: Maximum time to allow the ffprobe
            subprocess to run before raising.

    Returns:
        ProbedContainerMetadata: Typed, parsed container/stream
        metadata.

    Raises:
        FfprobeExecutionError: If the ffprobe binary cannot be found,
            exits with a non-zero status, times out, or emits output
            that cannot be parsed as JSON.

    Algorithmic notes:
        O(1) in this process's own work; wall-clock cost is dominated
        by ffprobe's own container parse, which for a well-formed
        mezzanine file's header/index is typically sub-second
        regardless of asset duration (ffprobe does not decode frame
        payloads for ``-show_format``/``-show_streams``).
    """
    command = [
        ffprobe_executable_path,
        "-v",
        "quiet",
        "-print_format",
        "json",
        "-show_format",
        "-show_streams",
        source_file_path,
    ]
    try:
        completed_process = subprocess.run(
            command,
            capture_output=True,
            check=True,
            timeout=subprocess_timeout_seconds,
            text=True,
        )
        parsed_json = json.loads(completed_process.stdout)
    except (
        OSError,
        subprocess.CalledProcessError,
        subprocess.TimeoutExpired,
        json.JSONDecodeError,
    ) as error:
        raise FfprobeExecutionError(source_file_path, error) from error

    return _parse_ffprobe_json_document(parsed_json)


def _parse_ffprobe_json_document(
    ffprobe_document: typing.Dict[str, typing.Any],
) -> ProbedContainerMetadata:
    """Parses a raw ffprobe JSON document into typed dataclasses.

    Args:
        ffprobe_document: The already-JSON-decoded ffprobe output,
            expected to contain top-level ``"format"`` and
            ``"streams"`` keys per ffprobe's documented schema.

    Returns:
        ProbedContainerMetadata: The parsed, typed result.
    """
    format_section = ffprobe_document.get("format", {})
    streams = ffprobe_document.get("streams", [])

    duration_seconds = float(format_section.get("duration", 0.0) or 0.0)

    video_stream_json = _find_first_stream_by_codec_type(streams, "video")
    audio_stream_json = _find_first_stream_by_codec_type(streams, "audio")

    video_stream_profile = (
        _parse_video_stream_profile(video_stream_json)
        if video_stream_json is not None
        else None
    )
    audio_stream_profile = (
        _parse_audio_stream_profile(audio_stream_json)
        if audio_stream_json is not None
        else None
    )
    color_space_profile = (
        _parse_color_space_profile(video_stream_json)
        if video_stream_json is not None
        else None
    )

    return ProbedContainerMetadata(
        duration_seconds=duration_seconds,
        video_stream_profile=video_stream_profile,
        audio_stream_profile=audio_stream_profile,
        color_space_profile=color_space_profile,
        raw_ffprobe_json=ffprobe_document,
    )


def _find_first_stream_by_codec_type(
    streams: typing.List[typing.Dict[str, typing.Any]], codec_type: str
) -> typing.Optional[typing.Dict[str, typing.Any]]:
    """Returns the first stream entry matching the given ``codec_type``.

    Args:
        streams: The ``"streams"`` list from the ffprobe JSON document.
        codec_type: Either ``"video"`` or ``"audio"``.

    Returns:
        dict or None: The first matching stream's JSON object, or
        ``None`` if no stream of that type is present.
    """
    for stream in streams:
        if stream.get("codec_type") == codec_type:
            return stream
    return None


def _parse_video_stream_profile(
    video_stream_json: typing.Dict[str, typing.Any],
) -> VideoStreamProfile:
    """Parses a single ffprobe video-stream JSON object.

    Args:
        video_stream_json: A ``streams[i]`` entry with
            ``codec_type == "video"``.

    Returns:
        VideoStreamProfile: The parsed, typed video stream metadata.
    """
    pixel_format = video_stream_json.get("pix_fmt", "unknown")
    frames_per_second = _parse_ffprobe_fraction_string(
        video_stream_json.get("r_frame_rate", "0/1")
    )
    return VideoStreamProfile(
        codec_name=video_stream_json.get("codec_name", "unknown"),
        width_pixels=int(video_stream_json.get("width", 0) or 0),
        height_pixels=int(video_stream_json.get("height", 0) or 0),
        frames_per_second=frames_per_second,
        pixel_format=pixel_format,
        bit_depth=_infer_bit_depth_from_pixel_format(pixel_format),
        color_range=video_stream_json.get("color_range"),
    )


def _parse_audio_stream_profile(
    audio_stream_json: typing.Dict[str, typing.Any],
) -> AudioStreamProfile:
    """Parses a single ffprobe audio-stream JSON object.

    Args:
        audio_stream_json: A ``streams[i]`` entry with
            ``codec_type == "audio"``.

    Returns:
        AudioStreamProfile: The parsed, typed audio stream metadata.
    """
    return AudioStreamProfile(
        codec_name=audio_stream_json.get("codec_name", "unknown"),
        sample_rate_hz=int(audio_stream_json.get("sample_rate", 0) or 0),
        channel_count=int(audio_stream_json.get("channels", 0) or 0),
        channel_layout=audio_stream_json.get("channel_layout"),
    )


def _parse_color_space_profile(
    video_stream_json: typing.Dict[str, typing.Any],
) -> ColorSpaceProfile:
    """Parses color space / HDR fields from an ffprobe video-stream object.

    Args:
        video_stream_json: A ``streams[i]`` entry with
            ``codec_type == "video"``.

    Returns:
        ColorSpaceProfile: The parsed color space profile, including
        the best-effort :class:`HdrFormat` classification described in
        this module's docstring.
    """
    color_primaries = video_stream_json.get("color_primaries")
    color_transfer = video_stream_json.get("color_transfer")
    color_space_matrix = video_stream_json.get("color_space")
    side_data_list = video_stream_json.get("side_data_list", []) or []

    is_bt2020 = bool(color_primaries) and "bt2020" in color_primaries.lower()

    has_mastering_display_metadata = any(
        "mastering display" in str(entry.get("side_data_type", "")).lower()
        for entry in side_data_list
    )
    has_content_light_level_metadata = any(
        "content light level" in str(entry.get("side_data_type", "")).lower()
        for entry in side_data_list
    )
    side_data_type_text_blob = " ".join(
        str(entry.get("side_data_type", "")) for entry in side_data_list
    ).lower()

    detected_hdr_format = _classify_hdr_format(
        color_transfer=color_transfer,
        is_bt2020=is_bt2020,
        has_mastering_display_metadata=has_mastering_display_metadata,
        has_content_light_level_metadata=has_content_light_level_metadata,
        side_data_type_text_blob=side_data_type_text_blob,
    )

    return ColorSpaceProfile(
        color_primaries=color_primaries,
        color_transfer_characteristics=color_transfer,
        color_space_matrix=color_space_matrix,
        is_bt2020=is_bt2020,
        detected_hdr_format=detected_hdr_format,
        has_mastering_display_metadata=has_mastering_display_metadata,
        has_content_light_level_metadata=has_content_light_level_metadata,
    )


def _classify_hdr_format(
    *,
    color_transfer: typing.Optional[str],
    is_bt2020: bool,
    has_mastering_display_metadata: bool,
    has_content_light_level_metadata: bool,
    side_data_type_text_blob: str,
) -> HdrFormat:
    """Applies the documented heuristic to classify HDR/dynamic-range format.

    Args:
        color_transfer: ffprobe ``color_transfer`` value, lowercase
            comparison performed internally.
        is_bt2020: Whether ``color_primaries`` indicated BT.2020.
        has_mastering_display_metadata: Whether SMPTE ST 2086 side data
            was present.
        has_content_light_level_metadata: Whether MaxCLL/MaxFALL side
            data was present.
        side_data_type_text_blob: Lowercased, space-joined text of all
            side-data-type strings, used for Dolby Vision / HDR10+
            keyword matching.

    Returns:
        HdrFormat: The best-effort classification. See module
        docstring for the full documented heuristic and its known
        limitations.
    """
    transfer = (color_transfer or "").lower()

    if "dovi" in side_data_type_text_blob or "dolby vision" in side_data_type_text_blob:
        return HdrFormat.DOLBY_VISION

    if "hdr10+" in side_data_type_text_blob or "hdr10plus" in side_data_type_text_blob:
        return HdrFormat.HDR10_PLUS

    if transfer == "arib-std-b67":
        return HdrFormat.HLG

    if transfer == "smpte2084":
        if has_mastering_display_metadata or has_content_light_level_metadata:
            return HdrFormat.HDR10
        # PQ transfer without mastering-display metadata is still
        # HDR-signalled per the transfer function itself, even though
        # it is not a fully spec-compliant authored HDR10 master.
        return HdrFormat.HDR10

    if is_bt2020:
        return HdrFormat.UNKNOWN

    return HdrFormat.SDR_BT709


def _parse_ffprobe_fraction_string(fraction_string: str) -> float:
    """Parses an ffprobe ``"num/den"`` rational string into a float.

    Args:
        fraction_string: A string like ``"30000/1001"`` or ``"25/1"``.

    Returns:
        float: The evaluated fraction, or 0.0 if the denominator is
        zero or the string is malformed.
    """
    try:
        numerator_text, denominator_text = fraction_string.split("/")
        numerator = float(numerator_text)
        denominator = float(denominator_text)
        if denominator == 0:
            return 0.0
        return numerator / denominator
    except (ValueError, AttributeError):
        return 0.0


def _infer_bit_depth_from_pixel_format(pixel_format: str) -> int:
    """Infers sample bit depth from an ffmpeg pixel format string.

    Args:
        pixel_format: An ffmpeg ``pix_fmt`` string, e.g.
            ``"yuv420p"``, ``"yuv420p10le"``, ``"yuv420p12le"``.

    Returns:
        int: 8, 10, or 12 based on substring matching; defaults to 8
        when no explicit bit-depth suffix is present (ffmpeg's
        convention of omitting the suffix for 8-bit formats).
    """
    if "12le" in pixel_format or "12be" in pixel_format:
        return 12
    if "10le" in pixel_format or "10be" in pixel_format:
        return 10
    return 8
