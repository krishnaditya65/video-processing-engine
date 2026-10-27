"""Rolling-window feature engineering for network-quality time series.

This module turns a raw stream of per-session network-quality samples,
each shaped as (timestamp_ms, measured_throughput_kbps, round_trip_time_ms),
into a fixed-width numeric feature vector suitable for input to the
Isolation Forest anomaly-detection model.

Design pattern -- Strategy:
    Each concrete feature computation (rolling mean/stddev, exponentially
    weighted moving average, rate-of-change) is implemented as a small pure
    function AND wrapped behind the ``RollingWindowFeatureExtractionStrategy``
    interface so that the set of features fed to the model can be swapped,
    extended, or A/B tested without touching the model or the pipeline code.
    ``CompositeFeatureExtractionStrategy`` composes many strategies into one,
    which is what the prediction pipeline uses by default.

All the ``compute_*`` functions in this module are pure: given the same
input list of samples they always return the same output, they perform no
I/O, and they mutate nothing. That purity is what makes them trivial to
unit test with small, hand-computable synthetic fixtures (see
``tests/test_feature_engineering.py``).
"""

from __future__ import annotations

import abc
import math
from dataclasses import dataclass
from typing import Sequence


@dataclass(frozen=True)
class NetworkQualitySample:
    """A single client-observed network-quality measurement.

    Attributes:
        timestamp_ms: Epoch milliseconds at which the sample was measured.
        measured_throughput_kbps: Instantaneous measured throughput, in
            kilobits per second, observed by the video player's ABR
            (adaptive bitrate) client.
        round_trip_time_ms: Round-trip time, in milliseconds, measured by
            the client (e.g. via a lightweight ping/ack on the CDN edge
            connection or manifest-fetch timing).
    """

    timestamp_ms: int
    measured_throughput_kbps: float
    round_trip_time_ms: float


# ---------------------------------------------------------------------------
# Pure statistical building blocks.
# ---------------------------------------------------------------------------


def compute_rolling_mean(values: Sequence[float], window_size: int) -> list[float]:
    """Compute the trailing rolling mean of ``values`` over ``window_size``.

    For index ``i`` the rolling mean is the arithmetic mean of
    ``values[max(0, i - window_size + 1) : i + 1]`` -- i.e. it uses only
    past and current samples (a causal/trailing window), which is required
    because at inference time future samples do not exist yet.

    Args:
        values: The sequence of raw numeric observations, in time order.
        window_size: The number of trailing samples to average over. Must
            be a positive integer. If ``window_size`` exceeds the number
            of samples seen so far at a given index, the shorter available
            window is used (a "partial" or "expanding" window at the
            series' start).

    Returns:
        A list the same length as ``values`` containing the rolling mean
        at each position.

    Raises:
        ValueError: If ``window_size`` is not a positive integer.
    """
    if window_size <= 0:
        raise ValueError("window_size must be a positive integer")

    rolling_means: list[float] = []
    for index in range(len(values)):
        window_start = max(0, index - window_size + 1)
        window = values[window_start : index + 1]
        rolling_means.append(sum(window) / len(window))
    return rolling_means


def compute_rolling_standard_deviation(
    values: Sequence[float], window_size: int
) -> list[float]:
    """Compute the trailing rolling (population) standard deviation.

    This is the primary "jitter" measure used by the model: a rolling
    standard deviation of throughput or RTT captures how erratically the
    network is behaving over the trailing window, independent of its
    absolute level.

    Args:
        values: The sequence of raw numeric observations, in time order.
        window_size: The number of trailing samples per window. See
            ``compute_rolling_mean`` for the windowing semantics.

    Returns:
        A list the same length as ``values`` containing the rolling
        population standard deviation at each position. A window of size
        1 yields a standard deviation of 0.0.

    Raises:
        ValueError: If ``window_size`` is not a positive integer.
    """
    if window_size <= 0:
        raise ValueError("window_size must be a positive integer")

    rolling_std_devs: list[float] = []
    for index in range(len(values)):
        window_start = max(0, index - window_size + 1)
        window = values[window_start : index + 1]
        window_mean = sum(window) / len(window)
        variance = sum((value - window_mean) ** 2 for value in window) / len(window)
        rolling_std_devs.append(math.sqrt(variance))
    return rolling_std_devs


def compute_rate_of_change(values: Sequence[float]) -> list[float]:
    """Compute the first-difference (sample-over-sample rate of change).

    ``rate_of_change[i] = values[i] - values[i - 1]``, with
    ``rate_of_change[0] = 0.0`` since there is no prior sample to
    difference against. A sustained negative rate of change in throughput
    is a strong precursor signal of an imminent bandwidth-degradation
    event.

    Args:
        values: The sequence of raw numeric observations, in time order.

    Returns:
        A list the same length as ``values`` with the first difference at
        each position.
    """
    if not values:
        return []
    rate_of_change: list[float] = [0.0]
    for index in range(1, len(values)):
        rate_of_change.append(values[index] - values[index - 1])
    return rate_of_change


def compute_exponentially_weighted_moving_average(
    values: Sequence[float], smoothing_alpha: float
) -> list[float]:
    """Compute an exponentially weighted moving average (EWMA) of ``values``.

    The EWMA gives more weight to recent samples than a simple rolling
    mean while still smoothing out single-sample noise spikes, which makes
    it well suited to tracking a "current trend level" of throughput/RTT
    for early degradation detection.

    Recurrence: ``ewma[0] = values[0]``;
    ``ewma[i] = alpha * values[i] + (1 - alpha) * ewma[i - 1]`` for i > 0.

    Args:
        values: The sequence of raw numeric observations, in time order.
        smoothing_alpha: The smoothing factor in the open interval
            (0, 1]. Larger values weight recent samples more heavily
            (less smoothing); smaller values smooth more aggressively.

    Returns:
        A list the same length as ``values`` containing the EWMA at each
        position.

    Raises:
        ValueError: If ``smoothing_alpha`` is not in (0, 1].
    """
    if not (0.0 < smoothing_alpha <= 1.0):
        raise ValueError("smoothing_alpha must be in the interval (0, 1]")
    if not values:
        return []

    ewma_values: list[float] = [float(values[0])]
    for index in range(1, len(values)):
        previous_ewma = ewma_values[-1]
        current_value = values[index]
        ewma_values.append(
            smoothing_alpha * current_value + (1.0 - smoothing_alpha) * previous_ewma
        )
    return ewma_values


def compute_rolling_throughput_jitter_features(
    samples: Sequence[NetworkQualitySample],
    rolling_window_size: int = 5,
    ewma_smoothing_alpha: float = 0.3,
) -> dict[str, list[float]]:
    """Compute the full set of rolling-window features for a sample stream.

    This is the primary documented entry point most callers should use: it
    fans a raw sample stream out to throughput and RTT series and computes
    rolling mean, rolling standard deviation ("jitter"), rate of change,
    and EWMA for both series.

    Args:
        samples: The session's network-quality samples, in ascending
            timestamp order.
        rolling_window_size: Trailing window width, in samples, used for
            the rolling mean/stddev computations. Defaults to 5 samples.
        ewma_smoothing_alpha: Smoothing factor for the EWMA computations.
            Defaults to 0.3.

    Returns:
        A dict mapping feature-series name to a list of per-sample values,
        each list the same length as ``samples``. Keys:
            ``throughput_rolling_mean_kbps``,
            ``throughput_rolling_jitter_stddev_kbps``,
            ``throughput_rate_of_change_kbps``,
            ``throughput_ewma_kbps``,
            ``rtt_rolling_mean_ms``,
            ``rtt_rolling_jitter_stddev_ms``,
            ``rtt_rate_of_change_ms``,
            ``rtt_ewma_ms``.
    """
    throughput_series = [sample.measured_throughput_kbps for sample in samples]
    rtt_series = [sample.round_trip_time_ms for sample in samples]

    return {
        "throughput_rolling_mean_kbps": compute_rolling_mean(
            throughput_series, rolling_window_size
        ),
        "throughput_rolling_jitter_stddev_kbps": compute_rolling_standard_deviation(
            throughput_series, rolling_window_size
        ),
        "throughput_rate_of_change_kbps": compute_rate_of_change(throughput_series),
        "throughput_ewma_kbps": compute_exponentially_weighted_moving_average(
            throughput_series, ewma_smoothing_alpha
        ),
        "rtt_rolling_mean_ms": compute_rolling_mean(rtt_series, rolling_window_size),
        "rtt_rolling_jitter_stddev_ms": compute_rolling_standard_deviation(
            rtt_series, rolling_window_size
        ),
        "rtt_rate_of_change_ms": compute_rate_of_change(rtt_series),
        "rtt_ewma_ms": compute_exponentially_weighted_moving_average(
            rtt_series, ewma_smoothing_alpha
        ),
    }


# ---------------------------------------------------------------------------
# Strategy pattern.
# ---------------------------------------------------------------------------


class RollingWindowFeatureExtractionStrategy(abc.ABC):
    """Strategy interface for pluggable feature-extraction algorithms.

    Concrete strategies implement ``extract_features_from_samples`` and
    ``feature_names`` so the prediction pipeline can be configured with any
    combination of strategies (e.g. throughput/RTT rolling statistics only,
    EWMA only, or a composite of everything) without any change to the
    pipeline, model wrapper, or API layer.
    """

    @abc.abstractmethod
    def extract_features_from_samples(
        self, samples: Sequence[NetworkQualitySample]
    ) -> dict[str, list[float]]:
        """Extract this strategy's named feature series from ``samples``.

        Args:
            samples: The session's network-quality samples, ascending by
                timestamp.

        Returns:
            A dict mapping feature name to a per-sample list of values,
            each list the same length as ``samples``.
        """
        raise NotImplementedError

    @property
    @abc.abstractmethod
    def feature_names(self) -> tuple[str, ...]:
        """Return the ordered tuple of feature-series names this strategy produces."""
        raise NotImplementedError


class ThroughputJitterRollingStatisticsStrategy(RollingWindowFeatureExtractionStrategy):
    """Rolling mean/stddev ("jitter") features for throughput and RTT."""

    def __init__(self, rolling_window_size: int = 5) -> None:
        """Initialize the strategy.

        Args:
            rolling_window_size: Trailing window width, in samples, used
                for the rolling mean/stddev computations.
        """
        self._rolling_window_size = rolling_window_size

    def extract_features_from_samples(
        self, samples: Sequence[NetworkQualitySample]
    ) -> dict[str, list[float]]:
        throughput_series = [sample.measured_throughput_kbps for sample in samples]
        rtt_series = [sample.round_trip_time_ms for sample in samples]
        return {
            "throughput_rolling_mean_kbps": compute_rolling_mean(
                throughput_series, self._rolling_window_size
            ),
            "throughput_rolling_jitter_stddev_kbps": compute_rolling_standard_deviation(
                throughput_series, self._rolling_window_size
            ),
            "rtt_rolling_mean_ms": compute_rolling_mean(
                rtt_series, self._rolling_window_size
            ),
            "rtt_rolling_jitter_stddev_ms": compute_rolling_standard_deviation(
                rtt_series, self._rolling_window_size
            ),
        }

    @property
    def feature_names(self) -> tuple[str, ...]:
        return (
            "throughput_rolling_mean_kbps",
            "throughput_rolling_jitter_stddev_kbps",
            "rtt_rolling_mean_ms",
            "rtt_rolling_jitter_stddev_ms",
        )


class ExponentialWeightedMovingAverageStrategy(RollingWindowFeatureExtractionStrategy):
    """EWMA-trend features for throughput and RTT."""

    def __init__(self, ewma_smoothing_alpha: float = 0.3) -> None:
        """Initialize the strategy.

        Args:
            ewma_smoothing_alpha: Smoothing factor in (0, 1] used for both
                the throughput and RTT EWMA computations.
        """
        self._ewma_smoothing_alpha = ewma_smoothing_alpha

    def extract_features_from_samples(
        self, samples: Sequence[NetworkQualitySample]
    ) -> dict[str, list[float]]:
        throughput_series = [sample.measured_throughput_kbps for sample in samples]
        rtt_series = [sample.round_trip_time_ms for sample in samples]
        return {
            "throughput_ewma_kbps": compute_exponentially_weighted_moving_average(
                throughput_series, self._ewma_smoothing_alpha
            ),
            "rtt_ewma_ms": compute_exponentially_weighted_moving_average(
                rtt_series, self._ewma_smoothing_alpha
            ),
        }

    @property
    def feature_names(self) -> tuple[str, ...]:
        return ("throughput_ewma_kbps", "rtt_ewma_ms")


class RateOfChangeFeatureStrategy(RollingWindowFeatureExtractionStrategy):
    """Sample-over-sample rate-of-change features for throughput and RTT."""

    def extract_features_from_samples(
        self, samples: Sequence[NetworkQualitySample]
    ) -> dict[str, list[float]]:
        throughput_series = [sample.measured_throughput_kbps for sample in samples]
        rtt_series = [sample.round_trip_time_ms for sample in samples]
        return {
            "throughput_rate_of_change_kbps": compute_rate_of_change(throughput_series),
            "rtt_rate_of_change_ms": compute_rate_of_change(rtt_series),
        }

    @property
    def feature_names(self) -> tuple[str, ...]:
        return ("throughput_rate_of_change_kbps", "rtt_rate_of_change_ms")


class CompositeFeatureExtractionStrategy(RollingWindowFeatureExtractionStrategy):
    """Composes multiple strategies into a single combined strategy.

    This is the strategy the prediction pipeline uses by default: it
    fans out to each configured sub-strategy and merges their outputs into
    one feature dict, preserving a stable, documented ordering of feature
    names via ``feature_names`` (used to build the fixed-width numeric
    vector the Isolation Forest expects).
    """

    def __init__(self, strategies: Sequence[RollingWindowFeatureExtractionStrategy]) -> None:
        """Initialize the composite with an ordered list of sub-strategies.

        Args:
            strategies: The sub-strategies to compose, applied in the
                given order. Feature name collisions between strategies
                will result in the later strategy's values overwriting the
                earlier one's for that name.
        """
        self._strategies = list(strategies)

    def extract_features_from_samples(
        self, samples: Sequence[NetworkQualitySample]
    ) -> dict[str, list[float]]:
        combined_features: dict[str, list[float]] = {}
        for strategy in self._strategies:
            combined_features.update(strategy.extract_features_from_samples(samples))
        return combined_features

    @property
    def feature_names(self) -> tuple[str, ...]:
        names: list[str] = []
        for strategy in self._strategies:
            for name in strategy.feature_names:
                if name not in names:
                    names.append(name)
        return tuple(names)


def build_default_feature_extraction_pipeline_strategy(
    rolling_window_size: int = 5, ewma_smoothing_alpha: float = 0.3
) -> CompositeFeatureExtractionStrategy:
    """Build the default composite strategy used in production.

    Combines rolling mean/stddev jitter statistics, EWMA trend features,
    and rate-of-change features -- matching the feature set the shipped
    Isolation Forest model was trained on (see
    ``ml/training_evaluation_harness.py``).

    Args:
        rolling_window_size: Trailing window width, in samples, for the
            rolling mean/stddev sub-strategy.
        ewma_smoothing_alpha: Smoothing factor in (0, 1] for the EWMA
            sub-strategy.

    Returns:
        A configured ``CompositeFeatureExtractionStrategy``.
    """
    return CompositeFeatureExtractionStrategy(
        [
            ThroughputJitterRollingStatisticsStrategy(rolling_window_size),
            ExponentialWeightedMovingAverageStrategy(ewma_smoothing_alpha),
            RateOfChangeFeatureStrategy(),
        ]
    )
