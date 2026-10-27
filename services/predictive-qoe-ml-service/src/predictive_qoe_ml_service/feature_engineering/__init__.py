"""Feature engineering package.

Exposes pure functions and Strategy-pattern classes that convert a raw
stream of network-quality samples (timestamp_ms, measured_throughput_kbps,
round_trip_time_ms) into rolling-window statistical features consumable by
the Isolation Forest anomaly-detection model.
"""

from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    NetworkQualitySample,
    RollingWindowFeatureExtractionStrategy,
    ThroughputJitterRollingStatisticsStrategy,
    ExponentialWeightedMovingAverageStrategy,
    RateOfChangeFeatureStrategy,
    CompositeFeatureExtractionStrategy,
    compute_rolling_mean,
    compute_rolling_standard_deviation,
    compute_rate_of_change,
    compute_exponentially_weighted_moving_average,
    compute_rolling_throughput_jitter_features,
    build_default_feature_extraction_pipeline_strategy,
)

__all__ = [
    "NetworkQualitySample",
    "RollingWindowFeatureExtractionStrategy",
    "ThroughputJitterRollingStatisticsStrategy",
    "ExponentialWeightedMovingAverageStrategy",
    "RateOfChangeFeatureStrategy",
    "CompositeFeatureExtractionStrategy",
    "compute_rolling_mean",
    "compute_rolling_standard_deviation",
    "compute_rate_of_change",
    "compute_exponentially_weighted_moving_average",
    "compute_rolling_throughput_jitter_features",
    "build_default_feature_extraction_pipeline_strategy",
]
