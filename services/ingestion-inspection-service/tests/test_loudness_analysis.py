"""Unit tests for the approximate EBU R128-inspired loudness computation.

Uses synthetically generated sine-wave PCM signals at known amplitudes,
so the expected LUFS-ish loudness ordering (quieter signal -> lower
loudness value) can be asserted without any real audio file.
"""

from __future__ import annotations

import numpy as np
import pytest

from ingestion_inspection_service.config import LoudnessAnalysisConfiguration
from ingestion_inspection_service.pipeline.stages import (
    compute_approximate_integrated_loudness_lufs,
)


def _make_sine_wave_pcm(
    amplitude: float, duration_seconds: float = 2.0, sample_rate_hz: int = 48_000
) -> np.ndarray:
    """Builds a mono sine-wave PCM signal at a given peak amplitude.

    Args:
        amplitude: Peak amplitude in ``[0, 1]``.
        duration_seconds: Signal duration in seconds.
        sample_rate_hz: Sample rate in Hz.

    Returns:
        numpy.ndarray: 1-D float64 array of sine-wave samples.
    """
    sample_count = int(duration_seconds * sample_rate_hz)
    t = np.arange(sample_count) / sample_rate_hz
    return amplitude * np.sin(2 * np.pi * 440.0 * t)


class TestComputeApproximateIntegratedLoudnessLufs:
    """Tests for compute_approximate_integrated_loudness_lufs."""

    def test_louder_signal_yields_higher_integrated_loudness(self) -> None:
        """A higher-amplitude sine wave yields higher estimated LUFS."""
        quiet_signal = _make_sine_wave_pcm(amplitude=0.05)
        loud_signal = _make_sine_wave_pcm(amplitude=0.5)

        quiet_result = compute_approximate_integrated_loudness_lufs(
            quiet_signal, 48_000
        )
        loud_result = compute_approximate_integrated_loudness_lufs(
            loud_signal, 48_000
        )

        assert loud_result.integrated_loudness_lufs > quiet_result.integrated_loudness_lufs

    def test_doubling_amplitude_increases_loudness_by_expected_order(self) -> None:
        """Doubling amplitude should raise loudness by roughly 6 dB (power^2 -> +6dB)."""
        base_signal = _make_sine_wave_pcm(amplitude=0.1)
        doubled_signal = _make_sine_wave_pcm(amplitude=0.2)

        base_result = compute_approximate_integrated_loudness_lufs(base_signal, 48_000)
        doubled_result = compute_approximate_integrated_loudness_lufs(
            doubled_signal, 48_000
        )

        delta = (
            doubled_result.integrated_loudness_lufs
            - base_result.integrated_loudness_lufs
        )
        # Doubling amplitude quadruples power -> +10*log10(4) ~= +6.02 dB.
        assert delta == pytest.approx(6.02, abs=0.5)

    def test_conforms_to_target_flag_reflects_tolerance(self) -> None:
        """conforms_to_target is True only when within the configured tolerance."""
        configuration = LoudnessAnalysisConfiguration(
            streaming_platform_target_lufs=-16.0,
            loudness_conformance_tolerance_lufs=1.0,
        )
        # Amplitude tuned so the sine wave's integrated loudness lands
        # close to -16 LUFS (RMS of sine = amplitude/sqrt(2); LUFS
        # formula is -0.691 + 10*log10(mean_square)).
        near_target_signal = _make_sine_wave_pcm(amplitude=0.35)

        result = compute_approximate_integrated_loudness_lufs(
            near_target_signal, 48_000, configuration
        )

        assert result.target_lufs_used == pytest.approx(-16.0)
        # Whatever the exact value, the flag must exactly match the
        # documented tolerance comparison.
        expected_conforms = (
            abs(result.integrated_loudness_lufs - configuration.streaming_platform_target_lufs)
            <= configuration.loudness_conformance_tolerance_lufs
        )
        assert result.conforms_to_target == expected_conforms

    def test_true_peak_estimate_reflects_sample_peak(self) -> None:
        """true_peak_estimate_dbtp is 0 dBTP for a full-scale (amplitude=1.0) signal."""
        full_scale_signal = _make_sine_wave_pcm(amplitude=1.0)

        result = compute_approximate_integrated_loudness_lufs(
            full_scale_signal, 48_000
        )

        assert result.true_peak_estimate_dbtp == pytest.approx(0.0, abs=0.1)

    def test_silence_does_not_raise_and_yields_very_low_loudness(self) -> None:
        """A silent (all-zero) signal is handled without raising and scores very low."""
        silence = np.zeros(48_000, dtype=np.float64)

        result = compute_approximate_integrated_loudness_lufs(silence, 48_000)

        assert result.integrated_loudness_lufs < -60.0

    def test_empty_signal_raises_value_error(self) -> None:
        """An empty PCM array raises ValueError."""
        with pytest.raises(ValueError):
            compute_approximate_integrated_loudness_lufs(np.array([]), 48_000)

    def test_multichannel_input_raises_value_error(self) -> None:
        """A non-1D (e.g. stereo, unsummed) PCM array raises ValueError."""
        stereo_like = np.zeros((100, 2), dtype=np.float64)
        with pytest.raises(ValueError):
            compute_approximate_integrated_loudness_lufs(stereo_like, 48_000)

    def test_non_positive_sample_rate_raises_value_error(self) -> None:
        """A zero or negative sample rate raises ValueError."""
        signal = _make_sine_wave_pcm(amplitude=0.2)
        with pytest.raises(ValueError):
            compute_approximate_integrated_loudness_lufs(signal, 0)

    def test_short_signal_shorter_than_one_block_does_not_raise(self) -> None:
        """A signal shorter than the 400ms measurement block is handled gracefully."""
        very_short_signal = _make_sine_wave_pcm(
            amplitude=0.3, duration_seconds=0.05, sample_rate_hz=48_000
        )
        result = compute_approximate_integrated_loudness_lufs(
            very_short_signal, 48_000
        )
        assert isinstance(result.integrated_loudness_lufs, float)
