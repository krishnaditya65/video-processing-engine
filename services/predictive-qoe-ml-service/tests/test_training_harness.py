"""Tests for the synthetic data generator and the training/evaluation harness.

These tests actually run the Isolation Forest training and held-out
evaluation (using a smaller synthetic dataset than the README's headline
numbers, for test speed) and assert the resulting metrics clear a sane
threshold -- catching, e.g., an accidental label-inversion or a broken
feature pipeline that would otherwise silently degrade the model.
"""

from __future__ import annotations

from predictive_qoe_ml_service.ml.synthetic_network_session_data_generator import (
    ANOMALOUS_SAMPLE_LABEL,
    NORMAL_SAMPLE_LABEL,
    generate_degrading_network_session_before_rebuffer,
    generate_stable_network_session,
    generate_synthetic_labeled_session_dataset,
)
from predictive_qoe_ml_service.ml.training_evaluation_harness import (
    run_isolation_forest_training_and_evaluation_harness,
)

import random


def test_generate_stable_network_session_labels_every_sample_as_normal():
    """A stable session should have zero anomalous samples by construction."""
    rng = random.Random(0)
    session = generate_stable_network_session(rng, session_length_samples=30)
    assert len(session.samples) == 30
    assert all(label == NORMAL_SAMPLE_LABEL for label in session.per_sample_labels)
    assert session.is_degrading_session is False


def test_generate_degrading_network_session_has_a_normal_prefix_and_anomalous_suffix():
    """A degrading session should start normal and transition to anomalous, never reversing."""
    rng = random.Random(0)
    session = generate_degrading_network_session_before_rebuffer(
        rng, session_length_samples=50
    )
    labels = session.per_sample_labels
    assert labels[0] == NORMAL_SAMPLE_LABEL
    assert labels[-1] == ANOMALOUS_SAMPLE_LABEL
    # Once it flips to anomalous it should never flip back to normal.
    first_anomalous_index = labels.index(ANOMALOUS_SAMPLE_LABEL)
    assert all(label == ANOMALOUS_SAMPLE_LABEL for label in labels[first_anomalous_index:])
    assert session.is_degrading_session is True


def test_degrading_session_throughput_trends_downward_after_onset():
    """Sanity-check the simulation itself: throughput should fall during degradation."""
    rng = random.Random(7)
    session = generate_degrading_network_session_before_rebuffer(
        rng, session_length_samples=60
    )
    onset_index = session.per_sample_labels.index(ANOMALOUS_SAMPLE_LABEL)
    pre_onset_avg = sum(
        s.measured_throughput_kbps for s in session.samples[:onset_index]
    ) / onset_index
    final_five_avg = sum(
        s.measured_throughput_kbps for s in session.samples[-5:]
    ) / 5
    assert final_five_avg < pre_onset_avg


def test_generate_synthetic_labeled_session_dataset_is_reproducible_given_same_seed():
    """Same seed should produce an identical dataset (determinism for reproducible training)."""
    dataset_a = generate_synthetic_labeled_session_dataset(
        random_seed=42, stable_session_count=5, degrading_session_count=5
    )
    dataset_b = generate_synthetic_labeled_session_dataset(
        random_seed=42, stable_session_count=5, degrading_session_count=5
    )
    throughput_a = [s.measured_throughput_kbps for session in dataset_a for s in session.samples]
    throughput_b = [s.measured_throughput_kbps for session in dataset_b for s in session.samples]
    assert throughput_a == throughput_b


def test_training_evaluation_harness_produces_reasonable_f1_on_synthetic_holdout():
    """The trained Isolation Forest should meaningfully separate the two synthetic classes.

    Uses a smaller dataset than the README's headline run for test speed,
    but still asserts real, computed metrics from sklearn clear a
    reasonable bar -- this is the guard against a silently broken
    training/feature pipeline.
    """
    report = run_isolation_forest_training_and_evaluation_harness(
        random_seed=123,
        stable_session_count=40,
        degrading_session_count=40,
        session_length_samples=40,
    )

    assert report.train_sample_count > 0
    assert report.test_sample_count > 0
    assert report.trained_model.is_trained is True

    assert report.precision > 0.5
    assert report.recall > 0.5
    assert report.f1_score_value > 0.5
