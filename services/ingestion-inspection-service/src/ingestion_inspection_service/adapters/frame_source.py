"""OpenCV-based frame sampling adapter.

Decodes a source video at a fixed *analysis* sampling rate (frames per
second of source-timeline duration) rather than decoding every frame,
which is what makes inspecting multi-hour 4K mezzanine assets tractable
within the target performance envelope documented in the README.

The sampling approach: given the source's native frame rate (from
``cv2.VideoCapture.get(cv2.CAP_PROP_FPS)``) and a desired analysis rate
(e.g. 1.0 fps), we compute an integer frame stride
``round(native_fps / analysis_fps)`` and seek/read every ``stride``-th
frame via ``cv2.VideoCapture.read()``, rather than performing a costly
timestamp-based seek per sample (which is slower and less reliable
across containers than sequential reads with a skip count for this
access pattern).
"""

from __future__ import annotations

import dataclasses
import typing

import cv2
import numpy as np

from ingestion_inspection_service.config import (
    DEFAULT_FRAME_SAMPLING_CONFIGURATION,
    FrameSamplingConfiguration,
)


class VideoDecodeError(RuntimeError):
    """Raised when a video file cannot be opened or decoded by OpenCV.

    Attributes:
        source_file_path: The media file path that failed to open/decode.
    """

    def __init__(self, source_file_path: str, reason: str) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file that failed.
            reason: Human-readable explanation of the failure.
        """
        super().__init__(f"Failed to decode '{source_file_path}': {reason}")
        self.source_file_path = source_file_path


@dataclasses.dataclass(frozen=True)
class SampledFrame:
    """One analysis-sampled frame plus its source-timeline timestamp.

    Attributes:
        frame_bgr: The decoded frame as an ``(H, W, 3)`` ``uint8`` NumPy
            array in OpenCV's BGR channel order.
        timestamp_seconds: The frame's presentation timestamp within
            the source asset's timeline, in seconds.
        source_frame_index: The zero-based index of this frame within
            the *full* (unsampled) native-frame-rate decode sequence,
            retained for debugging/traceability.
    """

    frame_bgr: np.ndarray
    timestamp_seconds: float
    source_frame_index: int


def sample_frames_at_fixed_rate(
    source_file_path: str,
    sampling_configuration: FrameSamplingConfiguration = DEFAULT_FRAME_SAMPLING_CONFIGURATION,
) -> typing.Iterator[SampledFrame]:
    """Decodes and yields frames from a video file at a fixed analysis rate.

    Args:
        source_file_path: Filesystem path to the video file to decode.
        sampling_configuration: Controls the target analysis frame rate
            and the hard cap on total sampled frames.

    Yields:
        SampledFrame: One sampled frame at a time, in ascending
        timestamp order.

    Raises:
        VideoDecodeError: If the file cannot be opened by OpenCV
            (unsupported codec/container, missing codec support in the
            local OpenCV build, corrupt file, or nonexistent path), or
            if it reports zero/invalid duration metadata.

    Algorithmic notes:
        Let ``N`` be the total native frame count and ``r`` the
        computed integer stride (``native_fps / target_analysis_fps``,
        minimum 1). This function performs O(N) sequential
        ``VideoCapture.read()`` calls (OpenCV must still demux/decode
        every frame it steps past when using sequential reads, which is
        why the *decode* cost does not shrink proportionally to the
        sampling rate the way the *analysis* cost does -- see the
        README's performance section for the full discussion of this
        tradeoff and why sequential-skip is nonetheless still chosen
        over per-sample keyframe seeking for correctness/robustness
        reasons across arbitrary mezzanine containers), but only O(N/r)
        frames are actually converted to NumPy arrays and returned to
        the caller, which is what keeps downstream analysis (SSIM,
        histograms, Canny) cheap.
    """
    video_capture = cv2.VideoCapture(source_file_path)
    if not video_capture.isOpened():
        video_capture.release()
        raise VideoDecodeError(
            source_file_path, "cv2.VideoCapture could not open the file."
        )

    try:
        native_frames_per_second = video_capture.get(cv2.CAP_PROP_FPS) or 0.0
        if native_frames_per_second <= 0:
            raise VideoDecodeError(
                source_file_path,
                "Source reports a zero or invalid native frame rate.",
            )

        target_fps = sampling_configuration.target_analysis_frames_per_second
        frame_stride = max(1, round(native_frames_per_second / target_fps))

        yielded_frame_count = 0
        source_frame_index = 0
        while yielded_frame_count < sampling_configuration.maximum_frames_to_sample:
            frame_read_successfully, frame_bgr = video_capture.read()
            if not frame_read_successfully:
                break

            if source_frame_index % frame_stride == 0:
                timestamp_seconds = source_frame_index / native_frames_per_second
                yield SampledFrame(
                    frame_bgr=frame_bgr,
                    timestamp_seconds=timestamp_seconds,
                    source_frame_index=source_frame_index,
                )
                yielded_frame_count += 1

            source_frame_index += 1
    finally:
        video_capture.release()


def probe_video_duration_seconds(source_file_path: str) -> float:
    """Returns a video file's duration in seconds via OpenCV frame count/FPS.

    This is a lightweight fallback used when ffprobe's own duration
    field is unavailable/zero; the primary duration source elsewhere in
    the pipeline is the ffprobe adapter, which is generally more
    reliable across exotic containers.

    Args:
        source_file_path: Filesystem path to the video file.

    Returns:
        float: Duration in seconds, computed as
        ``frame_count / frames_per_second``.

    Raises:
        VideoDecodeError: If the file cannot be opened or reports an
            invalid frame rate.
    """
    video_capture = cv2.VideoCapture(source_file_path)
    if not video_capture.isOpened():
        video_capture.release()
        raise VideoDecodeError(
            source_file_path, "cv2.VideoCapture could not open the file."
        )
    try:
        frames_per_second = video_capture.get(cv2.CAP_PROP_FPS) or 0.0
        frame_count = video_capture.get(cv2.CAP_PROP_FRAME_COUNT) or 0.0
        if frames_per_second <= 0:
            raise VideoDecodeError(
                source_file_path, "Source reports an invalid native frame rate."
            )
        return frame_count / frames_per_second
    finally:
        video_capture.release()
