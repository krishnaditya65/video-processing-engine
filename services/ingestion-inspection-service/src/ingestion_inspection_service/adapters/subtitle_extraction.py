"""ffprobe/ffmpeg subprocess adapter for embedded subtitle/caption extraction.

Follows the same pattern as ``adapters.ffprobe_client`` and
``adapters.audio_extraction``: shell out to the system ``ffprobe``/
``ffmpeg`` binaries (rather than parsing subtitle codecs in pure
Python), and degrade gracefully (never raise) when a source file has no
embedded subtitle streams or when ffmpeg fails to convert a given
stream -- self-skipping is handled by the *tests*, but this module's own
public API also intentionally returns empty results rather than raising
for the "no subtitles present" case, since that is an entirely normal,
expected outcome for the majority of mezzanine assets, not an error.

Workflow:

  1. :func:`detect_subtitle_streams` -- runs ffprobe
     (``-show_streams``) and filters for ``codec_type == "subtitle"``,
     returning typed :class:`SubtitleStreamInfo` records.
  2. :func:`extract_subtitle_stream_to_webvtt` -- runs ffmpeg to
     transcode one specific subtitle stream (selected by its ffmpeg
     stream index) to a WebVTT (``.vtt``) file, which is a
     text-based, widely-supported format suitable for web video
     players.
  3. :func:`parse_webvtt_cues` + :func:`detect_forced_narrative_heuristic`
     -- a basic heuristic over the extracted WebVTT's cues to flag
     tracks that look like "forced narrative" (a.k.a. "forced")
     subtitles: sparse (few cues relative to asset duration) and short
     (brief on-screen duration per cue), as commonly used for
     translating only foreign-language dialogue/signage rather than a
     full-dialogue caption track.
"""

from __future__ import annotations

import dataclasses
import json
import re
import subprocess
import typing


class SubtitleExtractionError(RuntimeError):
    """Raised when ffprobe/ffmpeg fail while inspecting or extracting subtitles.

    Attributes:
        source_file_path: The media file path being processed.
    """

    def __init__(self, source_file_path: str, underlying_error: Exception) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file being processed.
            underlying_error: The original exception raised during
                subprocess execution.
        """
        super().__init__(
            f"Subtitle extraction failed for '{source_file_path}': "
            f"{underlying_error!r}"
        )
        self.source_file_path = source_file_path
        self.underlying_error = underlying_error


@dataclasses.dataclass(frozen=True)
class SubtitleStreamInfo:
    """Describes one embedded subtitle stream as reported by ffprobe.

    Attributes:
        ffmpeg_stream_index: The stream's absolute index within the
            container (ffprobe/ffmpeg's ``-map 0:<index>`` addressing),
            used to select this exact stream during extraction.
        codec_name: Subtitle codec, e.g. ``"mov_text"``, ``"subrip"``,
            ``"webvtt"``, ``"ass"``.
        language_tag: BCP-47-ish language tag from stream tags (e.g.
            ``"eng"``), or ``None`` if not present.
        is_forced_disposition_flag: Whether the container's own
            ``disposition.forced`` flag was set for this stream (an
            explicit authoring signal, when present, distinct from this
            module's own content-based forced-narrative heuristic).
    """

    ffmpeg_stream_index: int
    codec_name: str
    language_tag: typing.Optional[str]
    is_forced_disposition_flag: bool


@dataclasses.dataclass(frozen=True)
class SubtitleCue:
    """One parsed WebVTT cue (a single caption's timing and text).

    Attributes:
        start_seconds: Cue start time, in seconds.
        end_seconds: Cue end time, in seconds.
        text: The cue's caption text (may span multiple lines, joined
            with ``"\\n"``).
    """

    start_seconds: float
    end_seconds: float
    text: str

    @property
    def duration_seconds(self) -> float:
        """Returns this cue's on-screen duration in seconds."""
        return self.end_seconds - self.start_seconds


@dataclasses.dataclass(frozen=True)
class ForcedNarrativeHeuristicResult:
    """Result of the sparse/short-cue forced-narrative subtitle heuristic.

    Attributes:
        is_likely_forced_narrative: Whether the track's cue pattern
            matches the documented heuristic.
        cue_count: Number of cues parsed.
        mean_cue_duration_seconds: Mean cue on-screen duration.
        cues_per_minute: ``cue_count`` normalized by asset duration, for
            comparison against the sparsity threshold.
    """

    is_likely_forced_narrative: bool
    cue_count: int
    mean_cue_duration_seconds: float
    cues_per_minute: float


def detect_subtitle_streams(
    source_file_path: str,
    *,
    ffprobe_executable_path: str = "ffprobe",
    subprocess_timeout_seconds: float = 60.0,
) -> typing.List[SubtitleStreamInfo]:
    """Detects embedded subtitle streams in a media file via ffprobe.

    Args:
        source_file_path: Filesystem path to the media file to probe.
        ffprobe_executable_path: Path/name of the ffprobe binary.
        subprocess_timeout_seconds: Maximum time to allow ffprobe to run.

    Returns:
        list[SubtitleStreamInfo]: One entry per embedded subtitle
        stream, in container order. Empty (never raises for this case)
        when the file has no subtitle streams.

    Raises:
        SubtitleExtractionError: If ffprobe itself is unavailable, times
            out, exits non-zero, or emits unparseable output.

    Algorithmic notes:
        O(1) relative to asset duration -- ffprobe's
        ``-show_streams`` does not decode subtitle payloads, only
        container-level stream headers.
    """
    command = [
        ffprobe_executable_path,
        "-v",
        "quiet",
        "-print_format",
        "json",
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
        raise SubtitleExtractionError(source_file_path, error) from error

    subtitle_streams = []
    for stream in parsed_json.get("streams", []):
        if stream.get("codec_type") != "subtitle":
            continue
        tags = stream.get("tags", {}) or {}
        disposition = stream.get("disposition", {}) or {}
        subtitle_streams.append(
            SubtitleStreamInfo(
                ffmpeg_stream_index=int(stream.get("index", 0)),
                codec_name=stream.get("codec_name", "unknown"),
                language_tag=tags.get("language"),
                is_forced_disposition_flag=bool(disposition.get("forced", 0)),
            )
        )
    return subtitle_streams


def extract_subtitle_stream_to_webvtt(
    source_file_path: str,
    ffmpeg_stream_index: int,
    output_vtt_path: str,
    *,
    ffmpeg_executable_path: str = "ffmpeg",
    subprocess_timeout_seconds: float = 120.0,
) -> str:
    """Extracts one embedded subtitle stream to a standalone WebVTT file.

    Args:
        source_file_path: Filesystem path to the source media file.
        ffmpeg_stream_index: The absolute container stream index of the
            subtitle stream to extract (see
            :attr:`SubtitleStreamInfo.ffmpeg_stream_index`).
        output_vtt_path: Filesystem path to write the extracted
            ``.vtt`` file to.
        ffmpeg_executable_path: Path/name of the ffmpeg binary.
        subprocess_timeout_seconds: Maximum time to allow ffmpeg to run.

    Returns:
        str: ``output_vtt_path``, for convenient chaining.

    Raises:
        SubtitleExtractionError: If ffmpeg is unavailable, exits
            non-zero (e.g. the stream index doesn't exist, or the
            subtitle codec -- such as bitmap-based ``dvd_subtitle`` --
            cannot be converted to a text-based format), or times out.

    Algorithmic notes:
        O(cue_count) dominated entirely by ffmpeg's own subtitle
        decode/re-encode, negligible relative to video/audio processing.
    """
    command = [
        ffmpeg_executable_path,
        "-y",
        "-v",
        "error",
        "-i",
        source_file_path,
        "-map",
        f"0:{ffmpeg_stream_index}",
        "-f",
        "webvtt",
        output_vtt_path,
    ]
    try:
        subprocess.run(
            command,
            capture_output=True,
            check=True,
            timeout=subprocess_timeout_seconds,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise SubtitleExtractionError(source_file_path, error) from error

    return output_vtt_path


_WEBVTT_TIMESTAMP_LINE_PATTERN = re.compile(
    r"(\d+(?::\d{2})?:\d{2}\.\d{3})\s*-->\s*(\d+(?::\d{2})?:\d{2}\.\d{3})"
)


def parse_webvtt_cues(webvtt_text: str) -> typing.List[SubtitleCue]:
    """Parses cue timing/text out of a WebVTT document's raw text.

    A minimal, purpose-built parser (not a full WebVTT spec
    implementation -- no cue settings/styling/regions support) covering
    exactly what :func:`detect_forced_narrative_heuristic` needs: cue
    start/end timestamps and text.

    Args:
        webvtt_text: The full text content of a ``.vtt`` file (or an
            equivalent in-memory string).

    Returns:
        list[SubtitleCue]: Parsed cues, in document order. Empty for a
        header-only/cueless WebVTT document.

    Algorithmic notes:
        Single linear pass, O(n) in the length of ``webvtt_text``.
    """
    lines = webvtt_text.splitlines()
    cues: typing.List[SubtitleCue] = []
    index = 0
    while index < len(lines):
        match = _WEBVTT_TIMESTAMP_LINE_PATTERN.search(lines[index])
        if match is None:
            index += 1
            continue

        start_seconds = _parse_webvtt_timestamp(match.group(1))
        end_seconds = _parse_webvtt_timestamp(match.group(2))

        text_lines = []
        index += 1
        while index < len(lines) and lines[index].strip() != "":
            text_lines.append(lines[index])
            index += 1

        cues.append(
            SubtitleCue(
                start_seconds=start_seconds,
                end_seconds=end_seconds,
                text="\n".join(text_lines),
            )
        )

    return cues


def _parse_webvtt_timestamp(timestamp_text: str) -> float:
    """Parses a WebVTT ``[HH:]MM:SS.mmm`` timestamp into seconds.

    Args:
        timestamp_text: A timestamp string as it appears in a WebVTT
            cue timing line, e.g. ``"00:00:01.500"`` or ``"01.500"``
            for hours-omitted short form.

    Returns:
        float: The timestamp converted to seconds.
    """
    parts = timestamp_text.split(":")
    parts = [float(part) for part in parts]
    while len(parts) < 3:
        parts.insert(0, 0.0)
    hours, minutes, seconds = parts
    return hours * 3600.0 + minutes * 60.0 + seconds


def detect_forced_narrative_heuristic(
    cues: typing.Sequence[SubtitleCue],
    asset_duration_seconds: float,
    *,
    maximum_cues_per_minute_for_sparse: float = 2.0,
    maximum_mean_cue_duration_seconds_for_short: float = 3.0,
) -> ForcedNarrativeHeuristicResult:
    """Applies a sparse/short-cue heuristic to flag likely "forced narrative" tracks.

    A **documented heuristic**, not a semantic/language-aware
    classifier: "forced" (a.k.a. "forced narrative") subtitle tracks
    conventionally caption only brief foreign-language dialogue or
    on-screen text within an otherwise same-language program, so they
    tend to have both (a) many fewer cues per minute of runtime than a
    full-dialogue caption track, and (b) short individual cue durations
    (a few words, not sustained conversation). Both conditions must hold
    for a track to be flagged, to avoid mislabeling either "sparse but
    long cues" (e.g. widely spaced on-screen text callouts) or "short
    but frequent cues" (e.g. fast-paced full-dialogue captioning) tracks.

    Args:
        cues: Parsed cues from :func:`parse_webvtt_cues`.
        asset_duration_seconds: Total asset duration, used to normalize
            cue count into a cues-per-minute rate.
        maximum_cues_per_minute_for_sparse: Threshold below which the
            track is considered "sparse". 2.0 cues/minute was chosen as
            comfortably below typical full-dialogue captioning rates
            (which commonly run well into the double digits per minute
            for dialogue-heavy content) while still accommodating a
            legitimately sparse forced-narrative track.
        maximum_mean_cue_duration_seconds_for_short: Threshold below
            which the track's mean cue duration is considered "short".
            3.0 seconds approximates a brief on-screen caption (a few
            words) versus sustained multi-line dialogue exchanges.

    Returns:
        ForcedNarrativeHeuristicResult: The classification plus the
        underlying statistics used to reach it.

    Algorithmic notes:
        O(n) in the number of cues.
    """
    if not cues or asset_duration_seconds <= 0:
        return ForcedNarrativeHeuristicResult(
            is_likely_forced_narrative=False,
            cue_count=len(cues),
            mean_cue_duration_seconds=0.0,
            cues_per_minute=0.0,
        )

    mean_cue_duration_seconds = sum(cue.duration_seconds for cue in cues) / len(cues)
    cues_per_minute = len(cues) / (asset_duration_seconds / 60.0)

    is_likely_forced_narrative = (
        cues_per_minute <= maximum_cues_per_minute_for_sparse
        and mean_cue_duration_seconds <= maximum_mean_cue_duration_seconds_for_short
    )

    return ForcedNarrativeHeuristicResult(
        is_likely_forced_narrative=is_likely_forced_narrative,
        cue_count=len(cues),
        mean_cue_duration_seconds=mean_cue_duration_seconds,
        cues_per_minute=cues_per_minute,
    )
