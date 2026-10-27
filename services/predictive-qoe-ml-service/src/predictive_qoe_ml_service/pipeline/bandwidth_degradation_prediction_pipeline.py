"""The end-to-end bandwidth-degradation prediction pipeline.

Design pattern -- Pipeline:
    This module implements the classic linear Pipeline pattern: each stage
    consumes the previous stage's output and produces a well-defined
    output for the next stage, with no stage reaching backward or sideways
    into another stage's internals.

        raw NetworkQualitySample stream
            -> (1) feature extraction stage (Strategy pattern; see
                   feature_engineering package)
            -> (2) vectorization stage (flatten named features into the
                   fixed-order numeric vector the model expects)
            -> (3) model inference stage (IsolationForestBandwidthDegradationModel)
            -> (4) decision stage (threshold the confidence score, compute
                   a recommended downshift bitrate and a
                   seconds-before-buffer-empty estimate)

    Keeping these stages separate and independently testable is what lets
    the training harness, the pure feature-engineering unit tests, and the
    FastAPI endpoint tests each exercise only the stage(s) they care about.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Sequence

import numpy as np

from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    NetworkQualitySample,
    RollingWindowFeatureExtractionStrategy,
    build_default_feature_extraction_pipeline_strategy,
)
from predictive_qoe_ml_service.ml.isolation_forest_bandwidth_degradation_model import (
    BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES,
    IsolationForestBandwidthDegradationModel,
)

#: Confidence score at/above which the pipeline flags the session as
#: having an imminent bandwidth-degradation event.
DEFAULT_DEGRADATION_DECISION_THRESHOLD = 0.5

#: Floor bitrate never recommended below, in kbps -- roughly a low-quality
#: audio-plus-360p-ish safety floor rather than dropping to zero.
MINIMUM_RECOMMENDED_DOWNSHIFT_BITRATE_KBPS = 400.0

#: Safety headroom multiplier applied to the most recent observed
#: throughput when recommending a downshift target, so the recommended
#: bitrate sits comfortably under currently available bandwidth rather
#: than exactly at it.
DOWNSHIFT_BITRATE_SAFETY_HEADROOM_FACTOR = 0.75


@dataclass(frozen=True)
class BandwidthDegradationPredictionDecision:
    """The final output of the prediction pipeline for one session.

    Attributes:
        is_degradation_imminent: Whether the pipeline predicts a
            bandwidth-degradation event is imminent for this session.
        degradation_confidence_score: A float in [0, 1]; higher means more
            confident a degradation event is imminent.
        recommended_downshift_bitrate_kbps: The bitrate, in kbps, the
            pipeline recommends the client player proactively switch down
            to. Only meaningfully below the client's current bitrate when
            ``is_degradation_imminent`` is True; otherwise mirrors current
            throughput with headroom applied as a conservative default.
        seconds_before_buffer_empty_estimate: An estimate, in seconds, of
            how long the client's playback buffer can sustain current
            playback before it empties if the degrading throughput trend
            continues. ``None`` when a buffer level was not supplied or
            when no degradation is predicted (buffer is not at risk).
    """

    is_degradation_imminent: bool
    degradation_confidence_score: float
    recommended_downshift_bitrate_kbps: float
    seconds_before_buffer_empty_estimate: float | None


class BandwidthDegradationPredictionPipeline:
    """Orchestrates feature extraction, model inference, and decisioning.

    Instances are cheap to construct and hold no per-request mutable
    state, so a single pipeline instance is safe to reuse (and is reused)
    across concurrent FastAPI requests.
    """

    def __init__(
        self,
        model: IsolationForestBandwidthDegradationModel,
        feature_extraction_strategy: RollingWindowFeatureExtractionStrategy | None = None,
        degradation_decision_threshold: float = DEFAULT_DEGRADATION_DECISION_THRESHOLD,
    ) -> None:
        """Configure the pipeline.

        Args:
            model: A trained ``IsolationForestBandwidthDegradationModel``.
            feature_extraction_strategy: The Strategy-pattern feature
                extractor to use. Defaults to
                ``build_default_feature_extraction_pipeline_strategy()``
                if not provided.
            degradation_decision_threshold: Confidence-score threshold at
                or above which ``is_degradation_imminent`` is set True.
        """
        self._model = model
        self._feature_extraction_strategy = (
            feature_extraction_strategy or build_default_feature_extraction_pipeline_strategy()
        )
        self._degradation_decision_threshold = degradation_decision_threshold

    def predict_imminent_bandwidth_degradation_from_network_samples(
        self,
        samples: Sequence[NetworkQualitySample],
        current_player_buffer_seconds: float | None = None,
        current_encoded_bitrate_kbps: float | None = None,
    ) -> BandwidthDegradationPredictionDecision:
        """Run the full pipeline for one session's recent network samples.

        Args:
            samples: The session's recent network-quality samples,
                ascending by timestamp. Must contain at least one sample.
            current_player_buffer_seconds: The client player's current
                playback buffer level, in seconds, if known. Used to
                compute ``seconds_before_buffer_empty_estimate``.
            current_encoded_bitrate_kbps: The bitrate, in kbps, the client
                is currently playing at, if known. Used as a fallback
                basis for the recommended downshift bitrate when recent
                throughput samples are noisy/sparse.

        Returns:
            A ``BandwidthDegradationPredictionDecision``.

        Raises:
            ValueError: If ``samples`` is empty.
        """
        if not samples:
            raise ValueError("samples must contain at least one NetworkQualitySample")

        # Stage 1: feature extraction (Strategy pattern).
        named_feature_series = self._feature_extraction_strategy.extract_features_from_samples(
            samples
        )

        # Stage 2: vectorization -- take the most recent value of each
        # named feature series, in the model's fixed, documented column
        # order.
        latest_feature_vector = np.array(
            [
                [named_feature_series[name][-1] for name in BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES]
            ]
        )

        # Stage 3: model inference.
        anomaly_score_result = self._model.score_feature_vectors(latest_feature_vector)[0]

        # Stage 4: decisioning.
        is_degradation_imminent = (
            anomaly_score_result.degradation_confidence_score
            >= self._degradation_decision_threshold
        )

        most_recent_throughput_kbps = samples[-1].measured_throughput_kbps
        recommended_downshift_bitrate_kbps = self._compute_recommended_downshift_bitrate_kbps(
            most_recent_throughput_kbps=most_recent_throughput_kbps,
            current_encoded_bitrate_kbps=current_encoded_bitrate_kbps,
            is_degradation_imminent=is_degradation_imminent,
        )

        seconds_before_buffer_empty_estimate = self._estimate_seconds_before_buffer_empty(
            current_player_buffer_seconds=current_player_buffer_seconds,
            current_encoded_bitrate_kbps=current_encoded_bitrate_kbps,
            most_recent_throughput_kbps=most_recent_throughput_kbps,
            is_degradation_imminent=is_degradation_imminent,
        )

        return BandwidthDegradationPredictionDecision(
            is_degradation_imminent=is_degradation_imminent,
            degradation_confidence_score=anomaly_score_result.degradation_confidence_score,
            recommended_downshift_bitrate_kbps=recommended_downshift_bitrate_kbps,
            seconds_before_buffer_empty_estimate=seconds_before_buffer_empty_estimate,
        )

    def _compute_recommended_downshift_bitrate_kbps(
        self,
        most_recent_throughput_kbps: float,
        current_encoded_bitrate_kbps: float | None,
        is_degradation_imminent: bool,
    ) -> float:
        """Compute a conservative recommended target bitrate, in kbps.

        When degradation is imminent, recommends
        ``most_recent_throughput_kbps * DOWNSHIFT_BITRATE_SAFETY_HEADROOM_FACTOR``,
        clamped to a sane floor. When no degradation is predicted, mirrors
        the client's current bitrate (no change recommended) if known,
        otherwise falls back to the same headroom-adjusted throughput
        basis.

        Args:
            most_recent_throughput_kbps: The most recent measured
                throughput sample, in kbps.
            current_encoded_bitrate_kbps: The client's current playback
                bitrate, in kbps, if known.
            is_degradation_imminent: Whether the pipeline predicted an
                imminent degradation event.

        Returns:
            A recommended bitrate in kbps, never below
            ``MINIMUM_RECOMMENDED_DOWNSHIFT_BITRATE_KBPS``.
        """
        if not is_degradation_imminent and current_encoded_bitrate_kbps is not None:
            return max(
                MINIMUM_RECOMMENDED_DOWNSHIFT_BITRATE_KBPS, current_encoded_bitrate_kbps
            )

        headroom_adjusted_bitrate = (
            most_recent_throughput_kbps * DOWNSHIFT_BITRATE_SAFETY_HEADROOM_FACTOR
        )
        return max(MINIMUM_RECOMMENDED_DOWNSHIFT_BITRATE_KBPS, headroom_adjusted_bitrate)

    def _estimate_seconds_before_buffer_empty(
        self,
        current_player_buffer_seconds: float | None,
        current_encoded_bitrate_kbps: float | None,
        most_recent_throughput_kbps: float,
        is_degradation_imminent: bool,
    ) -> float | None:
        """Estimate seconds until the client's playback buffer empties.

        Models buffer drain as a simple deficit-rate calculation: if the
        client is consuming the buffer at ``current_encoded_bitrate_kbps``
        but only receiving ``most_recent_throughput_kbps`` of fresh data
        per second, the buffer drains at
        ``(current_encoded_bitrate_kbps - most_recent_throughput_kbps) / current_encoded_bitrate_kbps``
        seconds of buffered playback per wall-clock second. This is a
        deliberately simple, documented heuristic -- not a full ABR
        buffer-model simulation -- appropriate for an early-warning
        estimate rather than a precise countdown.

        Args:
            current_player_buffer_seconds: Current buffer level in
                seconds, if known.
            current_encoded_bitrate_kbps: Current playback bitrate in
                kbps, if known.
            most_recent_throughput_kbps: Most recent measured throughput,
                in kbps.
            is_degradation_imminent: Whether degradation was predicted.

        Returns:
            Seconds until buffer-empty, or ``None`` if the inputs needed
            to compute it are unavailable, or if throughput is not
            currently outpaced by the encoded bitrate (buffer is not
            draining).
        """
        if not is_degradation_imminent:
            return None
        if current_player_buffer_seconds is None or current_encoded_bitrate_kbps is None:
            return None
        if current_encoded_bitrate_kbps <= 0:
            return None

        drain_deficit_kbps = current_encoded_bitrate_kbps - most_recent_throughput_kbps
        if drain_deficit_kbps <= 0:
            # Throughput still exceeds the playback consumption rate --
            # buffer is not currently draining despite the anomaly flag.
            return None

        buffer_drain_rate_seconds_per_second = (
            drain_deficit_kbps / current_encoded_bitrate_kbps
        )
        return current_player_buffer_seconds / buffer_drain_rate_seconds_per_second
