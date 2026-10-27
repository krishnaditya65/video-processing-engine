"""Pipeline package.

Contains the Pipeline-pattern orchestration that chains:
raw network-quality samples -> engineered features -> Isolation Forest
inference -> a final downshift decision with a buffer-empty time estimate.
"""

from predictive_qoe_ml_service.pipeline.bandwidth_degradation_prediction_pipeline import (
    BandwidthDegradationPredictionDecision,
    BandwidthDegradationPredictionPipeline,
)

__all__ = [
    "BandwidthDegradationPredictionDecision",
    "BandwidthDegradationPredictionPipeline",
]
