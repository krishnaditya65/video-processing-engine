# Predictive QoE ML Service

A microservice for a hyperscale video-streaming platform that predicts an
**imminent client-side bandwidth-degradation event** (the kind that leads
to a rebuffer) 5-15 seconds before it would otherwise empty the client's
playback buffer, using an unsupervised **Isolation Forest** anomaly
detector trained on rolling-window statistics of ISP-side network jitter,
round-trip time (RTT), and throughput variance. On a positive prediction,
the service proactively pushes a structured "downshift bitrate" command
over a WebSocket to the connected client player, so the ABR (adaptive
bitrate) logic can drop quality *before* the buffer runs dry rather than
reactively *after* a stall has already begun.

## Table of contents

- [Architecture](#architecture)
- [Why Isolation Forest](#why-isolation-forest)
- [Feature engineering](#feature-engineering)
- [Synthetic data generation methodology](#synthetic-data-generation-methodology)
- [Measured model metrics](#measured-model-metrics-synthetic-data)
- [API](#api)
- [WebSocket downshift command protocol](#websocket-downshift-command-protocol)
- [Design patterns used](#design-patterns-used)
- [Naming convention](#naming-convention)
- [Latency targets](#latency-targets)
- [Running the service](#running-the-service)
- [Running tests](#running-tests)
- [Limitations](#limitations)

## Architecture

```
services/predictive-qoe-ml-service/
  src/predictive_qoe_ml_service/
    feature_engineering/   Strategy-pattern rolling-window feature extraction (pure functions)
    ml/                     Isolation Forest wrapper, synthetic data generator, training harness
    pipeline/               Pipeline pattern: samples -> features -> model -> decision
    websocket/               In-memory session-socket Registry + downshift-command emitter
    api/                     FastAPI app: HTTP prediction endpoint + WebSocket route
    config.py                Service-wide paths/tunables
  scripts/run_training_harness.py   CLI: train, evaluate, persist the model
  tests/                     pytest suite
  models/                    Persisted trained model artifact (joblib)
```

Request flow for `POST /predict/bandwidth-degradation`:

```
raw NetworkQualitySample[]
   -> CompositeFeatureExtractionStrategy.extract_features_from_samples()  (feature_engineering/)
   -> vectorize to fixed-order numpy row                                  (pipeline/)
   -> IsolationForestBandwidthDegradationModel.score_feature_vectors()    (ml/)
   -> BandwidthDegradationPredictionDecision                              (pipeline/)
   -> [if imminent] BandwidthDegradationDownshiftCommandEmitter pushes a
      DOWNSHIFT_BITRATE_COMMAND WebSocket message to the session's socket (websocket/)
   -> JSON HTTP response
```

## Why Isolation Forest

[Isolation Forest](https://en.wikipedia.org/wiki/Isolation_forest) (Liu,
Ting & Zhou, 2008) builds an ensemble of random binary trees, each of
which recursively partitions the feature space by repeatedly picking a
random feature and a random split value. Points that are "few and
different" from the bulk of the data -- anomalies -- tend to get isolated
into their own leaf after far fewer random splits than points sitting
inside a dense, typical cluster. The anomaly score is derived from the
average path length across all trees needed to isolate a point: short
average path length implies anomalous, long average path length implies
normal.

This fits imminent-degradation detection on jitter/RTT/throughput
particularly well:

- **No reliable labels at training time.** In production we rarely have
  clean, timely "this exact session rebuffered because of ISP congestion"
  ground truth at the volume needed for supervised learning, and healthy
  network behavior vastly dominates raw telemetry. Isolation Forest trains
  purely on unlabeled feature vectors describing "what normal network
  jitter looks like" for a population of sessions.
- **No distributional assumptions.** Throughput and RTT are heavy-tailed
  and session/ISP-dependent; a method like a Gaussian/Mahalanobis-distance
  outlier detector would be a poor fit. Isolation Forest makes no
  parametric assumption about the feature distribution.
- **Cheap to train and to score.** Near-linear training cost and no
  distance-matrix computation at inference time make it practical to
  retrain frequently and to run inference well inside this service's tight
  early-warning latency budget (see [Latency targets](#latency-targets)).
- **A naturally continuous anomaly score.** `decision_function` gives a
  continuous score we rescale into a bounded `[0, 1]`
  `degradationConfidenceScore` rather than only a binary label, which the
  downstream ABR client can use to modulate how aggressively it reacts.

See
[`ml/isolation_forest_bandwidth_degradation_model.py`](src/predictive_qoe_ml_service/ml/isolation_forest_bandwidth_degradation_model.py)
for the concrete `sklearn.ensemble.IsolationForest` wrapper.

## Feature engineering

Raw samples are triples: `(timestampMs, measuredThroughputKbps,
roundTripTimeMs)`. The default feature-extraction strategy (see
[`feature_engineering/rolling_window_feature_strategies.py`](src/predictive_qoe_ml_service/feature_engineering/rolling_window_feature_strategies.py))
computes, per sample, using a trailing (causal) window so it only ever
looks at past/current data:

| Feature | Meaning |
|---|---|
| `throughput_rolling_mean_kbps` | Trailing rolling mean of throughput |
| `throughput_rolling_jitter_stddev_kbps` | Trailing rolling population stddev of throughput ("jitter") |
| `throughput_rate_of_change_kbps` | Sample-over-sample first difference of throughput |
| `throughput_ewma_kbps` | Exponentially weighted moving average of throughput |
| `rtt_rolling_mean_ms` | Trailing rolling mean of RTT |
| `rtt_rolling_jitter_stddev_ms` | Trailing rolling population stddev of RTT |
| `rtt_rate_of_change_ms` | Sample-over-sample first difference of RTT |
| `rtt_ewma_ms` | Exponentially weighted moving average of RTT |

Every one of these is a **pure function** (`compute_rolling_mean`,
`compute_rolling_standard_deviation`, `compute_rate_of_change`,
`compute_exponentially_weighted_moving_average`,
`compute_rolling_throughput_jitter_features`) -- same input always gives
the same output, no I/O, no mutation -- which is what makes them trivial
to unit test against hand-computed values on tiny fixtures (see
`tests/test_feature_engineering.py`).

## Synthetic data generation methodology

Real production ISP telemetry and confirmed rebuffer-event labels are not
available in this development environment, so
[`ml/synthetic_network_session_data_generator.py`](src/predictive_qoe_ml_service/ml/synthetic_network_session_data_generator.py)
synthesizes two classes of 1-second-cadence sessions from a documented,
seeded random process:

- **Stable sessions** (normal class): throughput and RTT are drawn i.i.d.
  per sample from Gaussian distributions around a fixed healthy mean
  (`~6000 kbps` throughput, `~35 ms` RTT), modeling ordinary measurement
  noise on a well-provisioned connection. Every sample is labeled normal
  (`+1`, matching scikit-learn's `IsolationForest.predict()` inlier
  convention).
- **Degrading sessions** (anomaly class): identical to a stable session up
  to a randomly chosen onset point (uniformly between 40% and 70% of the
  way through the session). From onset onward, mean throughput ramps
  linearly down toward ~10% of its healthy value while its sample-to-sample
  variance grows, and RTT ramps up toward ~6x its healthy mean with growing
  variance -- simulating congestion building toward a degradation event
  severe enough that, left unaddressed, the client's buffer would run out.
  Samples from onset onward are labeled anomalous (`-1`); samples before
  onset are labeled normal.

**Ground-truth labels are used only for held-out evaluation** (computing
precision/recall/F1) and are never passed to `IsolationForest.fit()` --
training is genuinely unsupervised, consistent with how this model would
have to be trained in production.

## Measured model metrics (synthetic data)

Produced by an actual run of
[`scripts/run_training_harness.py`](scripts/run_training_harness.py)
(150 stable + 150 degrading synthetic sessions, 60 samples each, 75/25
stratified train/test split, `IsolationForest(n_estimators=100,
contamination=0.25, random_state=42)`):

```
Train samples: 13499
Test samples:  4501
------------------------------------------------------------------------------
Precision (anomalous class): 0.8614
Recall    (anomalous class): 0.9251
F1 score  (anomalous class): 0.8921
```

**These numbers are measured on synthetic data generated by
`synthetic_network_session_data_generator.py`, not on production
traffic.** They demonstrate that the pipeline, feature engineering, and
model genuinely separate the two simulated classes; they are not a claim
about real-world ISP degradation detection accuracy, which would need to
be re-measured against real labeled production telemetry before this
model is trusted for live traffic decisions. `tests/test_training_harness.py`
re-runs a smaller version of this same harness and asserts
precision/recall/F1 all exceed `0.5` as a regression guard.

## API

### `POST /predict/bandwidth-degradation`

Request:

```json
{
  "sessionId": "session-abc-123",
  "recentNetworkQualitySamples": [
    {"timestampMs": 1000, "measuredThroughputKbps": 5000.0, "roundTripTimeMs": 30.0},
    {"timestampMs": 2000, "measuredThroughputKbps": 4800.0, "roundTripTimeMs": 32.0}
  ],
  "currentPlayerBufferSeconds": 12.0,
  "currentEncodedBitrateKbps": 4000.0
}
```

Response:

```json
{
  "isDegradationImminent": true,
  "degradationConfidenceScore": 0.91,
  "recommendedDownshiftBitrateKbps": 1500.0,
  "secondsBeforeBufferEmptyEstimate": 8.5
}
```

`currentPlayerBufferSeconds` and `currentEncodedBitrateKbps` are optional;
without them `secondsBeforeBufferEmptyEstimate` is `null` and the
recommended bitrate falls back to a throughput-headroom heuristic. See
[`api/schemas.py`](src/predictive_qoe_ml_service/api/schemas.py) for the
full schema and
[`pipeline/bandwidth_degradation_prediction_pipeline.py`](src/predictive_qoe_ml_service/pipeline/bandwidth_degradation_prediction_pipeline.py)
for the documented buffer-drain and downshift-bitrate heuristics.

### `GET /health`

Trivial liveness probe: `{"status": "ok", "service": "predictive-qoe-ml-service"}`.

## WebSocket downshift command protocol

Client players connect to `WS /ws/session/{sessionId}` to register
themselves in the in-memory session-socket registry. When
`POST /predict/bandwidth-degradation` predicts imminent degradation for
that session, the server pushes a JSON text frame:

```json
{
  "messageType": "DOWNSHIFT_BITRATE_COMMAND",
  "sessionId": "session-abc-123",
  "recommendedBitrateKbps": 1500.0,
  "degradationConfidenceScore": 0.91,
  "secondsBeforeBufferEmptyEstimate": 8.5,
  "issuedAtEpochMs": 1699999999000
}
```

See
[`websocket/downshift_command_emitter.py`](src/predictive_qoe_ml_service/websocket/downshift_command_emitter.py)
for the message dataclass and emitter, and
[`websocket/session_socket_registry.py`](src/predictive_qoe_ml_service/websocket/session_socket_registry.py)
for the registry.

## Design patterns used

- **Strategy** (`feature_engineering/rolling_window_feature_strategies.py`):
  `RollingWindowFeatureExtractionStrategy` is an abstract interface with
  concrete implementations (`ThroughputJitterRollingStatisticsStrategy`,
  `ExponentialWeightedMovingAverageStrategy`, `RateOfChangeFeatureStrategy`)
  that can be swapped or composed (`CompositeFeatureExtractionStrategy`)
  without touching the model or pipeline.
- **Pipeline** (`pipeline/bandwidth_degradation_prediction_pipeline.py`):
  `BandwidthDegradationPredictionPipeline` chains feature extraction ->
  vectorization -> model inference -> decisioning as independently
  testable stages.
- **Repository/Registry** (`websocket/session_socket_registry.py`):
  `InMemorySessionSocketRegistry` is the sole owner of the
  `session_id -> socket` mapping; callers depend on its
  register/unregister/lookup interface, not on how sockets are stored,
  so the storage backend (e.g. a future Redis-backed registry for
  multi-node deployments) can change without touching calling code.

## Naming convention

This service is written in idiomatic Python, so **all module, class,
function, and variable names use `snake_case`** per PEP 8 -- including
deliberately long, descriptive names such as
`compute_rolling_throughput_jitter_features` and
`predict_imminent_bandwidth_degradation_from_network_samples` -- even
though the broader platform this microservice belongs to generally
follows a **camelCase** naming philosophy for its TypeScript/JavaScript
services. This is a language-idiom choice, not an inconsistency: at the
JSON API boundary (HTTP request/response bodies and WebSocket messages),
all field names are `camelCase` (e.g. `isDegradationImminent`,
`recommendedDownshiftBitrateKbps`) via Pydantic field aliases (see
`api/schemas.py`) and explicit dict construction (see
`websocket/downshift_command_emitter.py`), so the rest of the platform
interacts with this service exclusively through camelCase JSON while the
Python implementation underneath stays idiomatic.

## Latency targets

**Target: inference p99 < 20ms per prediction request**, so that a
10-second early-warning budget (predict, push the WebSocket command, and
have the client act on it, all comfortably before the buffer would empty)
is preserved.

Measured (in-process, warmed up, 500 iterations, on a single-request
20-sample session, on the development machine used to build this
service; excludes HTTP/network transport overhead):

```
mean=9.678ms  p50=8.758ms  p99=15.944ms  max=46.340ms
```

This is comfortably under the 20ms target for the feature-extraction +
model-inference + decisioning pipeline itself. The `n_estimators=100`
Isolation Forest and the O(window) pure-Python rolling-window feature
functions are deliberately kept small/cheap specifically to preserve this
budget; end-to-end request latency in a real deployment will additionally
include HTTP framework, network, and (if applicable) authentication
overhead on top of this measured pipeline latency.

## Running the service

```bash
cd services/predictive-qoe-ml-service
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

# Train + evaluate + persist a model (recommended before serving):
python scripts/run_training_harness.py

# Run the API (loads the persisted model, or trains a fallback on the fly
# if none is found -- see api/main.py):
uvicorn predictive_qoe_ml_service.api.main:app --reload --app-dir src
```

## Running tests

```bash
source .venv/bin/activate
pytest -v
```

All 19 tests pass in this environment (`pip install scikit-learn fastapi
numpy pytest websockets` succeeded without issue in the local venv used to
build this service -- no sandbox package-installation limitations were
hit). Test breakdown:

- `tests/test_feature_engineering.py` -- rolling mean/stddev/rate-of-change/EWMA
  correctness against hand-computed values on small fixtures.
- `tests/test_training_harness.py` -- synthetic data generator invariants
  (label ordering, reproducibility, throughput trend direction) plus an
  actual smaller training/evaluation run asserting precision/recall/F1 all
  exceed `0.5`.
- `tests/test_api_endpoint.py` -- `POST /predict/bandwidth-degradation` and
  `GET /health` via FastAPI's `TestClient`, with the prediction pipeline
  dependency overridden with a mock so these tests don't depend on a real
  trained model.

## Limitations

- **Synthetic training/evaluation data only.** The reported precision/
  recall/F1 describe how well the model separates this codebase's own
  simulated "stable" vs "degrading" processes, not real ISP behavior. The
  simulation's parameters (mean throughput, ramp shape, onset timing) are
  reasonable but essentially handpicked; real-world validation against
  labeled production telemetry is required before any production trust
  decision.
- **In-memory, single-process session-socket registry.** `websocket/session_socket_registry.py`
  holds the session-to-socket mapping in a plain process-local dict. In a
  horizontally scaled, multi-node deployment, a session's WebSocket
  connection and the process handling its `POST /predict/...` request may
  live on different nodes, so a production deployment would need to back
  this same Repository interface with a shared store (e.g. Redis) or route
  commands through a message bus keyed by session id.
  This is intentionally out of scope for this reference implementation.
  Every place that depends on the registry does so through its narrow
  interface (`register_session_socket` / `get_registered_socket_for_session`
  / `unregister_session_socket`), specifically so that swap is possible
  without touching calling code.
  It is documented again here as the single biggest gap between this
  reference implementation and a real hyperscale deployment.
  Load-balancer/session-affinity configuration is also out of scope here.
- **No model registry/versioning/rollback tooling.** The trained model is
  a single joblib file on local disk (`models/isolation_forest_bandwidth_degradation_model.joblib`).
  A production system would want a real model registry, canary/shadow
  evaluation before promotion, and rollback.
- **The buffer-empty-time and recommended-bitrate calculations are simple,
  documented heuristics** (see
  `pipeline/bandwidth_degradation_prediction_pipeline.py`), not a full ABR
  buffer-model simulation -- appropriate for an early-warning estimate, not
  a precise countdown.
- **No authentication/authorization, rate limiting, or TLS termination**
  is implemented at this layer; those are assumed to be handled by the
  platform's gateway/ingress in front of this service.
