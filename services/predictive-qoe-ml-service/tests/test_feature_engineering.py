"""Unit tests for the rolling-window feature engineering module.

Each expected value below is hand-computed from the input fixture so
these tests verify correctness against arithmetic worked out independently
of the implementation, not just "does the function run".
"""

from __future__ import annotations

import math

import pytest

from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    NetworkQualitySample,
    compute_exponentially_weighted_moving_average,
    compute_rate_of_change,
    compute_rolling_mean,
    compute_rolling_standard_deviation,
    compute_rolling_throughput_jitter_features,
)


def test_compute_rolling_mean_matches_hand_computed_values_for_window_of_three():
    """[1,2,3,4,5] with window=3 -> [1, 1.5, 2, 3, 4] (hand-computed trailing means)."""
    values = [1.0, 2.0, 3.0, 4.0, 5.0]
    result = compute_rolling_mean(values, window_size=3)
    expected = [1.0, 1.5, 2.0, 3.0, 4.0]
    assert result == pytest.approx(expected)


def test_compute_rolling_mean_with_window_larger_than_series_uses_full_series():
    """A window bigger than the whole series should just average everything seen so far."""
    values = [2.0, 4.0, 6.0]
    result = compute_rolling_mean(values, window_size=10)
    # i0: mean([2])=2 ; i1: mean([2,4])=3 ; i2: mean([2,4,6])=4
    assert result == pytest.approx([2.0, 3.0, 4.0])


def test_compute_rolling_mean_rejects_non_positive_window_size():
    """window_size must be a positive integer."""
    with pytest.raises(ValueError):
        compute_rolling_mean([1.0, 2.0], window_size=0)


def test_compute_rolling_standard_deviation_matches_hand_computed_values():
    """[1,2,3,4,5] with window=3 -> population stddevs hand-computed below."""
    values = [1.0, 2.0, 3.0, 4.0, 5.0]
    result = compute_rolling_standard_deviation(values, window_size=3)
    # i0: [1] -> std=0
    # i1: [1,2] mean=1.5 var=((0.5)^2*2)/2=0.25 -> std=0.5
    # i2: [1,2,3] mean=2 var=(1+0+1)/3=0.6667 -> std=0.8165
    # i3: [2,3,4] mean=3 var=(1+0+1)/3=0.6667 -> std=0.8165
    # i4: [3,4,5] mean=4 var=(1+0+1)/3=0.6667 -> std=0.8165
    expected = [0.0, 0.5, math.sqrt(2 / 3), math.sqrt(2 / 3), math.sqrt(2 / 3)]
    assert result == pytest.approx(expected, abs=1e-9)


def test_compute_rolling_standard_deviation_of_constant_series_is_zero():
    """A perfectly constant series has zero jitter at every point."""
    values = [10.0, 10.0, 10.0, 10.0]
    result = compute_rolling_standard_deviation(values, window_size=2)
    assert result == pytest.approx([0.0, 0.0, 0.0, 0.0])


def test_compute_rate_of_change_matches_hand_computed_first_differences():
    """First element of rate-of-change is always 0.0 (no prior sample to diff against)."""
    values = [5.0, 8.0, 3.0, 3.0, 10.0]
    result = compute_rate_of_change(values)
    expected = [0.0, 3.0, -5.0, 0.0, 7.0]
    assert result == pytest.approx(expected)


def test_compute_rate_of_change_of_empty_series_is_empty():
    """Edge case: an empty input series produces an empty output list."""
    assert compute_rate_of_change([]) == []


def test_compute_exponentially_weighted_moving_average_matches_hand_computed_recurrence():
    """alpha=0.5 EWMA of [1,2,3,4,5], hand-unrolled via the documented recurrence."""
    values = [1.0, 2.0, 3.0, 4.0, 5.0]
    result = compute_exponentially_weighted_moving_average(values, smoothing_alpha=0.5)
    # ewma0=1
    # ewma1=0.5*2+0.5*1=1.5
    # ewma2=0.5*3+0.5*1.5=2.25
    # ewma3=0.5*4+0.5*2.25=3.125
    # ewma4=0.5*5+0.5*3.125=4.0625
    expected = [1.0, 1.5, 2.25, 3.125, 4.0625]
    assert result == pytest.approx(expected)


def test_compute_exponentially_weighted_moving_average_rejects_out_of_range_alpha():
    """smoothing_alpha must be in the interval (0, 1]."""
    with pytest.raises(ValueError):
        compute_exponentially_weighted_moving_average([1.0, 2.0], smoothing_alpha=0.0)
    with pytest.raises(ValueError):
        compute_exponentially_weighted_moving_average([1.0, 2.0], smoothing_alpha=1.5)


def test_compute_rolling_throughput_jitter_features_returns_all_expected_series_with_correct_length():
    """The composite feature function should return every documented key, correctly sized."""
    samples = [
        NetworkQualitySample(
            timestamp_ms=i * 1000, measured_throughput_kbps=5000.0 - i * 100, round_trip_time_ms=30.0 + i
        )
        for i in range(6)
    ]
    features = compute_rolling_throughput_jitter_features(
        samples, rolling_window_size=3, ewma_smoothing_alpha=0.4
    )

    expected_keys = {
        "throughput_rolling_mean_kbps",
        "throughput_rolling_jitter_stddev_kbps",
        "throughput_rate_of_change_kbps",
        "throughput_ewma_kbps",
        "rtt_rolling_mean_ms",
        "rtt_rolling_jitter_stddev_ms",
        "rtt_rate_of_change_ms",
        "rtt_ewma_ms",
    }
    assert set(features.keys()) == expected_keys
    for series in features.values():
        assert len(series) == len(samples)

    # Throughput is monotonically decreasing by a constant 100 kbps/sample,
    # so the rate-of-change series (after the first, always-0.0 element)
    # should be a constant -100.0.
    assert features["throughput_rate_of_change_kbps"][0] == pytest.approx(0.0)
    assert all(
        value == pytest.approx(-100.0)
        for value in features["throughput_rate_of_change_kbps"][1:]
    )
