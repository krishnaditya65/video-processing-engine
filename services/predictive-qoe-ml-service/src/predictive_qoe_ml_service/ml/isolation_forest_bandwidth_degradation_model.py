"""Isolation Forest wrapper for bandwidth-degradation anomaly detection.

Why Isolation Forest for this problem:
    Isolation Forest (Liu, Ting & Zhou, 2008) is an unsupervised anomaly
    detection algorithm that builds an ensemble of random binary trees,
    each of which recursively partitions the feature space by picking a
    random feature and a random split value. Anomalous points -- ones that
    are "few and different" -- tend to require far fewer random splits to
    isolate into their own leaf than normal points sitting inside a dense
    cluster of similar observations. The model's anomaly score is derived
    from the average path length to isolate a point across all trees in
    the forest: short average path length => anomalous, long average path
    length => normal.

    This fits the bandwidth-degradation use case well for several
    reasons:
      * We do not need (and in production do not reliably have) labeled
        "this session rebuffered" ground truth at training time -- healthy
        network behavior vastly dominates telemetry, and Isolation Forest
        trains purely on unlabeled feature vectors of "what normal jitter
        looks like" for a population of sessions.
      * It scales near-linearly with dataset size and needs no distance
        matrix, so it is cheap enough to retrain frequently and run
        inference well within the tight latency budget this early-warning
        system requires (see the module-level LATENCY note below).
      * It does not assume a particular data distribution (unlike, say, a
        Gaussian/Mahalanobis-distance outlier detector), which matters
        because jitter/RTT/throughput distributions are heavy-tailed and
        session-dependent.
      * Its output is naturally a continuous anomaly score, which we
        rescale into a bounded [0, 1] "degradation confidence score" for
        the API response rather than only a hard inlier/outlier label.

Latency target: this model is intentionally small (see
``DEFAULT_ISOLATION_FOREST_ESTIMATOR_COUNT``) so that a single
``predict_anomaly_scores_for_feature_vectors`` call comfortably completes
in low single-digit milliseconds on commodity hardware, preserving the
service's documented target of inference p99 < 20ms per prediction
request (see README "Latency Targets").
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from sklearn.ensemble import IsolationForest

#: Default number of trees in the Isolation Forest ensemble. Kept modest
#: (rather than e.g. sklearn's higher defaults for large tabular datasets)
#: because this model operates on a small, low-dimensional feature vector
#: per prediction request and must stay comfortably inside the service's
#: sub-20ms p99 inference latency budget.
DEFAULT_ISOLATION_FOREST_ESTIMATOR_COUNT = 100

#: Expected fraction of anomalous (degrading-network) samples in the
#: training population; passed to sklearn as the ``contamination``
#: parameter, which calibrates the internal decision threshold between
#: inlier and outlier scores.
DEFAULT_ISOLATION_FOREST_CONTAMINATION = 0.25

#: The canonical, order-significant list of feature names this model
#: expects. Any feature dict fed to this model must be flattened into a
#: numeric vector in exactly this order -- see
#: ``pipeline/bandwidth_degradation_prediction_pipeline.py``.
BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES: tuple[str, ...] = (
    "throughput_rolling_mean_kbps",
    "throughput_rolling_jitter_stddev_kbps",
    "throughput_rate_of_change_kbps",
    "throughput_ewma_kbps",
    "rtt_rolling_mean_ms",
    "rtt_rolling_jitter_stddev_ms",
    "rtt_rate_of_change_ms",
    "rtt_ewma_ms",
)


@dataclass(frozen=True)
class BandwidthDegradationAnomalyScoreResult:
    """The result of scoring one feature vector for bandwidth-degradation anomaly.

    Attributes:
        is_anomalous_prediction: True if the underlying Isolation Forest
            classified this feature vector as an outlier (predict() == -1).
        raw_anomaly_score: sklearn's raw ``decision_function`` output.
            Positive values indicate inliers, negative values indicate
            outliers; larger magnitude means more confidently classified.
        degradation_confidence_score: The raw anomaly score rescaled into
            a bounded [0, 1] range, where values near 1.0 indicate high
            confidence of an imminent bandwidth-degradation event and
            values near 0.0 indicate normal network behavior.
    """

    is_anomalous_prediction: bool
    raw_anomaly_score: float
    degradation_confidence_score: float


class IsolationForestBandwidthDegradationModel:
    """Wraps a trained ``sklearn.ensemble.IsolationForest`` for this domain.

    Encapsulates model construction, training, persistence, and inference
    with a domain-specific interface so callers (the prediction pipeline,
    the training harness, the tests) never touch raw sklearn objects or
    numpy arrays directly.
    """

    def __init__(
        self,
        n_estimators: int = DEFAULT_ISOLATION_FOREST_ESTIMATOR_COUNT,
        contamination: float = DEFAULT_ISOLATION_FOREST_CONTAMINATION,
        random_state: int = 42,
    ) -> None:
        """Construct an untrained model wrapper.

        Args:
            n_estimators: Number of isolation trees in the ensemble.
            contamination: Expected proportion of anomalous samples in the
                training population; calibrates sklearn's internal
                decision threshold.
            random_state: Seed for reproducible tree construction.
        """
        self._sklearn_isolation_forest = IsolationForest(
            n_estimators=n_estimators,
            contamination=contamination,
            random_state=random_state,
            n_jobs=-1,
        )
        self._is_trained = False
        # Populated after training so raw decision_function scores can be
        # rescaled to a stable [0, 1] confidence range at inference time.
        self._training_score_min: float = 0.0
        self._training_score_max: float = 1.0

    @property
    def is_trained(self) -> bool:
        """Whether ``train_on_feature_vectors`` has been called successfully."""
        return self._is_trained

    def train_on_feature_vectors(self, feature_vectors: np.ndarray) -> None:
        """Fit the Isolation Forest on an array of unlabeled feature vectors.

        Training is unsupervised: no labels are passed to sklearn, in
        keeping with real-world deployment where confirmed
        "this session subsequently rebuffered" ground truth is comparatively
        rare and lagged relative to raw telemetry.

        Args:
            feature_vectors: A 2D array of shape
                ``(n_samples, len(BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES))``.

        Raises:
            ValueError: If ``feature_vectors`` is empty or not 2D with the
                expected number of columns.
        """
        if feature_vectors.ndim != 2 or feature_vectors.shape[0] == 0:
            raise ValueError("feature_vectors must be a non-empty 2D array")
        if feature_vectors.shape[1] != len(BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES):
            raise ValueError(
                "feature_vectors must have "
                f"{len(BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES)} columns, "
                f"got {feature_vectors.shape[1]}"
            )

        self._sklearn_isolation_forest.fit(feature_vectors)
        self._is_trained = True

        # Calibrate the confidence rescaling range from the training
        # population's own decision_function distribution.
        training_scores = self._sklearn_isolation_forest.decision_function(feature_vectors)
        self._training_score_min = float(np.min(training_scores))
        self._training_score_max = float(np.max(training_scores))

    def score_feature_vectors(
        self, feature_vectors: np.ndarray
    ) -> list[BandwidthDegradationAnomalyScoreResult]:
        """Score a batch of feature vectors for bandwidth-degradation anomaly.

        Args:
            feature_vectors: A 2D array of shape
                ``(n_samples, len(BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES))``.

        Returns:
            One ``BandwidthDegradationAnomalyScoreResult`` per input row,
            in the same order.

        Raises:
            RuntimeError: If the model has not yet been trained.
        """
        if not self._is_trained:
            raise RuntimeError(
                "IsolationForestBandwidthDegradationModel must be trained "
                "(or loaded) before scoring"
            )

        predictions = self._sklearn_isolation_forest.predict(feature_vectors)
        raw_scores = self._sklearn_isolation_forest.decision_function(feature_vectors)

        results: list[BandwidthDegradationAnomalyScoreResult] = []
        for prediction, raw_score in zip(predictions, raw_scores):
            results.append(
                BandwidthDegradationAnomalyScoreResult(
                    is_anomalous_prediction=bool(prediction == -1),
                    raw_anomaly_score=float(raw_score),
                    degradation_confidence_score=self._rescale_raw_score_to_confidence(
                        float(raw_score)
                    ),
                )
            )
        return results

    def _rescale_raw_score_to_confidence(self, raw_score: float) -> float:
        """Rescale a raw decision_function score to a [0, 1] degradation confidence.

        sklearn's ``decision_function`` returns larger positive values for
        more confidently normal points and more negative values for more
        confidently anomalous points. We invert and min-max normalize
        against the training population's observed score range so that
        1.0 means "as anomalous as the most anomalous training example"
        and 0.0 means "as normal as the most normal training example",
        clamping outside that observed range.

        Args:
            raw_score: A single raw ``decision_function`` output.

        Returns:
            A float in [0.0, 1.0].
        """
        score_range = self._training_score_max - self._training_score_min
        if score_range <= 0:
            return 0.0 if raw_score >= 0 else 1.0

        normalized_normality = (raw_score - self._training_score_min) / score_range
        normalized_normality = min(1.0, max(0.0, normalized_normality))
        return 1.0 - normalized_normality

    def save_trained_model_to_disk(self, file_path: str) -> None:
        """Persist the trained sklearn estimator (and calibration range) to disk.

        Uses ``joblib`` (sklearn's recommended serialization mechanism for
        estimators containing numpy arrays).

        Args:
            file_path: Destination path for the serialized model bundle.

        Raises:
            RuntimeError: If the model has not yet been trained.
        """
        if not self._is_trained:
            raise RuntimeError("Cannot save an untrained model")

        import joblib

        joblib.dump(
            {
                "sklearn_isolation_forest": self._sklearn_isolation_forest,
                "training_score_min": self._training_score_min,
                "training_score_max": self._training_score_max,
            },
            file_path,
        )

    @classmethod
    def load_trained_model_from_disk(
        cls, file_path: str
    ) -> "IsolationForestBandwidthDegradationModel":
        """Load a previously saved model bundle produced by ``save_trained_model_to_disk``.

        Args:
            file_path: Path to the serialized model bundle.

        Returns:
            A fully initialized, trained ``IsolationForestBandwidthDegradationModel``.
        """
        import joblib

        bundle = joblib.load(file_path)
        model = cls()
        model._sklearn_isolation_forest = bundle["sklearn_isolation_forest"]
        model._training_score_min = bundle["training_score_min"]
        model._training_score_max = bundle["training_score_max"]
        model._is_trained = True
        return model
