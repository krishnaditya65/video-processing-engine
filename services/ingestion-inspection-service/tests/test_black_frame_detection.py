"""Unit tests for black-frame luminance statistics and range merging."""

from __future__ import annotations

import pytest

from ingestion_inspection_service.config import BlackFrameDetectionThresholds
from ingestion_inspection_service.pipeline.stages import (
    compute_luminance_statistics_for_frame,
    merge_black_frame_samples_into_ranges,
)
from tests.conftest import make_solid_color_frame


class TestComputeLuminanceStatisticsForFrame:
    """Tests for compute_luminance_statistics_for_frame."""

    def test_black_frame_has_near_zero_luminance(self) -> None:
        """A pure black BGR frame yields mean and max luminance near 0."""
        black_frame = make_solid_color_frame(32, 32, (0, 0, 0))
        mean_luminance, max_luminance = compute_luminance_statistics_for_frame(
            black_frame
        )
        assert mean_luminance == pytest.approx(0.0, abs=1e-6)
        assert max_luminance == pytest.approx(0.0, abs=1e-6)

    def test_white_frame_has_near_max_luminance(self) -> None:
        """A pure white BGR frame yields mean and max luminance near 255."""
        white_frame = make_solid_color_frame(32, 32, (255, 255, 255))
        mean_luminance, max_luminance = compute_luminance_statistics_for_frame(
            white_frame
        )
        assert mean_luminance == pytest.approx(255.0, abs=1.0)
        assert max_luminance == pytest.approx(255.0, abs=1e-6)

    def test_non_three_channel_frame_raises_value_error(self) -> None:
        """A grayscale (2-D) frame raises ValueError."""
        import numpy as np

        with pytest.raises(ValueError):
            compute_luminance_statistics_for_frame(np.zeros((32, 32), dtype="uint8"))


class TestMergeBlackFrameSamplesIntoRanges:
    """Tests for merge_black_frame_samples_into_ranges."""

    def test_no_black_frames_yields_empty_list(self) -> None:
        """All-bright samples produce no black-frame ranges."""
        timestamps = [0.0, 1.0, 2.0, 3.0]
        mean_lum = [200.0, 210.0, 195.0, 205.0]
        max_lum = [220.0, 230.0, 215.0, 225.0]

        ranges = merge_black_frame_samples_into_ranges(timestamps, mean_lum, max_lum)

        assert ranges == []

    def test_single_isolated_black_sample_below_minimum_is_dropped(self) -> None:
        """A single black sample, below the minimum-consecutive threshold, is dropped."""
        timestamps = [0.0, 1.0, 2.0, 3.0]
        mean_lum = [200.0, 5.0, 195.0, 205.0]
        max_lum = [220.0, 10.0, 215.0, 225.0]
        thresholds = BlackFrameDetectionThresholds(minimum_consecutive_black_frames=2)

        ranges = merge_black_frame_samples_into_ranges(
            timestamps, mean_lum, max_lum, thresholds
        )

        assert ranges == []

    def test_consecutive_black_samples_merge_into_one_range(self) -> None:
        """Three consecutive black samples merge into a single BlackFrameRange."""
        timestamps = [0.0, 1.0, 2.0, 3.0, 4.0, 5.0]
        mean_lum = [200.0, 3.0, 4.0, 5.0, 195.0, 205.0]
        max_lum = [220.0, 8.0, 9.0, 10.0, 215.0, 225.0]
        thresholds = BlackFrameDetectionThresholds(minimum_consecutive_black_frames=2)

        ranges = merge_black_frame_samples_into_ranges(
            timestamps, mean_lum, max_lum, thresholds
        )

        assert len(ranges) == 1
        assert ranges[0].start_timestamp_seconds == pytest.approx(1.0)
        assert ranges[0].end_timestamp_seconds == pytest.approx(3.0)
        assert ranges[0].sampled_frame_count == 3
        assert ranges[0].mean_luminance_during_range == pytest.approx(4.0)

    def test_two_separate_black_runs_produce_two_ranges(self) -> None:
        """Two black runs separated by bright frames produce two separate ranges."""
        timestamps = [float(i) for i in range(10)]
        mean_lum = [200.0, 3.0, 3.0, 200.0, 200.0, 200.0, 4.0, 4.0, 4.0, 200.0]
        max_lum = [220.0, 8.0, 8.0, 220.0, 220.0, 220.0, 9.0, 9.0, 9.0, 220.0]

        ranges = merge_black_frame_samples_into_ranges(timestamps, mean_lum, max_lum)

        assert len(ranges) == 2
        assert ranges[0].sampled_frame_count == 2
        assert ranges[1].sampled_frame_count == 3

    def test_trailing_black_run_is_captured(self) -> None:
        """A black run that extends to the very last sample is still captured."""
        timestamps = [0.0, 1.0, 2.0, 3.0]
        mean_lum = [200.0, 210.0, 3.0, 4.0]
        max_lum = [220.0, 230.0, 8.0, 9.0]

        ranges = merge_black_frame_samples_into_ranges(timestamps, mean_lum, max_lum)

        assert len(ranges) == 1
        assert ranges[0].end_timestamp_seconds == pytest.approx(3.0)

    def test_bright_highlight_over_dark_frame_is_not_classified_black(self) -> None:
        """Low mean luminance with a bright peak (e.g. titles) is not 'black'."""
        timestamps = [0.0, 1.0]
        mean_lum = [10.0, 10.0]
        max_lum = [250.0, 250.0]  # A bright highlight present despite low mean.

        ranges = merge_black_frame_samples_into_ranges(timestamps, mean_lum, max_lum)

        assert ranges == []

    def test_mismatched_input_lengths_raise_value_error(self) -> None:
        """Inconsistent input sequence lengths raise ValueError."""
        with pytest.raises(ValueError):
            merge_black_frame_samples_into_ranges([0.0, 1.0], [10.0], [10.0, 10.0])
