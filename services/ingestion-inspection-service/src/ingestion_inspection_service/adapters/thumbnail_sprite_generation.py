"""ffmpeg-backed scene-thumbnail extraction and sprite-sheet compositing.

Given a list of scene boundary timestamps, this module:

  1. Shells out to ``ffmpeg`` once per scene to extract exactly one
     representative frame at (or immediately after) the scene's start
     timestamp (``-ss <timestamp> -frames:v 1``), decoded straight to a
     small in-memory BGR array via ``cv2.imdecode`` over ffmpeg's
     piped PNG output -- no intermediate frame files are written to
     disk.
  2. Resizes each extracted frame to a fixed thumbnail size and
     composites them into a single grid image (a "sprite sheet") purely
     with ``numpy`` array concatenation (``np.vstack``/``np.hstack``),
     wrapping to a new row every ``sprite_sheet_columns`` thumbnails --
     the standard technique video players use for scrubbing-preview
     thumbnails (one HTTP request for the whole sheet instead of one per
     thumbnail).
  3. Produces a WebVTT-formatted "sprite index" mapping each scene's
     time range to its thumbnail's pixel rectangle within the sheet
     (``<sprite_file>#xywh=<x>,<y>,<w>,<h>``), the de facto convention
     several open-source and commercial video players (e.g. video.js's
     ``vtt-thumbnails`` plugin) use for scrubbing previews.
"""

from __future__ import annotations

import dataclasses
import subprocess
import typing

import cv2
import numpy as np

from ingestion_inspection_service.config import (
    DEFAULT_SPRITE_SHEET_GENERATION_CONFIGURATION,
    SpriteSheetGenerationConfiguration,
)


class ThumbnailExtractionError(RuntimeError):
    """Raised when ffmpeg fails to extract a representative frame.

    Attributes:
        source_file_path: The media file path being processed.
        timestamp_seconds: The timestamp extraction was attempted at.
    """

    def __init__(
        self,
        source_file_path: str,
        timestamp_seconds: float,
        underlying_error: Exception,
    ) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file being processed.
            timestamp_seconds: Timestamp extraction was attempted at.
            underlying_error: The original exception raised.
        """
        super().__init__(
            f"Thumbnail extraction failed for '{source_file_path}' at "
            f"t={timestamp_seconds}s: {underlying_error!r}"
        )
        self.source_file_path = source_file_path
        self.timestamp_seconds = timestamp_seconds
        self.underlying_error = underlying_error


@dataclasses.dataclass(frozen=True)
class SpriteThumbnailRectangle:
    """Pixel rectangle of one thumbnail within a composited sprite sheet.

    Attributes:
        scene_index: Index of the scene this thumbnail represents.
        start_timestamp_seconds: The scene's start timestamp (the
            timestamp the representative frame was extracted at).
        end_timestamp_seconds: The scene's end timestamp.
        x_pixels: Left edge of the thumbnail's rectangle within the
            sprite sheet.
        y_pixels: Top edge of the thumbnail's rectangle within the
            sprite sheet.
        width_pixels: Thumbnail width.
        height_pixels: Thumbnail height.
    """

    scene_index: int
    start_timestamp_seconds: float
    end_timestamp_seconds: float
    x_pixels: int
    y_pixels: int
    width_pixels: int
    height_pixels: int


@dataclasses.dataclass(frozen=True)
class SpriteSheetGenerationResult:
    """Result of generating a sprite sheet for an asset's scenes.

    Attributes:
        sprite_sheet_image_bgr: The composited sprite sheet image,
            ``(H, W, 3)`` ``uint8`` BGR.
        thumbnail_rectangles: One :class:`SpriteThumbnailRectangle` per
            input scene, in the same order.
    """

    sprite_sheet_image_bgr: np.ndarray
    thumbnail_rectangles: typing.List[SpriteThumbnailRectangle]


def extract_representative_frame_via_ffmpeg(
    source_file_path: str,
    timestamp_seconds: float,
    *,
    ffmpeg_executable_path: str = "ffmpeg",
    subprocess_timeout_seconds: float = 60.0,
) -> np.ndarray:
    """Extracts a single decoded frame at a given timestamp via ffmpeg.

    Args:
        source_file_path: Filesystem path to the source media file.
        timestamp_seconds: Timestamp, in seconds, to seek to before
            extracting the frame.
        ffmpeg_executable_path: Path/name of the ffmpeg binary.
        subprocess_timeout_seconds: Maximum time to allow ffmpeg to run.

    Returns:
        numpy.ndarray: The extracted frame, ``(H, W, 3)`` ``uint8`` BGR.

    Raises:
        ThumbnailExtractionError: If ffmpeg is unavailable, exits
            non-zero, times out, or produces output that ``cv2.imdecode``
            cannot parse as an image.

    Algorithmic notes:
        ffmpeg's ``-ss`` placed before ``-i`` performs an input-side
        (keyframe-nearby) seek, which is fast (does not decode from the
        start of the file) at the cost of landing on the nearest
        preceding keyframe rather than a frame-exact timestamp -- an
        acceptable tradeoff for a scrubbing-preview thumbnail, which
        does not need frame accuracy.
    """
    command = [
        ffmpeg_executable_path,
        "-y",
        "-v",
        "error",
        "-ss",
        str(timestamp_seconds),
        "-i",
        source_file_path,
        "-frames:v",
        "1",
        "-f",
        "image2pipe",
        "-vcodec",
        "png",
        "pipe:1",
    ]
    try:
        completed_process = subprocess.run(
            command,
            capture_output=True,
            check=True,
            timeout=subprocess_timeout_seconds,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise ThumbnailExtractionError(
            source_file_path, timestamp_seconds, error
        ) from error

    png_bytes = completed_process.stdout
    if not png_bytes:
        raise ThumbnailExtractionError(
            source_file_path,
            timestamp_seconds,
            RuntimeError("ffmpeg produced no image output."),
        )

    frame_bgr = cv2.imdecode(np.frombuffer(png_bytes, dtype=np.uint8), cv2.IMREAD_COLOR)
    if frame_bgr is None:
        raise ThumbnailExtractionError(
            source_file_path,
            timestamp_seconds,
            RuntimeError("cv2.imdecode could not decode ffmpeg's PNG output."),
        )
    return frame_bgr


def composite_sprite_sheet_from_scene_frames(
    scene_frames_bgr: typing.Sequence[np.ndarray],
    scene_start_timestamps_seconds: typing.Sequence[float],
    scene_end_timestamps_seconds: typing.Sequence[float],
    configuration: SpriteSheetGenerationConfiguration = DEFAULT_SPRITE_SHEET_GENERATION_CONFIGURATION,
) -> SpriteSheetGenerationResult:
    """Resizes and grid-composites per-scene frames into one sprite sheet.

    Pure ``numpy``/``cv2`` array operation -- no I/O -- so it is unit
    testable directly against small synthetic frames.

    Args:
        scene_frames_bgr: One representative BGR frame per scene, in
            scene order.
        scene_start_timestamps_seconds: Each scene's start timestamp,
            same length/order as ``scene_frames_bgr``.
        scene_end_timestamps_seconds: Each scene's end timestamp, same
            length/order.
        configuration: Controls thumbnail size and sheet column count.

    Returns:
        SpriteSheetGenerationResult: The composited sheet image plus the
        pixel rectangle each input scene was placed at.

    Raises:
        ValueError: If the three input sequences have mismatched
            lengths, or if ``scene_frames_bgr`` is empty.

    Algorithmic notes:
        O(n) resize operations (each O(thumbnail_area)) plus O(n)
        array-concatenation work to build the grid -- negligible
        relative to the ffmpeg extraction cost that typically produces
        ``scene_frames_bgr`` in the first place.
    """
    scene_count = len(scene_frames_bgr)
    if (
        len(scene_start_timestamps_seconds) != scene_count
        or len(scene_end_timestamps_seconds) != scene_count
    ):
        raise ValueError(
            "scene_frames_bgr, scene_start_timestamps_seconds, and "
            "scene_end_timestamps_seconds must all have the same length."
        )
    if scene_count == 0:
        raise ValueError(
            "composite_sprite_sheet_from_scene_frames requires at least one scene frame."
        )

    thumbnail_width = configuration.thumbnail_width_pixels
    thumbnail_height = configuration.thumbnail_height_pixels
    columns = configuration.sprite_sheet_columns
    rows = (scene_count + columns - 1) // columns
    effective_columns = min(scene_count, columns)

    sheet_width = thumbnail_width * effective_columns
    sheet_height = thumbnail_height * rows
    sprite_sheet_image_bgr = np.zeros((sheet_height, sheet_width, 3), dtype=np.uint8)

    thumbnail_rectangles: typing.List[SpriteThumbnailRectangle] = []
    for scene_index, frame in enumerate(scene_frames_bgr):
        resized_thumbnail = cv2.resize(
            frame, (thumbnail_width, thumbnail_height), interpolation=cv2.INTER_AREA
        )
        column = scene_index % columns
        row = scene_index // columns
        x_pixels = column * thumbnail_width
        y_pixels = row * thumbnail_height

        sprite_sheet_image_bgr[
            y_pixels : y_pixels + thumbnail_height,
            x_pixels : x_pixels + thumbnail_width,
        ] = resized_thumbnail

        thumbnail_rectangles.append(
            SpriteThumbnailRectangle(
                scene_index=scene_index,
                start_timestamp_seconds=scene_start_timestamps_seconds[scene_index],
                end_timestamp_seconds=scene_end_timestamps_seconds[scene_index],
                x_pixels=x_pixels,
                y_pixels=y_pixels,
                width_pixels=thumbnail_width,
                height_pixels=thumbnail_height,
            )
        )

    return SpriteSheetGenerationResult(
        sprite_sheet_image_bgr=sprite_sheet_image_bgr,
        thumbnail_rectangles=thumbnail_rectangles,
    )


def build_webvtt_sprite_index(
    thumbnail_rectangles: typing.Sequence[SpriteThumbnailRectangle],
    sprite_sheet_file_name: str,
) -> str:
    """Builds a WebVTT-formatted sprite index for player scrubbing previews.

    Args:
        thumbnail_rectangles: Rectangles produced by
            :func:`composite_sprite_sheet_from_scene_frames`.
        sprite_sheet_file_name: Filename (or relative URL) of the sprite
            sheet image these rectangles refer into, embedded in each
            cue's media fragment reference.

    Returns:
        str: A complete WebVTT document, one cue per thumbnail
        rectangle, each cue's text pointing at
        ``<sprite_sheet_file_name>#xywh=<x>,<y>,<w>,<h>`` per the
        Media Fragments URI convention players use for sprite-based
        scrubbing previews.

    Algorithmic notes:
        O(n) in the number of rectangles.
    """
    lines = ["WEBVTT", ""]
    for rectangle in thumbnail_rectangles:
        start_text = _format_webvtt_timestamp(rectangle.start_timestamp_seconds)
        end_text = _format_webvtt_timestamp(rectangle.end_timestamp_seconds)
        fragment = (
            f"{sprite_sheet_file_name}#xywh="
            f"{rectangle.x_pixels},{rectangle.y_pixels},"
            f"{rectangle.width_pixels},{rectangle.height_pixels}"
        )
        lines.append(f"{start_text} --> {end_text}")
        lines.append(fragment)
        lines.append("")
    return "\n".join(lines)


def _format_webvtt_timestamp(total_seconds: float) -> str:
    """Formats a seconds value as a WebVTT ``HH:MM:SS.mmm`` timestamp.

    Args:
        total_seconds: Timestamp in seconds (non-negative).

    Returns:
        str: A zero-padded ``HH:MM:SS.mmm`` timestamp string.
    """
    total_milliseconds = round(total_seconds * 1000.0)
    hours, remainder_ms = divmod(total_milliseconds, 3_600_000)
    minutes, remainder_ms = divmod(remainder_ms, 60_000)
    seconds, milliseconds = divmod(remainder_ms, 1_000)
    return f"{hours:02d}:{minutes:02d}:{seconds:02d}.{milliseconds:03d}"
