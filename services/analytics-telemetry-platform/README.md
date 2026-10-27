# Predictive Analytics & Telemetry Lake

A Node.js/TypeScript microservice that ingests high-volume QoE (Quality of
Experience) playback telemetry from a hyperscale video streaming platform,
persists it to ClickHouse for analytical querying, and provides real
business-logic layers on top: QoE metric computation, hardware decoder
anomaly detection, automated encoding A/B test evaluation (with live
rollback automation), real-time concurrent-viewer aggregation, and a
QoE/churn correlation analyzer, all wired together with OpenTelemetry-based
micro-session distributed tracing (now with a Redis-backed, multi-replica
lookup) and outbound webhook-based auto-remediation to sibling services.

**Target broker:** this service targets **Redpanda**, not Apache Kafka
directly. Redpanda is wire-protocol-compatible with the Kafka broker API,
so the `kafkajs` client library (a Kafka client) works against it
unmodified. Every reference to "Kafka" in code comments describes the
protocol/API being spoken, not the specific broker deployed in production.

---

## Architecture

```
                 ┌─────────────────────────────────────────────────────────┐
                 │              Client player SDKs (web/mobile/TV)          │
                 └───────────────────────────┬───────────────────────────────┘
                                              │ QoE beacons (JSON over HTTP -> topic)
                                              ▼
                          ┌───────────────────────────────────┐
                          │   Redpanda (Kafka-API compatible)   │
                          │      topic: qoe-playback-beacons    │
                          └───────────────────┬─────────────────┘
                                              │ eachBatch()
                                              ▼
   ┌──────────────────────────────────────────────────────────────────────────┐
   │                     PlaybackBeaconKafkaConsumer (kafkajs)                 │
   │                                                                            │
   │   Beacon Ingestion Pipeline (Pipeline pattern):                           │
   │   ┌────────────┐   ┌─────────────┐   ┌────────────┐   ┌────────────────┐ │
   │   │ Validate    │→ │ Normalize   │→ │ Enrich      │→ │ Batch-Insert    │ │
   │   │ (Zod)       │  │ (units/     │  │ (traceId -> │  │ (micro-batch    │ │
   │   │             │  │  defaults)  │  │  chunk      │  │  accumulator)   │ │
   │   └────────────┘   └─────────────┘  │  provenance)│  └────────┬───────┘ │
   │                                      └─────────────┘           │         │
   └──────────────────────────────────────────────────────────────┼─────────┘
                                                                    ▼
                          ┌──────────────────────────────────────────────────┐
                          │  PlaybackBeaconRepository (Repository pattern)    │
                          │        @clickhouse/client -> ClickHouse           │
                          └───────────────────┬────────────────────────────────┘
                                              ▼
                          ┌───────────────────────────────────┐
                          │  telemetry_lake.playback_beacons    │
                          │  MergeTree, partitioned/ordered for  │
                          │  time-series compression + query      │
                          │  performance (see below)              │
                          └───────────────────┬─────────────────┘
                                              │
                     ┌────────────────────────┼─────────────────────────┐
                     ▼                        ▼                         ▼
         ┌───────────────────┐   ┌─────────────────────────┐  ┌──────────────────────┐
         │  QoE Metrics        │   │  Firmware Cohort          │  │  Encoding A/B Test    │
         │  (pure functions)    │   │  Anomaly Detector          │  │  Evaluator             │
         │  src/metrics/        │   │  (z-score, Strategy        │  │  (Welch's t-test,      │
         │  qoeMetrics.ts        │   │  pattern) + Observer/      │  │  Strategy pattern)      │
         │                       │   │  EventEmitter               │  │                        │
         └───────────────────┘   └─────────────────────────┘  └──────────────────────┘
```

### Design Patterns Applied

| Pattern | Where | Why |
|---|---|---|
| **Repository** | `src/clickhouse/playbackBeaconRepository.ts` | Isolates all SQL/ClickHouse-shape knowledge behind a typed method surface (`insertEnrichedPlaybackBeaconBatch`, `queryRebufferingRatioForAssetOverTimeRange`, ...). Callers never write raw SQL. |
| **Strategy** | `src/anomaly/firmwareCohortAnomalyDetector.ts` (`AnomalyDetectionStrategy`, `ZScoreAnomalyDetectionStrategy`); `src/abtest/encodingExperimentEvaluator.ts` (`SignificanceTestStrategy`, `WelchTTestSignificanceTestStrategy`) | Both the anomaly-detection algorithm and the significance test are injected interfaces, so alternative algorithms (e.g. an EWMA anomaly detector, or a two-proportion z-test) can be substituted without touching the calling code. |
| **Observer / EventEmitter** | `src/anomaly/anomalyEventEmitter.ts` (`AnomalyEventEmitter`, `AnomalyDetectedEvent`); extended by `src/anomaly/manifestFilterRuleEmitter.ts` (`ManifestFilterRuleEmitter`) | Decouples anomaly *detection* from anomaly *response*. Alerting, auto-remediation (`ManifestFilterRuleEmitter`), and dashboard-sink consumers can each subscribe independently, without the detector knowing about any of them. |
| **Pipeline** | `src/ingestion/pipeline/*` (`beaconValidationStage.ts` → `beaconNormalizationStage.ts` → `beaconEnrichmentStage.ts` → `beaconBatchInsertStage.ts`, composed in `beaconIngestionPipeline.ts`) | Each stage has a single responsibility and a clean input/output contract; the first two stages are pure and independently unit-tested without any I/O. |
| **Repository** | `src/tracing/chunkProvenanceLookup.ts` / `src/tracing/redisBackedChunkProvenanceLookup.ts` (`ChunkProvenanceLookup` interface; `InMemoryChunkProvenanceLookup` and `RedisBackedChunkProvenanceLookup` implementations) | Two interchangeable concrete repositories behind one interface: in-memory for local dev/tests, Redis-backed for multi-replica production, with no change to calling code. |

---

## ClickHouse Schema Design Rationale

Full DDL: [`sql/schema.sql`](./sql/schema.sql). Summary of the key decisions
(also documented inline in the SQL file):

1. **`ENGINE = MergeTree`** — beacons are immutable, write-once facts; no
   upsert/collapse semantics are needed, so the plain `MergeTree` engine
   (rather than `ReplacingMergeTree`/`CollapsingMergeTree`) is the correct,
   simplest choice.

2. **`PARTITION BY toYYYYMMDD(eventTimestamp)`** — daily partitions keep
   per-partition part counts manageable at hyperscale volume, let
   ClickHouse prune entire partitions instantly for "last N days" dashboard
   queries (the dominant access pattern), and let TTL-based retention drop
   whole partitions cheaply instead of deleting individual rows.

3. **`ORDER BY (assetId, toStartOfHour(eventTimestamp), eventType, beaconId)`**
   — the single most consequential decision in the schema:
   - `assetId` leads because the dominant query pattern is "QoE metrics for
     asset X over time range Y" (per-title dashboards, launch war-rooms) —
     this lets ClickHouse's sparse primary index skip all granules for
     every other asset entirely.
   - `toStartOfHour(eventTimestamp)` as the second key column means, within
     each `(assetId, hour)` bucket, `eventTimestamp` values are
     monotonically increasing runs. This is the classic
     **delta-of-delta / delta-encoding sweet spot**: consecutive sorted
     timestamp values differ by small, similar deltas, so the `DoubleDelta`
     codec (used on `eventTimestamp` in the DDL) plus `ZSTD` compresses that
     column 10-20x smaller than an unsorted equivalent — and the same
     benefit compounds on other monotonically-drifting numeric columns
     (`playbackPositionSeconds`) using the `Delta` codec.
   - `eventType` and `beaconId` trail as tiebreakers, keeping same-kind
     events physically grouped without materially hurting the leading
     columns' compression.

4. **Explicit `CODEC(Delta/DoubleDelta, ZSTD(...))`** on hot numeric/time
   columns — this is the standard ClickHouse time-series compression
   recipe, and it is precisely what the `ORDER BY` key above is chosen to
   make effective (delta encoding only pays off when consecutive on-disk
   values are already numerically close, which the sort order guarantees).

5. **`LowCardinality(String)`** on bounded-cardinality dimension columns
   (`eventType`, `deviceOperatingSystem`, `hardwareDecoderModel`,
   `internetServiceProviderName`, `deviceFirmwareVersion`) — ClickHouse
   dictionary-encodes these, shrinking storage and speeding up the
   `GROUP BY`/filter operations the anomaly detector and A/B evaluator rely
   on.

6. **`TTL ... + INTERVAL 90 DAY DELETE`** — raw beacon rows are retained 90
   days at full grain; longer-range trends are expected to be served from
   pre-aggregated rollup materialized views (a sketch is included,
   commented, at the bottom of `schema.sql`) rather than scanning raw rows
   indefinitely.

---

## QoE Metric Formulas

Implemented as pure functions in `src/metrics/qoeMetrics.ts`:

- **Rebuffering Ratio** = `totalRebufferDurationMillis / (totalRebufferDurationMillis + totalWatchTimeMillis)`,
  where watch time is derived from positive deltas between consecutive
  per-session heartbeat `playbackPositionSeconds` values (robust to paused
  sessions and out-of-order delivery).
- **Time-To-First-Frame percentiles (p50/p95/p99)** — nearest-rank method:
  `rank = ceil(p/100 * N)`, value = the N-th smallest observed sample. Used
  instead of linear interpolation so percentiles always report an actually
  observed sample.
- **Average Bitrate Played** — arithmetic mean of all reported
  `bitrateKbps` samples (documented as *not* time-weighted; time-weighting
  is a noted possible extension).
- **Completion Percentage** = `min(maxPlaybackPositionSeconds / assetDurationSeconds, 1) * 100`.

## Hardware Decoder Anomaly Detection

`src/anomaly/anomalyStatistics.ts` implements mean, **sample** standard
deviation (Bessel's-corrected, N-1 denominator — the correct estimator when
the input is a sample from a rolling baseline window rather than a full
population), and z-score:

```
z = (observedValue - baselineMean) / baselineStandardDeviation
```

`src/anomaly/firmwareCohortAnomalyDetector.ts` computes a firmware cohort's
current-window decode-error rate (`decodeErrorCount / distinctSessionCount`),
z-scores it against a rolling baseline of prior windows, and — when
`|z| >= 3.0` (the conventional three-sigma "this is not noise" threshold
used broadly in statistical process control) — emits an
`AnomalyDetectedEvent` through `AnomalyEventEmitter` (Observer pattern).

## Automated Encoding A/B Test Evaluator

`src/abtest/significanceTests.ts` implements **Welch's t-test** (unequal
variance two-sample t-test), chosen over Student's pooled t-test or a
two-proportion z-test because per-session rebuffering ratio is a
continuous, bounded metric whose variance is not assumed equal between
control and treatment arms:

```
t  = (mean_treatment - mean_control) / sqrt(variance_control/n_control + variance_treatment/n_treatment)

df = (variance_control/n_control + variance_treatment/n_treatment)^2
     ─────────────────────────────────────────────────────────────────
     (variance_control/n_control)^2/(n_control-1) + (variance_treatment/n_treatment)^2/(n_treatment-1)
```

The two-tailed p-value is computed from `t` and `df` via the regularized
incomplete beta function (`P(|T| > |t|) = I_x(df/2, 1/2)`, `x = df/(df+t²)`),
evaluated with a continued-fraction expansion — there is no closed form for
the Student's t CDF, so this is the standard numerical technique (see
doc comments in `significanceTests.ts` for the full derivation and
references).

`src/abtest/encodingExperimentEvaluator.ts` applies a documented decision
policy at `alpha = 0.05` (95% confidence, configurable):
- `p >= alpha` → **INCONCLUSIVE** (keep collecting data)
- `p < alpha` and treatment rebuffering ratio lower → **PROMOTE**
- `p < alpha` and treatment rebuffering ratio higher → **ROLLBACK**

## Live A/B Rollback Automation

`src/abtest/encodingExperimentAutoRemediationService.ts` closes the loop on
the encoding A/B evaluator's decision: `EncodingExperimentAutoRemediationService`
takes an `EncodingExperimentEvaluationResult` and, only for a `ROLLBACK`
verdict, calls the control-plane-api-gateway's remediation webhook (see
"Outbound Webhook Contracts" below) with retry-with-exponential-backoff so a
statistically significant regression is reverted automatically, within
seconds, without a human in the loop. `PROMOTE`/`INCONCLUSIVE` verdicts are
a no-op. Webhook delivery failures are reported in a returned
`WebhookDeliveryResult`, never thrown -- a flaky/unreachable control plane
can never crash the caller's evaluation loop.

## Anomaly Auto-Remediation

`src/anomaly/manifestFilterRuleEmitter.ts` extends the Observer chain
documented above: `ManifestFilterRuleEmitter` subscribes to
`AnomalyEventEmitter`'s `anomalyDetected` channel (the same channel
`FirmwareCohortAnomalyDetector` already publishes to) and, for every
confirmed anomaly, constructs a structured `ManifestFilterRule` -- e.g. "LG
TV firmware `LG-webOS-06.40.11` cannot reliably decode HEVC; downgrade its
manifest to H.264" -- and POSTs it to the edge-packaging service's
manifest-filter-rules webhook (see "Outbound Webhook Contracts" below).
Rule construction (`buildManifestFilterRuleFromAnomalyEvent`) is a pure
function, independently unit-tested from the webhook-delivery side effect.
Today's anomaly-to-action mapping is fixed (`downgradeToH264` for any
confirmed firmware-cohort decode-error anomaly); a pluggable
anomaly-to-action strategy is a documented possible extension (see
"Limitations") if other anomaly types eventually need a different
remediation.

## Real-Time Concurrent-Viewer (CCU) Aggregation

`src/metrics/concurrentViewerAggregator.ts`'s `ConcurrentViewerAggregator`
maintains an in-memory sliding-window count of active playback sessions
from beacon heartbeats (`recordSessionHeartbeat(sessionId, assetId,
timestamp)`), exposing `getCurrentConcurrentViewerCount()` and
`getConcurrentViewerCountByAsset(assetId)`.

A session counts as "concurrent" iff its most recent recorded heartbeat is
within `activeSessionWindowMillis` (default: 45,000ms) of now -- the
standard sliding-window CCU definition, which does not require an explicit
"session ended" beacon (unreliable in practice: players crash or lose
connectivity without a clean teardown).

**Memory-leak avoidance:** only the most recent heartbeat per `sessionId`
is retained (a `Map`, overwritten on re-heartbeat, never grows per extra
heartbeat), and `evictExpiredSessions()` removes any session whose most
recent heartbeat has aged out of the window. Eviction runs on every read
and write, plus optionally on a periodic timer via `startPeriodicEviction()`
(started in `src/index.ts`, stopped on shutdown) to bound memory during
idle periods with no reads or writes at all. This keeps memory bounded to
O(currently-or-recently-active sessions), never O(total heartbeats ever
received).

**Documented limitation:** this is a single-process, in-memory aggregator
(mirroring `InMemoryChunkProvenanceLookup`) -- a multi-replica deployment
would need either sticky per-session routing or a shared store (e.g. Redis
per-session TTL keys) to produce a cluster-wide CCU figure; see
"Limitations" below.

## Cohort LTV/Churn Correlation

`src/analytics/qoeChurnCorrelationAnalyzer.ts`'s
`computeRebufferingChurnPearsonCorrelation` computes the Pearson
product-moment correlation coefficient between per-user average
rebuffering ratio and a 30-day churn outcome, over a synthetic joined
dataset of `{ userId, averageRebufferingRatio, churnedWithin30Days }`
(the QoE-beacon-to-subscription join itself is assumed to happen upstream,
e.g. a scheduled warehouse job; this module is the pure statistics layer on
top of the joined result set).

Formula:

```
r = Σ((x_i - x̄)(y_i - ȳ)) / sqrt(Σ(x_i - x̄)² * Σ(y_i - ȳ)²)
```

where `x` is `averageRebufferingRatio` and `y` is `churnedWithin30Days`
coded as `1`/`0` -- applying Pearson's r to a binary-coded variable is
mathematically well-defined and known as the **point-biserial correlation
coefficient**; the arithmetic is identical to standard Pearson's r.

`tests/qoeChurnCorrelationAnalyzer.test.ts` includes a fully hand-verified
5-user fixture (arithmetic shown step-by-step in the test's comments):
`x = [0.01, 0.02, 0.05, 0.10, 0.20]`, `y = [0, 0, 0, 1, 1]` &rarr; `r ≈
0.869926`, i.e. a strong positive correlation between rebuffering and
churn, quantifying the retention case for QoE investment.

## Outbound Webhook Contracts

Both auto-remediation services above call an HTTP webhook on a sibling
service via the real global `fetch` (Node >= 18; see `package.json`
"engines"), wrapped with retry-with-exponential-backoff
(`src/webhooks/webhookRetryHttpClient.ts`, shared by both call sites):
3 attempts by default, starting at a 200ms delay and doubling per retry
(200ms, then 400ms), configurable per caller. Never throws: failed
delivery after exhausting all attempts is reported via a
`WebhookDeliveryResult`, not raised.

These endpoints are owned by, and being implemented by, sibling
teams/agents (control-plane-api-gateway and edge-packaging respectively);
this document is the source-of-truth contract both sides should match.

### `POST {CONTROL_PLANE_BASE_URL}/internal/remediation/rollback-experiment`

Called by `EncodingExperimentAutoRemediationService` whenever
`evaluateEncodingExperimentStatisticalSignificance` produces a `ROLLBACK`
verdict.

```json
{
  "experimentId": "string",
  "decision": "ROLLBACK",
  "controlArmMeanRebufferingRatio": 0.01,
  "treatmentArmMeanRebufferingRatio": 0.05,
  "pValue": 0.0001,
  "significanceLevelAlpha": 0.05,
  "rationale": "string, human-readable",
  "detectedAtEpochMillis": 1700000000000
}
```

### `POST {EDGE_PACKAGING_BASE_URL}/internal/manifest-filters/rules`

Called by `ManifestFilterRuleEmitter` for every confirmed
`AnomalyDetectedEvent`.

```json
{
  "deviceCohortMatcher": { "firmwareVersion": "string" },
  "action": "downgradeToH264",
  "reason": "string, human-readable",
  "sourceAnomaly": {
    "firmwareCohortId": "string",
    "zScore": 82.0,
    "observedErrorRate": 0.42,
    "baselineMeanErrorRate": 0.01
  },
  "createdAtEpochMillis": 1700000000000
}
```

Both request bodies are exported TypeScript types
(`EncodingExperimentRollbackWebhookRequestBody`, `ManifestFilterRule`) for
receiving-side teams to import/mirror directly.

## Micro-Session Distributed Tracing

`src/tracing/otelSetup.ts` wires the real `@opentelemetry/sdk-node` +
`@opentelemetry/api`. `src/tracing/chunkProvenanceLookup.ts` documents the
(simplified) linkage model: encoding workers publish a
`ChunkProvenanceRecord` (worker id, chunk id, encode timestamp) keyed by
their job's OpenTelemetry `traceId`; the player SDK echoes that same
`traceId` onto every beacon it emits while playing the resulting chunk (via
W3C Trace Context propagation at chunk-request time). The enrichment
pipeline stage resolves `traceId → encodingWorkerId` via the
`ChunkProvenanceLookup` interface — an in-memory reference implementation
is provided; production should back it with Redis or an equivalent
low-latency KV store with a TTL matching CDN edge cache TTL.

**This is now implemented**: `src/tracing/redisBackedChunkProvenanceLookup.ts`
provides `RedisBackedChunkProvenanceLookup`, a Repository-pattern
`ChunkProvenanceLookup` implementation storing each record as a JSON string
under `provenance:{traceId}` with a TTL (default 4 hours, matching typical
CDN edge cache TTLs), so any replica in the consumer fleet can resolve a
lookup, not just the replica that registered it. It depends on a minimal,
locally defined `ChunkProvenanceRedisClientLike` interface (`set`/`get`/
`del`) rather than the concrete `ioredis` client, following the same
Dependency Inversion pattern as the control-plane-api-gateway service's
`RedisClientLike` (`services/control-plane-api-gateway/src/redis/redisClientLike.interface.ts`)
— tests inject a trivial in-memory fake client; the real `ioredis` `Redis`
client structurally satisfies the interface as-is. `src/index.ts` wires it
in automatically when `REDIS_URL` is set, falling back to
`InMemoryChunkProvenanceLookup` otherwise.

---

## Ingestion Throughput Targets

**Target: sustain up to 2,000,000 beacons/sec ingestion** across the
consumer fleet via batched micro-batch ClickHouse writes.

- `BeaconMicroBatchAccumulator` (`src/ingestion/pipeline/beaconBatchInsertStage.ts`)
  buffers enriched beacons and flushes on a **dual trigger**: whichever
  comes first of (a) `maxBatchSizeRows` buffered rows (default: 50,000), or
  (b) `maxBatchLingerMillis` elapsed since the oldest buffered beacon
  (default: 500ms).
- **Documented tradeoff:** ClickHouse insert efficiency scales strongly
  with batch size — few, large inserts dramatically outperform many, small
  ones (fewer parts created, less merge pressure, lower per-insert HTTP/TCP
  overhead). Larger `maxBatchSizeRows`/`maxBatchLingerMillis` improves
  cluster efficiency and lowers cost per ingested row, at the cost of
  higher end-to-end latency between "beacon produced" and "beacon queryable
  in ClickHouse." The 50,000-row / 500ms defaults target sub-second
  queryability while keeping batch sizes in ClickHouse's efficient range;
  tune upward for cost efficiency, downward for near-real-time freshness.
  ClickHouse's `async_insert` setting (enabled in
  `clickhouseClient.ts`) provides a second layer of server-side batching as
  defense-in-depth against undersized client batches.
- At full 2M/sec peak with 50,000-row batches, this implies roughly 40
  concurrent insert streams/sec in aggregate across the consumer fleet —
  achieved via horizontal scaling of consumer instances (`kafkajs`
  `partitionsConsumedConcurrently`, and topic partition count sized
  accordingly), not a single hot insert path.

## Query Latency Targets (p50 / p95 / p99)

For common QoE dashboard queries (single-asset rebuffering ratio / TTFF
percentiles over a rolling 24h window, as implemented in
`PlaybackBeaconRepository`), against a well-partitioned, appropriately
sized ClickHouse cluster:

| Query pattern | p50 | p95 | p99 |
|---|---|---|---|
| Single-asset, 24h window, aggregate rebuffer/heartbeat counts | 50ms | 150ms | 400ms |
| Single-asset, 24h window, TTFF quantiles | 80ms | 200ms | 500ms |
| Cross-asset (e.g. top-N by rebuffering ratio), 24h window | 300ms | 800ms | 2000ms |
| Firmware-cohort decode-error rate rollup, 7-day window | 200ms | 600ms | 1500ms |

These are documented targets for capacity-planning purposes, not measured
benchmarks (no live ClickHouse cluster is provisioned in this repository —
see "Testing Strategy" below).

---

## Testing Strategy

Run with:

```bash
npm install
npm test
```

**What is genuinely, exhaustively unit-tested** (80 tests, all passing, in
`tests/` — the original 43 plus 37 new), with hand-computed expected values
shown as arithmetic in test comments:

- `src/metrics/qoeMetrics.ts` — all four QoE metric functions.
- `src/anomaly/anomalyStatistics.ts` + `firmwareCohortAnomalyDetector.ts` —
  mean, sample stddev, z-score, the `ZScoreAnomalyDetectionStrategy`, and
  the full `AnomalyDetectedEvent` emission path.
- `src/abtest/significanceTests.ts` + `encodingExperimentEvaluator.ts` —
  Welch's t-test (cross-checked against a textbook critical t-value:
  t=2.086 at df=20 ⇒ p≈0.05) and the promote/rollback/inconclusive decision
  policy.
- `src/ingestion/pipeline/beaconValidationStage.ts` +
  `beaconNormalizationStage.ts` — the full Zod-based validation contract
  and unit coercion/normalization logic.
- `src/webhooks/webhookRetryHttpClient.ts` — retry-with-exponential-backoff
  against a fake `fetch`, including the exact backoff delay schedule, using
  an injected fake sleep function (no real wall-clock delay in the suite).
- `src/abtest/encodingExperimentAutoRemediationService.ts` — verdict
  routing (only `ROLLBACK` triggers a webhook), the exact request URL/body
  shape, and failure propagation via `WebhookDeliveryResult`.
- `src/anomaly/manifestFilterRuleEmitter.ts` — pure `ManifestFilterRule`
  construction from an anomaly event, the Observer-pattern subscription to
  `AnomalyEventEmitter`, and webhook delivery/failure handling.
- `src/metrics/concurrentViewerAggregator.ts` — sliding-window CCU
  counting, per-asset breakdown, window-reset-on-reheartbeat, and the
  eviction logic that bounds memory (including an explicit
  500-then-evict-to-0 test proving no memory leak).
- `src/analytics/qoeChurnCorrelationAnalyzer.ts` — Pearson correlation,
  including a fully hand-verified 5-user fixture (r ≈ 0.869926, arithmetic
  shown step-by-step in the test comment) plus zero-variance/undersized-input
  error cases.
- `src/tracing/redisBackedChunkProvenanceLookup.ts` — register/resolve
  round-trip, key-prefix and TTL correctness, TTL-expiry behavior, and
  multi-key isolation, all against a fake in-memory Redis client (no real
  Redis connection).

**What is structurally correct but NOT exercised against live
infrastructure** (no Redpanda broker or ClickHouse cluster exists in this
environment):

- `src/ingestion/kafkaConsumer.ts` — coded against the real `kafkajs`
  `Kafka`/`Consumer`/`eachBatch` API surface, but never connected to an
  actual broker.
- `src/clickhouse/clickhouseClient.ts` + `playbackBeaconRepository.ts` —
  coded against the real `@clickhouse/client` `createClient`/`.insert`/
  `.query` API surface, but never run against a live ClickHouse instance.
- `src/ingestion/pipeline/beaconEnrichmentStage.ts` — its logic is a
  deterministic, simple mapping (async only because it awaits the
  provenance lookup); covered indirectly via the fact that
  `ChunkProvenanceLookup`'s in-memory implementation is straightforward,
  but has no dedicated async test in this suite.
- `src/tracing/otelSetup.ts` — wired against the real
  `@opentelemetry/sdk-node` API with a `ConsoleSpanExporter` (no OTLP
  Collector assumed to exist); not imported by the test suite.
- `src/index.ts` — the composition root; connects to real infrastructure on
  `main()`, intentionally excluded from `npm test`.
- `src/webhooks/webhookRetryHttpClient.ts`,
  `encodingExperimentAutoRemediationService.ts`,
  `manifestFilterRuleEmitter.ts` — coded against the real global `fetch`
  API, but every test injects a fake `WebhookFetchLike`; no real outbound
  HTTP call is ever made in the suite, and neither of the two receiving
  endpoints exists in this repository (see "Outbound Webhook Contracts").
- `src/tracing/redisBackedChunkProvenanceLookup.ts` — coded against a
  minimal `ChunkProvenanceRedisClientLike` interface the real `ioredis`
  `Redis` client satisfies structurally, but never run against a live
  Redis instance; tests use an in-memory fake client only.

## Limitations / What Is Simulated

- No live Redpanda or ClickHouse cluster is provisioned or reachable in
  this repository; integration correctness against real infrastructure is
  unverified beyond "compiles against the real client library APIs."
- `InMemoryChunkProvenanceLookup` is a reference implementation only — not
  suitable for a multi-replica production deployment (state isn't shared
  across instances); `RedisBackedChunkProvenanceLookup` is the production
  implementation, but it too has never been run against a live Redis
  instance in this environment (tests use a fake client).
- Average Bitrate Played is a simple sample mean, not time-weighted by how
  long each bitrate was held.
- The OpenTelemetry wiring emits to `ConsoleSpanExporter`, not a real OTLP
  collector endpoint — swapping the exporter is a one-line change, noted in
  `otelSetup.ts`.
- The throughput/latency numbers in this README are documented engineering
  targets for capacity planning, not measured benchmark results.
- Both outbound webhooks (`/internal/remediation/rollback-experiment` and
  `/internal/manifest-filters/rules`) are called against a documented
  contract; the receiving endpoints are owned by sibling
  services/teams (control-plane-api-gateway, edge-packaging) and are not
  implemented in this repository, so end-to-end delivery is unverified
  beyond "the request this service sends matches the documented shape."
- `ManifestFilterRuleEmitter`'s anomaly-to-action mapping is fixed
  (`downgradeToH264` for any confirmed firmware-cohort anomaly); a
  pluggable Strategy-style mapping (mirroring
  `AnomalyDetectionStrategy`/`SignificanceTestStrategy`) is a natural
  future extension if other anomaly types need a different remediation.
- `ConcurrentViewerAggregator` is single-process/in-memory (like
  `InMemoryChunkProvenanceLookup`); a multi-replica deployment needs either
  sticky per-session routing to one replica, or a shared store (e.g. Redis
  per-session TTL keys aggregated via keyspace cardinality) for a
  cluster-wide CCU figure.
- `qoeChurnCorrelationAnalyzer.ts` operates on an already-joined dataset;
  the actual QoE-beacon-to-subscription/churn join (presumably a scheduled
  ClickHouse or data-warehouse job) is out of scope for this module and
  not implemented here. Pearson/point-biserial correlation also does not
  imply causation — a documented caveat for how this metric should be
  interpreted by stakeholders.

---

## Project Structure

```
analytics-telemetry-platform/
├── package.json
├── tsconfig.json
├── jest.config.js
├── .env.example
├── README.md
├── sql/
│   └── schema.sql                          # ClickHouse DDL
├── src/
│   ├── index.ts                            # composition root
│   ├── types/beaconTypes.ts
│   ├── schemas/playbackBeaconSchema.ts     # Zod validation contract
│   ├── ingestion/
│   │   ├── kafkaConsumer.ts                # kafkajs consumer
│   │   └── pipeline/
│   │       ├── beaconValidationStage.ts
│   │       ├── beaconNormalizationStage.ts
│   │       ├── beaconEnrichmentStage.ts
│   │       ├── beaconBatchInsertStage.ts
│   │       └── beaconIngestionPipeline.ts
│   ├── clickhouse/
│   │   ├── clickhouseClient.ts
│   │   └── playbackBeaconRepository.ts     # Repository pattern
│   ├── metrics/
│   │   ├── qoeMetrics.ts                   # pure QoE metric functions
│   │   └── concurrentViewerAggregator.ts   # sliding-window CCU aggregation
│   ├── analytics/
│   │   └── qoeChurnCorrelationAnalyzer.ts  # Pearson rebuffering/churn correlation
│   ├── anomaly/
│   │   ├── anomalyStatistics.ts            # mean/stddev/z-score
│   │   ├── firmwareCohortAnomalyDetector.ts # Strategy pattern
│   │   ├── anomalyEventEmitter.ts          # Observer pattern
│   │   └── manifestFilterRuleEmitter.ts    # Observer subscriber -> edge-packaging webhook
│   ├── abtest/
│   │   ├── significanceTests.ts            # Welch's t-test
│   │   ├── encodingExperimentEvaluator.ts  # Strategy pattern
│   │   └── encodingExperimentAutoRemediationService.ts # ROLLBACK -> control-plane webhook
│   ├── webhooks/
│   │   └── webhookRetryHttpClient.ts       # shared retry-with-backoff POST helper
│   └── tracing/
│       ├── otelSetup.ts
│       ├── chunkProvenanceLookup.ts
│       └── redisBackedChunkProvenanceLookup.ts # Repository pattern, Redis-backed
└── tests/
    ├── qoeMetrics.test.ts
    ├── anomalyStatistics.test.ts
    ├── significanceTests.test.ts
    ├── beaconValidationPipeline.test.ts
    ├── webhookRetryHttpClient.test.ts
    ├── encodingExperimentAutoRemediationService.test.ts
    ├── manifestFilterRuleEmitter.test.ts
    ├── concurrentViewerAggregator.test.ts
    ├── qoeChurnCorrelationAnalyzer.test.ts
    └── redisBackedChunkProvenanceLookup.test.ts
```
