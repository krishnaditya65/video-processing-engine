"""ffmpeg subprocess adapter for raw mono PCM audio extraction.

Used to feed
:func:`~ingestion_inspection_service.pipeline.stages.compute_approximate_integrated_loudness_lufs`
with real sample data. Shells out to ``ffmpeg`` (rather than ``ffprobe``,
which does not decode audio payloads) requesting 16-bit signed
little-endian PCM at a fixed 48 kHz mono downmix, piped to stdout, which
is then parsed into a normalized ``[-1.0, 1.0]`` float64 NumPy array.

Downmixing every source to mono before loudness measurement is itself a
simplification (full BS.1770 measures and channel-weights each channel
independently); see the loudness stage's docstring for the full list of
documented simplifications.
"""

from __future__ import annotations

import subprocess
import typing

import numpy as np


class AudioExtractionError(RuntimeError):
    """Raised when ffmpeg fails to extract PCM audio from a media file.

    Attributes:
        source_file_path: The media file path audio extraction was
            attempted against.
    """

    def __init__(self, source_file_path: str, underlying_error: Exception) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file being processed.
            underlying_error: The original exception raised during
                subprocess execution.
        """
        super().__init__(
            f"ffmpeg audio extraction failed for '{source_file_path}': "
            f"{underlying_error!r}"
        )
        self.source_file_path = source_file_path
        self.underlying_error = underlying_error


def extract_mono_pcm_via_ffmpeg(
    source_file_path: str,
    *,
    ffmpeg_executable_path: str = "ffmpeg",
    target_sample_rate_hz: int = 48_000,
    subprocess_timeout_seconds: float = 300.0,
) -> typing.Tuple[np.ndarray, int]:
    """Extracts mono, normalized-float PCM audio from a media file via ffmpeg.

    Args:
        source_file_path: Filesystem path to the media file.
        ffmpeg_executable_path: Path/name of the ffmpeg binary; defaults
            to ``"ffmpeg"`` (resolved via ``PATH``).
        target_sample_rate_hz: Sample rate to resample to during
            extraction. 48 kHz is chosen as a common professional
            mezzanine audio sample rate that avoids upsampling most
            source material.
        subprocess_timeout_seconds: Maximum time to allow the ffmpeg
            subprocess to run.

    Returns:
        tuple[numpy.ndarray, int]: ``(pcm_samples, sample_rate_hz)``
        where ``pcm_samples`` is a 1-D ``float64`` array normalized to
        ``[-1.0, 1.0]``.

    Raises:
        AudioExtractionError: If ffmpeg is unavailable, exits non-zero,
            times out, or produces no decodable audio (e.g. the source
            has no audio stream).

    Algorithmic notes:
        O(duration) dominated entirely by ffmpeg's own decode; this
        function's own post-processing (int16 -> normalized float64
        conversion) is a single O(n) vectorized NumPy division.
    """
    command = [
        ffmpeg_executable_path,
        "-v",
        "error",
        "-i",
        source_file_path,
        "-vn",
        "-ac",
        "1",
        "-ar",
        str(target_sample_rate_hz),
        "-f",
        "s16le",
        "-acodec",
        "pcm_s16le",
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
        raise AudioExtractionError(source_file_path, error) from error

    raw_bytes = completed_process.stdout
    if not raw_bytes:
        raise AudioExtractionError(
            source_file_path,
            RuntimeError("ffmpeg produced no audio output (no audio stream?)."),
        )

    int16_samples = np.frombuffer(raw_bytes, dtype=np.int16)
    normalized_float_samples = int16_samples.astype(np.float64) / 32768.0
    return normalized_float_samples, target_sample_rate_hz
