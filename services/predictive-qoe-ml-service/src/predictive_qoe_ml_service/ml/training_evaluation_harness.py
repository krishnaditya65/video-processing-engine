"""Training and evaluation harness for the bandwidth-degradation Isolation Forest.

This module:
  1. Generates a synthetic labeled dataset of stable and degrading network
     sessions (see ``synthetic_network_session_data_generator``).
  2. Runs each session's raw samples through the same feature-extraction
     strategy used in production (``build_default_feature_extraction_pipeline_strategy``)
     to build per-sample feature vectors.
  3. Splits the resulting (feature_vector, label) pairs into a train and a
     held-out test split.
  4. Trains ``IsolationForestBandwidthDegradationModel`` unsupervised (no
     labels passed to ``fit``) on the training split's feature vectors.
  5. Scores the held-out test split and computes real precision, recall,
     and F1 (via ``sklearn.metrics``) against the synthetic ground-truth
     labels, purely for evaluation/reporting -- never used in training.

Run directly as a script (``python -m predictive_qoe_ml_service.ml.training_evaluation_harness``
or via ``scripts/run_training_harness.py``) to reproduce the numbers
reported in the README.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from sklearn.metrics import f1_score, precision_score, recall_score

from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    build_default_feature_extraction_pipeline_strategy,
)
from predictive_qoe_ml_service.ml.isolation_forest_bandwidth_degradation_model import (
    BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES,
    IsolationForestBandwidthDegradationModel,
)
from predictive_qoe_ml_service.ml.synthetic_network_session_data_generator import (
    NORMAL_SAMPLE_LABEL,
    generate_synthetic_labeled_session_dataset,
)


@dataclass(frozen=True)
class TrainingEvaluationReport:
    """Held-out evaluation metrics from one training/evaluation harness run.

    Attributes:
        precision: sklearn-computed precision of the trained model's
            anomaly predictions against synthetic ground-truth labels on
            the held-out test split (positive class = anomalous/-1).
        recall: sklearn-computed recall, same setup.
        f1_score_value: sklearn-computed F1 score, same setup.
        train_sample_count: Number of feature vectors used for (unsupervised)
            training.
        test_sample_count: Number of feature vectors in the held-out test
            split used for evaluation.
        trained_model: The trained model instance, ready for inference or
            for persisting via ``save_trained_model_to_disk``.
    """

    precision: float
    recall: float
    f1_score_value: float
    train_sample_count: int
    test_sample_count: int
    trained_model: IsolationForestBandwidthDegradationModel


def _build_feature_vectors_and_labels_from_sessions(sessions) -> tuple[np.ndarray, np.ndarray]:
    """Flatten a list of ``LabeledNetworkSession`` into (X, y) arrays.

    Every sample of every session is turned into one row using the shared
    default feature-extraction strategy, in
    ``BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES`` column order, paired
    with that sample's ground-truth label.

    Args:
        sessions: A list of ``LabeledNetworkSession`` (see
            ``synthetic_network_session_data_generator``).

    Returns:
        A tuple ``(feature_matrix, labels)`` where ``feature_matrix`` has
        shape ``(total_samples, len(BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES))``
        and ``labels`` has shape ``(total_samples,)``.
    """
    feature_extraction_strategy = build_default_feature_extraction_pipeline_strategy()

    all_rows: list[list[float]] = []
    all_labels: list[int] = []

    for session in sessions:
        named_feature_series = feature_extraction_strategy.extract_features_from_samples(
            session.samples
        )
        sample_count = len(session.samples)
        for sample_index in range(sample_count):
            row = [
                named_feature_series[name][sample_index]
                for name in BANDWIDTH_DEGRADATION_MODEL_FEATURE_NAMES
            ]
            all_rows.append(row)
            all_labels.append(session.per_sample_labels[sample_index])

    return np.array(all_rows), np.array(all_labels)


def run_isolation_forest_training_and_evaluation_harness(
    random_seed: int = 1337,
    stable_session_count: int = 150,
    degrading_session_count: int = 150,
    session_length_samples: int = 60,
    test_split_fraction: float = 0.25,
) -> TrainingEvaluationReport:
    """Generate synthetic data, train the Isolation Forest, and evaluate it.

    Args:
        random_seed: Seed controlling both synthetic data generation and
            the train/test split shuffle, for reproducibility.
        stable_session_count: Number of synthetic stable sessions to
            generate.
        degrading_session_count: Number of synthetic degrading sessions to
            generate.
        session_length_samples: Samples per synthetic session.
        test_split_fraction: Fraction of the flattened sample set held out
            for evaluation (stratified by label so both classes appear in
            both splits).

    Returns:
        A ``TrainingEvaluationReport`` with real, computed precision/
        recall/F1 plus the trained model.
    """
    sessions = generate_synthetic_labeled_session_dataset(
        random_seed=random_seed,
        stable_session_count=stable_session_count,
        degrading_session_count=degrading_session_count,
        session_length_samples=session_length_samples,
    )
    feature_matrix, labels = _build_feature_vectors_and_labels_from_sessions(sessions)

    train_features, test_features, train_labels, test_labels = _stratified_train_test_split(
        feature_matrix, labels, test_split_fraction, random_seed
    )

    model = IsolationForestBandwidthDegradationModel()
    # Train unsupervised: labels are intentionally not passed to fit().
    model.train_on_feature_vectors(train_features)

    score_results = model.score_feature_vectors(test_features)
    predicted_labels = np.array(
        [-1 if result.is_anomalous_prediction else 1 for result in score_results]
    )

    precision = precision_score(
        test_labels, predicted_labels, pos_label=-1, zero_division=0
    )
    recall = recall_score(test_labels, predicted_labels, pos_label=-1, zero_division=0)
    f1 = f1_score(test_labels, predicted_labels, pos_label=-1, zero_division=0)

    return TrainingEvaluationReport(
        precision=float(precision),
        recall=float(recall),
        f1_score_value=float(f1),
        train_sample_count=int(train_features.shape[0]),
        test_sample_count=int(test_features.shape[0]),
        trained_model=model,
    )


def _stratified_train_test_split(
    feature_matrix: np.ndarray,
    labels: np.ndarray,
    test_split_fraction: float,
    random_seed: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    """Split (X, y) into train/test, preserving each class's proportion.

    A plain random split risks a test split with too few (or zero)
    anomalous examples given class imbalance; stratifying by label avoids
    that and keeps precision/recall/F1 computations meaningful.

    Args:
        feature_matrix: Full feature matrix.
        labels: Full label vector, aligned with ``feature_matrix`` rows.
        test_split_fraction: Fraction of each class held out for testing.
        random_seed: Seed for the shuffle, for reproducibility.

    Returns:
        ``(train_features, test_features, train_labels, test_labels)``.
    """
    rng = np.random.RandomState(random_seed)
    train_indices: list[int] = []
    test_indices: list[int] = []

    for class_label in np.unique(labels):
        class_indices = np.where(labels == class_label)[0]
        rng.shuffle(class_indices)
        split_point = int(len(class_indices) * (1.0 - test_split_fraction))
        train_indices.extend(class_indices[:split_point].tolist())
        test_indices.extend(class_indices[split_point:].tolist())

    rng.shuffle(train_indices)
    rng.shuffle(test_indices)

    return (
        feature_matrix[train_indices],
        feature_matrix[test_indices],
        labels[train_indices],
        labels[test_indices],
    )


def _print_training_evaluation_report(report: TrainingEvaluationReport) -> None:
    """Pretty-print a ``TrainingEvaluationReport`` to stdout.

    Args:
        report: The report to print.
    """
    print("Predictive QoE ML Service -- Isolation Forest training/evaluation report")
    print("=" * 78)
    print(f"Train samples: {report.train_sample_count}")
    print(f"Test samples:  {report.test_sample_count}")
    print("-" * 78)
    print(f"Precision (anomalous class): {report.precision:.4f}")
    print(f"Recall    (anomalous class): {report.recall:.4f}")
    print(f"F1 score  (anomalous class): {report.f1_score_value:.4f}")
    print("=" * 78)
    print(
        "NOTE: these metrics are measured on SYNTHETIC data generated by "
        "synthetic_network_session_data_generator.py, not production traffic."
    )


if __name__ == "__main__":
    generated_report = run_isolation_forest_training_and_evaluation_harness()
    _print_training_evaluation_report(generated_report)
