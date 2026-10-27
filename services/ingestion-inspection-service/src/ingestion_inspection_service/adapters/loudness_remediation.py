"""ffmpeg ``loudnorm``-backed audio loudness remediation (correction).

Extends the existing *analysis*-only loudness pass
(``pipeline.stages.compute_approximate_integrated_loudness_lufs``) into
actual *correction*: this module shells out to ffmpeg's built-in
``loudnorm`` audio filter (an implementation of the EBU R128 loudness
normalization algorithm, part of ffmpeg itself) to produce a new media
file normalized to a target integrated loudness, then re-runs this
service's own approximate LUFS analysis on both the original and the
normalized output so callers get a documented, consistent-methodology
"before/after" comparison.

Why ffmpeg's ``loudnorm`` rather than reimplementing normalization:
correcting loudness (as opposed to merely *measuring* it, which this
service already does from first principles for the analysis pass)
requires applying a time-varying gain adjustment and truePeak limiting
to the actual audio samples and re-encoding -- squarely inside ffmpeg's
existing, heavily-optimized job, and reimplementing a correct/safe
audio limiter is a correctness risk not worth taking on for this
service's scope.

Single-pass vs. two-pass ``loudnorm``: ffmpeg's ``loudnorm`` filter
supports a more accurate two-pass mode (measure first, then apply
precise correction using the measured stats), but that requires an
extra full-file analysis pass. This implementation deliberately uses
**single-pass** mode (measurement and correction happen together,
with ffmpeg's own dynamic/linear normalization) to keep remediation a
single ffmpeg invocation, at the cost of somewhat less precise final
loudness than the two-pass mode would achieve -- documented here as a
known limitation, not silently glossed over.
"""

from __future__ import annotations

import dataclasses
import subprocess
import typing

from ingestion_inspection_service.adapters.audio_extraction import (
    AudioExtractionError,
    extract_mono_pcm_via_ffmpeg,
)
from ingestion_inspection_service.config import (
    DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
    LoudnessAnalysisConfiguration,
)
from ingestion_inspection_service.domain.models import LoudnessAnalysisResult
from ingestion_inspection_service.pipeline.stages import (
    compute_approximate_integrated_loudness_lufs,
)


class LoudnessRemediationError(RuntimeError):
    """Raised when ffmpeg fails to apply loudness normalization.

    Attributes:
        source_file_path: The media file path being remediated.
    """

    def __init__(self, source_file_path: str, underlying_error: Exception) -> None:
        """Builds the error with a descriptive message.

        Args:
            source_file_path: Path of the media file being processed.
            underlying_error: The original exception raised.
        """
        super().__init__(
            f"Loudness remediation failed for '{source_file_path}': "
            f"{underlying_error!r}"
        )
        self.source_file_path = source_file_path
        self.underlying_error = underlying_error


@dataclasses.dataclass(frozen=True)
class LoudnessRemediationResult:
    """Before/after report for one loudness-normalization run.

    Attributes:
        output_file_path: Filesystem path of the newly written,
            loudness-normalized media file.
        target_lufs: The integrated loudness target requested.
        loudness_before: This service's own approximate LUFS analysis
            (see ``pipeline.stages.compute_approximate_integrated_loudness_lufs``)
            of the *original* source file's audio.
        loudness_after: The same analysis re-run against the
            *normalized output* file's audio, for a like-for-like
            before/after comparison using one consistent methodology.
    """

    output_file_path: str
    target_lufs: float
    loudness_before: LoudnessAnalysisResult
    loudness_after: LoudnessAnalysisResult


def apply_loudness_normalization_via_ffmpeg(
    source_file_path: str,
    output_file_path: str,
    *,
    target_lufs: float = DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION.streaming_platform_target_lufs,
    true_peak_limit_dbtp: float = -1.5,
    loudness_range_target_lu: float = 11.0,
    ffmpeg_executable_path: str = "ffmpeg",
    loudness_configuration: LoudnessAnalysisConfiguration = DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
    subprocess_timeout_seconds: float = 600.0,
) -> LoudnessRemediationResult:
    """Normalizes an asset's audio to a target LUFS via ffmpeg's loudnorm filter.

    Args:
        source_file_path: Filesystem path to the source media file.
        output_file_path: Filesystem path to write the normalized
            output file to. The container/codec are inferred by ffmpeg
            from this path's extension.
        target_lufs: Target integrated loudness, in LUFS. Defaults to
            the platform's configured streaming target
            (``LoudnessAnalysisConfiguration.streaming_platform_target_lufs``,
            -16.0 LUFS) so remediation and analysis agree on "in spec"
            by default.
        true_peak_limit_dbtp: Maximum true-peak level to enforce, in
            dBTP. -1.5 dBTP is a common streaming-delivery ceiling that
            leaves headroom for downstream lossy transcode
            inter-sample peak growth.
        loudness_range_target_lu: Target loudness range (LRA), in LU,
            passed to ``loudnorm``'s ``LRA`` parameter -- how much
            dynamic range variation to preserve rather than compress
            away. 11 LU is ffmpeg's own documented default.
        ffmpeg_executable_path: Path/name of the ffmpeg binary.
        loudness_configuration: Used both for the default
            ``target_lufs`` and as the configuration passed through to
            the before/after analysis passes.
        subprocess_timeout_seconds: Maximum time to allow the ffmpeg
            normalization subprocess to run; higher than most other
            adapters' defaults since this re-encodes the full asset
            duration.

    Returns:
        LoudnessRemediationResult: The output file path plus
        before/after LUFS analysis.

    Raises:
        LoudnessRemediationError: If the ffmpeg normalization subprocess
            fails, times out, or is unavailable.
        AudioExtractionError: If PCM extraction (for the before/after
            analysis passes) fails against either the original or the
            normalized output file.

    Algorithmic notes:
        Dominated by ffmpeg's own O(duration) audio (and, if present,
        video passthrough/copy) processing; this function's own added
        cost is two O(duration) PCM extraction + O(n) LUFS analysis
        passes (before and after), each with the same complexity
        profile as
        ``compute_approximate_integrated_loudness_lufs`` documents.

    Known limitation:
        Uses ffmpeg's **single-pass** ``loudnorm`` mode (see module
        docstring) rather than the more precise two-pass
        measure-then-correct mode, so the resulting
        ``loudness_after.integrated_loudness_lufs`` may land within a
        few tenths to ~1 LU of ``target_lufs`` rather than matching it
        exactly.
    """
    loudness_before = _analyze_file_loudness(
        source_file_path, loudness_configuration
    )

    loudnorm_filter = (
        f"loudnorm=I={target_lufs}:TP={true_peak_limit_dbtp}:"
        f"LRA={loudness_range_target_lu}"
    )
    command = [
        ffmpeg_executable_path,
        "-y",
        "-v",
        "error",
        "-i",
        source_file_path,
        "-af",
        loudnorm_filter,
        output_file_path,
    ]
    try:
        subprocess.run(
            command,
            capture_output=True,
            check=True,
            timeout=subprocess_timeout_seconds,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise LoudnessRemediationError(source_file_path, error) from error

    loudness_after = _analyze_file_loudness(output_file_path, loudness_configuration)

    return LoudnessRemediationResult(
        output_file_path=output_file_path,
        target_lufs=target_lufs,
        loudness_before=loudness_before,
        loudness_after=loudness_after,
    )


def _analyze_file_loudness(
    file_path: str, loudness_configuration: LoudnessAnalysisConfiguration
) -> LoudnessAnalysisResult:
    """Extracts PCM audio from a file and runs the approximate LUFS pass.

    Args:
        file_path: Filesystem path to the media file to analyze.
        loudness_configuration: Passed through to the analysis
            function.

    Returns:
        LoudnessAnalysisResult: The analysis result for ``file_path``.

    Raises:
        AudioExtractionError: If ffmpeg cannot extract PCM audio from
            ``file_path``.
    """
    pcm_samples, sample_rate_hz = extract_mono_pcm_via_ffmpeg(file_path)
    return compute_approximate_integrated_loudness_lufs(
        pcm_samples, sample_rate_hz, loudness_configuration
    )
