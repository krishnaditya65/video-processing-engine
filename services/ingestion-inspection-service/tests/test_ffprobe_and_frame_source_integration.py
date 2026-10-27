"""Integration tests exercising the real ffprobe/ffmpeg/OpenCV boundary.

These tests are skipped automatically when ffmpeg/ffprobe are not
available on PATH (see ``tests.conftest.ffmpeg_and_ffprobe_are_available``),
so the rest of the suite (pure algorithmic logic on synthetic arrays)
remains runnable in environments without those binaries, per the task's
documented fallback requirement.
"""

from __future__ import annotations

import pytest

from ingestion_inspection_service.adapters.audio_extraction import (
    extract_mono_pcm_via_ffmpeg,
)
from ingestion_inspection_service.adapters.ffprobe_client import (
    extract_typed_stream_metadata_via_ffprobe,
)
from ingestion_inspection_service.adapters.frame_source import (
    sample_frames_at_fixed_rate,
)
from ingestion_inspection_service.config import FrameSamplingConfiguration
from ingestion_inspection_service.domain.models import HdrFormat
from tests.conftest import ffmpeg_and_ffprobe_are_available

pytestmark = pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)


class TestFfprobeClientAgainstRealFile:
    """Tests extract_typed_stream_metadata_via_ffprobe against a real MP4."""

    def test_probes_duration_and_video_stream_from_real_file(
        self, synthetic_test_video_path: str
    ) -> None:
        """A real 2-second synthetic clip reports ~2s duration and H.264 video."""
        result = extract_typed_stream_metadata_via_ffprobe(synthetic_test_video_path)

        assert result.duration_seconds == pytest.approx(2.0, abs=0.3)
        assert result.video_stream_profile is not None
        assert result.video_stream_profile.codec_name == "h264"
        assert result.video_stream_profile.width_pixels == 64
        assert result.video_stream_profile.height_pixels == 64

    def test_probes_audio_stream_from_real_file(
        self, synthetic_test_video_path: str
    ) -> None:
        """The synthetic clip's AAC sine-wave audio track is detected."""
        result = extract_typed_stream_metadata_via_ffprobe(synthetic_test_video_path)

        assert result.audio_stream_profile is not None
        assert result.audio_stream_profile.codec_name == "aac"

    def test_color_space_profile_defaults_to_sdr_for_plain_h264(
        self, synthetic_test_video_path: str
    ) -> None:
        """A plain BT.709-ish synthetic H.264 clip is not misclassified as HDR."""
        result = extract_typed_stream_metadata_via_ffprobe(synthetic_test_video_path)

        assert result.color_space_profile is not None
        assert result.color_space_profile.detected_hdr_format in (
            HdrFormat.SDR_BT709,
            HdrFormat.UNKNOWN,
        )

    def test_nonexistent_file_raises_ffprobe_execution_error(self) -> None:
        """Probing a nonexistent file raises FfprobeExecutionError."""
        from ingestion_inspection_service.adapters.ffprobe_client import (
            FfprobeExecutionError,
        )

        with pytest.raises(FfprobeExecutionError):
            extract_typed_stream_metadata_via_ffprobe("/no/such/file/anywhere.mov")


class TestFrameSourceAgainstRealFile:
    """Tests sample_frames_at_fixed_rate against a real MP4."""

    def test_samples_frames_with_ascending_timestamps(
        self, synthetic_test_video_path: str
    ) -> None:
        """Sampled frames are yielded in ascending timestamp order with real pixel data."""
        sampling_configuration = FrameSamplingConfiguration(
            target_analysis_frames_per_second=5.0
        )
        sampled_frames = list(
            sample_frames_at_fixed_rate(
                synthetic_test_video_path, sampling_configuration
            )
        )

        assert len(sampled_frames) >= 2
        timestamps = [frame.timestamp_seconds for frame in sampled_frames]
        assert timestamps == sorted(timestamps)
        assert sampled_frames[0].frame_bgr.shape == (64, 64, 3)

    def test_detects_real_red_to_blue_scene_cut_via_histogram_strategy(
        self, synthetic_test_video_path: str
    ) -> None:
        """The real red->blue cut in the fixture video is detected end-to-end."""
        from ingestion_inspection_service.strategies.scene_detection import (
            HistogramDifferenceSceneDetectionStrategy,
            detect_scene_boundaries_via_adaptive_thresholding,
            compute_motion_complexity_score,
        )

        sampling_configuration = FrameSamplingConfiguration(
            target_analysis_frames_per_second=5.0
        )
        sampled_frames = list(
            sample_frames_at_fixed_rate(
                synthetic_test_video_path, sampling_configuration
            )
        )
        strategy = HistogramDifferenceSceneDetectionStrategy()

        dissimilarities = [
            strategy.compute_frame_pair_dissimilarity_score(
                sampled_frames[i].frame_bgr, sampled_frames[i + 1].frame_bgr
            )
            for i in range(len(sampled_frames) - 1)
        ]
        complexities = [
            compute_motion_complexity_score(frame.frame_bgr)
            for frame in sampled_frames
        ]
        timestamps = [frame.timestamp_seconds for frame in sampled_frames]

        boundaries = detect_scene_boundaries_via_adaptive_thresholding(
            frame_pair_dissimilarity_scores=dissimilarities,
            frame_timestamps_seconds=timestamps,
            per_frame_motion_complexity_scores=complexities,
            detection_strategy_name=strategy.strategy_name,
            fixed_cut_dissimilarity_threshold=strategy.fixed_cut_dissimilarity_threshold,
            asset_duration_seconds=timestamps[-1] + 0.2,
        )

        assert len(boundaries) >= 2

    def test_raises_video_decode_error_for_nonexistent_file(self) -> None:
        """Sampling frames from a nonexistent file raises VideoDecodeError."""
        from ingestion_inspection_service.adapters.frame_source import (
            VideoDecodeError,
        )

        with pytest.raises(VideoDecodeError):
            list(sample_frames_at_fixed_rate("/no/such/file/anywhere.mov"))


class TestAudioExtractionAgainstRealFile:
    """Tests extract_mono_pcm_via_ffmpeg against a real MP4."""

    def test_extracts_nonzero_length_pcm_from_real_file(
        self, synthetic_test_video_path: str
    ) -> None:
        """Real PCM audio is extracted with the expected sample rate and duration."""
        pcm_samples, sample_rate_hz = extract_mono_pcm_via_ffmpeg(
            synthetic_test_video_path, target_sample_rate_hz=48_000
        )

        assert sample_rate_hz == 48_000
        approx_duration = len(pcm_samples) / sample_rate_hz
        assert approx_duration == pytest.approx(2.0, abs=0.3)
        assert pcm_samples.dtype.kind == "f"
        assert pcm_samples.max() <= 1.0
        assert pcm_samples.min() >= -1.0
