"""Synthetic network-session data generation for training and evaluation.

Real production ISP jitter/RTT/throughput-variance telemetry, and real
labeled "this session rebuffered 10 seconds later" events, are not
available inside this development sandbox. To still exercise a genuine,
fully-trained scikit-learn Isolation Forest and report real, reproducibly
computed precision/recall/F1 numbers, this module synthesizes two classes
of network sessions from documented, parameterized random-process models:

STABLE sessions ("negative" / normal class):
    Throughput and RTT are drawn from a stationary process: a roughly
    constant mean with small, homoscedastic Gaussian noise on every
    sample. This models a healthy, well-provisioned last-mile connection
    with ordinary measurement noise.

DEGRADING sessions ("positive" / anomaly class):
    For the first portion of the session, the same stationary process as
    the stable case is used. Partway through, a degradation onset point is
    chosen, after which:
      * mean throughput ramps down (simulating congestion / a
        bandwidth-degradation event building up),
      * sample-to-sample variance ("jitter") increases, and
      * RTT ramps up with increased variance,
    culminating in throughput low enough that, if the trend continued
    unaddressed, the client's playback buffer would run out. Samples
    generated during and after the onset point are labeled anomalous
    (label = -1, matching scikit-learn's IsolationForest convention);
    samples before onset (and all samples in stable sessions) are labeled
    normal (label = 1).

This labeling is only used for held-out evaluation (computing
precision/recall/F1 against the unsupervised model's predictions) and for
choosing a decision threshold -- it is never used as a training target,
since Isolation Forest is trained unsupervised on feature vectors alone,
consistent with real-world deployment where genuine "rebuffer occurred"
ground truth is comparatively rare and lagged.

The generation process is deterministic given a random seed, so training
runs are reproducible.
"""

from __future__ import annotations

import random
from dataclasses import dataclass

from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    NetworkQualitySample,
)

#: Label used for normal / stable-network samples (matches sklearn's
#: IsolationForest.predict() convention for inliers).
NORMAL_SAMPLE_LABEL = 1

#: Label used for anomalous / degrading-network samples (matches sklearn's
#: IsolationForest.predict() convention for outliers).
ANOMALOUS_SAMPLE_LABEL = -1

_SAMPLE_INTERVAL_MS = 1000
_STABLE_MEAN_THROUGHPUT_KBPS = 6000.0
_STABLE_THROUGHPUT_STDDEV_KBPS = 150.0
_STABLE_MEAN_RTT_MS = 35.0
_STABLE_RTT_STDDEV_MS = 3.0


@dataclass(frozen=True)
class LabeledNetworkSession:
    """One synthetic session: its raw samples plus a per-sample ground-truth label.

    Attributes:
        samples: The session's simulated network-quality samples, in
            ascending timestamp order.
        per_sample_labels: Ground-truth labels aligned 1:1 with
            ``samples``; each is either ``NORMAL_SAMPLE_LABEL`` or
            ``ANOMALOUS_SAMPLE_LABEL``.
        is_degrading_session: Whether this session belongs to the
            "degrading network before rebuffer" simulated class (True) or
            the "stable network" class (False).
    """

    samples: list[NetworkQualitySample]
    per_sample_labels: list[int]
    is_degrading_session: bool


def generate_stable_network_session(
    random_generator: random.Random,
    session_length_samples: int = 60,
    start_timestamp_ms: int = 0,
) -> LabeledNetworkSession:
    """Simulate one "stable network" session (the normal class).

    Throughput and RTT are drawn i.i.d. per sample from Gaussian
    distributions around a fixed healthy mean, modeling ordinary
    measurement noise on a well-provisioned connection.

    Args:
        random_generator: A seeded ``random.Random`` instance, so callers
            control reproducibility explicitly rather than relying on
            module-global random state.
        session_length_samples: Number of 1-second samples to simulate.
        start_timestamp_ms: Epoch milliseconds of the first sample.

    Returns:
        A ``LabeledNetworkSession`` with every sample labeled
        ``NORMAL_SAMPLE_LABEL``.
    """
    samples: list[NetworkQualitySample] = []
    for index in range(session_length_samples):
        throughput = max(
            0.0,
            random_generator.gauss(
                _STABLE_MEAN_THROUGHPUT_KBPS, _STABLE_THROUGHPUT_STDDEV_KBPS
            ),
        )
        rtt = max(
            1.0, random_generator.gauss(_STABLE_MEAN_RTT_MS, _STABLE_RTT_STDDEV_MS)
        )
        samples.append(
            NetworkQualitySample(
                timestamp_ms=start_timestamp_ms + index * _SAMPLE_INTERVAL_MS,
                measured_throughput_kbps=throughput,
                round_trip_time_ms=rtt,
            )
        )
    labels = [NORMAL_SAMPLE_LABEL] * session_length_samples
    return LabeledNetworkSession(
        samples=samples, per_sample_labels=labels, is_degrading_session=False
    )


def generate_degrading_network_session_before_rebuffer(
    random_generator: random.Random,
    session_length_samples: int = 60,
    start_timestamp_ms: int = 0,
) -> LabeledNetworkSession:
    """Simulate one "degrading network before rebuffer" session (the anomaly class).

    The session starts identically to a stable session. At a randomly
    chosen onset sample (uniformly between 40% and 70% of the way through
    the session), throughput begins ramping linearly downward while its
    per-sample noise (jitter) grows, and RTT begins ramping upward with
    growing noise -- simulating congestion building toward a
    bandwidth-degradation event severe enough to eventually empty the
    client's playback buffer.

    Args:
        random_generator: A seeded ``random.Random`` instance.
        session_length_samples: Number of 1-second samples to simulate.
        start_timestamp_ms: Epoch milliseconds of the first sample.

    Returns:
        A ``LabeledNetworkSession`` where samples before the onset point
        are labeled ``NORMAL_SAMPLE_LABEL`` and samples at/after the onset
        point are labeled ``ANOMALOUS_SAMPLE_LABEL``.
    """
    onset_index = random_generator.randint(
        int(session_length_samples * 0.4), int(session_length_samples * 0.7)
    )
    samples: list[NetworkQualitySample] = []
    labels: list[int] = []

    for index in range(session_length_samples):
        if index < onset_index:
            throughput = max(
                0.0,
                random_generator.gauss(
                    _STABLE_MEAN_THROUGHPUT_KBPS, _STABLE_THROUGHPUT_STDDEV_KBPS
                ),
            )
            rtt = max(
                1.0,
                random_generator.gauss(_STABLE_MEAN_RTT_MS, _STABLE_RTT_STDDEV_MS),
            )
            labels.append(NORMAL_SAMPLE_LABEL)
        else:
            samples_since_onset = index - onset_index
            degradation_progress = min(
                1.0, samples_since_onset / max(1, (session_length_samples - onset_index))
            )
            # Throughput ramps down toward ~10% of its healthy mean, with
            # growing jitter (noise stddev scales up as degradation deepens).
            degraded_mean_throughput = _STABLE_MEAN_THROUGHPUT_KBPS * (
                1.0 - 0.9 * degradation_progress
            )
            degraded_throughput_stddev = _STABLE_THROUGHPUT_STDDEV_KBPS * (
                1.0 + 6.0 * degradation_progress
            )
            throughput = max(
                0.0,
                random_generator.gauss(
                    degraded_mean_throughput, degraded_throughput_stddev
                ),
            )
            # RTT ramps up toward ~6x its healthy mean, with growing jitter.
            degraded_mean_rtt = _STABLE_MEAN_RTT_MS * (1.0 + 5.0 * degradation_progress)
            degraded_rtt_stddev = _STABLE_RTT_STDDEV_MS * (1.0 + 8.0 * degradation_progress)
            rtt = max(
                1.0, random_generator.gauss(degraded_mean_rtt, degraded_rtt_stddev)
            )
            labels.append(ANOMALOUS_SAMPLE_LABEL)

        samples.append(
            NetworkQualitySample(
                timestamp_ms=start_timestamp_ms + index * _SAMPLE_INTERVAL_MS,
                measured_throughput_kbps=throughput,
                round_trip_time_ms=rtt,
            )
        )

    return LabeledNetworkSession(
        samples=samples, per_sample_labels=labels, is_degrading_session=True
    )


def generate_synthetic_labeled_session_dataset(
    random_seed: int,
    stable_session_count: int = 150,
    degrading_session_count: int = 150,
    session_length_samples: int = 60,
) -> list[LabeledNetworkSession]:
    """Generate a full synthetic dataset of labeled stable/degrading sessions.

    Args:
        random_seed: Seed for the internal ``random.Random`` instance,
            making the generated dataset fully reproducible.
        stable_session_count: Number of stable-network sessions to
            generate.
        degrading_session_count: Number of degrading-network sessions to
            generate.
        session_length_samples: Number of 1-second samples per session.

    Returns:
        A shuffled list of ``LabeledNetworkSession``, mixing both classes.
    """
    random_generator = random.Random(random_seed)
    sessions: list[LabeledNetworkSession] = []

    for session_index in range(stable_session_count):
        sessions.append(
            generate_stable_network_session(
                random_generator,
                session_length_samples=session_length_samples,
                start_timestamp_ms=session_index * session_length_samples * _SAMPLE_INTERVAL_MS,
            )
        )

    for session_index in range(degrading_session_count):
        sessions.append(
            generate_degrading_network_session_before_rebuffer(
                random_generator,
                session_length_samples=session_length_samples,
                start_timestamp_ms=session_index * session_length_samples * _SAMPLE_INTERVAL_MS,
            )
        )

    random_generator.shuffle(sessions)
    return sessions
