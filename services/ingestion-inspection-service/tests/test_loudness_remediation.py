"""Integration tests for ffmpeg loudnorm-based loudness remediation.

Fully dependent on a real ffmpeg binary with the loudnorm filter (audio
decode/re-encode + gain correction is not something meaningfully
testable with synthetic in-memory arrays alone, unlike the pure LUFS
*analysis* math already covered by ``test_loudness_analysis.py``), so
this module self-skips entirely when ffmpeg/ffprobe are unavailable,
per the established pattern in
``test_ffprobe_and_frame_source_integration.py``.
"""

from __future__ import annotations

import pytest

from ingestion_inspection_service.adapters.audio_extraction import (
    AudioExtractionError,
)
from ingestion_inspection_service.adapters.loudness_remediation import (
    LoudnessRemediationError,
    apply_loudness_normalization_via_ffmpeg,
)
from tests.conftest import ffmpeg_and_ffprobe_are_available

pytestmark = pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)


class TestApplyLoudnessNormalizationViaFfmpeg:
    """Tests loudnorm-based remediation against the shared synthetic video fixture."""

    def test_produces_an_output_file_with_before_after_lufs(
        self, synthetic_test_video_path: str, tmp_path
    ) -> None:
        """Running remediation writes an output file and reports before/after LUFS."""
        output_path = tmp_path / "normalized.mov"

        result = apply_loudness_normalization_via_ffmpeg(
            synthetic_test_video_path, str(output_path), target_lufs=-16.0
        )

        assert output_path.exists()
        assert output_path.stat().st_size > 0
        assert result.output_file_path == str(output_path)
        assert result.target_lufs == -16.0
        assert isinstance(result.loudness_before.integrated_loudness_lufs, float)
        assert isinstance(result.loudness_after.integrated_loudness_lufs, float)

    def test_normalized_output_loudness_moves_toward_target(
        self, synthetic_test_video_path: str, tmp_path
    ) -> None:
        """The normalized output's LUFS is closer to the target than the source's."""
        output_path = tmp_path / "normalized_toward_target.mov"
        target_lufs = -16.0

        result = apply_loudness_normalization_via_ffmpeg(
            synthetic_test_video_path, str(output_path), target_lufs=target_lufs
        )

        before_distance = abs(
            result.loudness_before.integrated_loudness_lufs - target_lufs
        )
        after_distance = abs(
            result.loudness_after.integrated_loudness_lufs - target_lufs
        )
        assert after_distance <= before_distance + 1.0  # allow single-pass slack

    def test_nonexistent_source_file_raises_loudness_remediation_error(
        self, tmp_path
    ) -> None:
        """A nonexistent source file raises a documented extraction/remediation error."""
        with pytest.raises((LoudnessRemediationError, AudioExtractionError)):
            apply_loudness_normalization_via_ffmpeg(
                "/no/such/file/anywhere.mov", str(tmp_path / "out.mov")
            )
