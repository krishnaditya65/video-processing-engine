# SLOs, SLIs, and SLAs

This document defines the Service Level Indicators (SLIs — what is
measured), Service Level Objectives (SLOs — the internal target for each
SLI), and the Service Level Agreement (SLA — the external commitment
derived from the SLOs with margin) for each service.

**Convention:** targets below are engineering targets consistent with
published, well-known figures for large-scale video platforms, used here as
design goals for this architecture — they are labeled `TARGET`, distinct
from `MEASURED` numbers that were actually collected while building this
repository (see [testing-and-benchmarking.md](testing-and-benchmarking.md)
for every measured number and how it was obtained).

---

## 1. control-plane-api-gateway

| SLI | p50 (TARGET) | p95 (TARGET) | p99 (TARGET) | SLA |
|---|---|---|---|---|
| GraphQL asset lookup (Redis cache hit) | 8ms | 25ms | 50ms | 99.9% of requests < 100ms |
| Playback session creation | 40ms | 90ms | 120ms | 99.9% of requests < 250ms |
| Temporal workflow dispatch (enqueue only, not full DAG) | 15ms | 40ms | 60ms | 99.5% of requests < 150ms |
| Internal remediation webhook receipt (`/internal/remediation/*`) | 5ms | 15ms | 25ms | 99.9% of requests < 100ms — internal, guarded by shared-secret header, rate-limited |

**Error budget:** 0.1% of GraphQL requests may fail per rolling 30-day
window before the SLA is breached (≈ 43 minutes of full downtime-equivalent
per month at typical QPS).

**MEASURED:** Jest unit + e2e suite (**54 unit tests across 11 suites + 1
e2e test**, up from the original 22/4) completed in single-digit seconds
wall-clock; `npx tsc --noEmit` and `npm run build` both clean; the compiled
app booted successfully and bound its GraphQL listener (see service
SERVICE_README.md for the full breakdown, including the new §7–9 covering
OIDC/mTLS auth strategy selection and the internal remediation webhook
contract). No live-traffic latency was measured (no load generator run
against a live deployment in this environment; see `load-testing/` for
the k6 scripts authored to collect this).

---

## 2. ingestion-inspection-service

| SLI | TARGET |
|---|---|
| Structural-map generation time for a 2-hour 4K asset | < 90s (via 1fps frame-sampled analysis, not full-frame-rate decode) |
| Scene-cut detection precision (manual QA sample) | > 95% agreement with human-labeled cuts |
| API availability (`POST /assets/{assetId}/inspect`) | 99.9% |

**Explicit tradeoff documented:** 1fps sampling trades recall on
sub-second shots for a 24x–30x reduction in frames analyzed vs. full
frame-rate decode, which is what makes the 90s target achievable — tunable
via `analysis_frames_per_second`.

**MEASURED:** `pytest -v` → **141 passed, 0 failed** (67 original + 74 new
covering shot-type classification, perceptual-hash dedup, subtitle
extraction, sprite-sheet generation, and loudness remediation), including
real ffmpeg/ffprobe-backed integration tests against a generated sample
video, plus an end-to-end CLI smoke test confirming loudness correction
moved a fixture from -21.78 LUFS to the -16.00 LUFS target.

---

## 3. distributed-encoding-orchestrator

| SLI | TARGET |
|---|---|
| End-to-end distributed encode of a 2-hour film across a 500-worker fleet | < 8 minutes wall-clock |
| Worker-pool scheduling overhead (task dispatch, not encode time) | Negligible relative to encode time |
| ffmpeg subprocess failure recovery (circuit breaker + backoff) | Worker resumes within 3 retry attempts, exponential backoff |
| Checkpoint-resume after simulated worker preemption | Zero data loss, zero duplicate work (proven by test, not just claimed) |
| Per-title bitrate ladder computation | Sub-second for a typical multi-scene title (duration-weighted convex-hull aggregation) |

**Throughput math (documented in service README, reproduced here):**
A 2-hour film → 3,600 two-second chunks (verified by test). At an assumed
15 encode-seconds/chunk, one bitrate-ladder rung costs 54,000 CPU-seconds;
÷ 500 workers ≈ 108s (1.8 min) per rung; 4 ladder rungs ≈ 7.2 min — under
the 8-minute target.

**MEASURED (Apple M4, in-repo benchmarks):**
- Convex-hull computation over 10,000 rate-distortion samples: **≈7.6ms**
- WorkerPool dispatching + completing 1,000 trivial tasks end-to-end: **≈2.3ms**
- Full test suite (`go test ./... -race`): **119 test functions across 8
  packages, zero failures**, re-verified clean across 3 repeated `-race`
  runs during doc reconciliation (an earlier-flagged flaky test in
  `internal/worker` did not reproduce in the final state).

---

## 4. edge-packaging-and-manifest-service

| SLI | p99 (TARGET) |
|---|---|
| Personalized manifest generation | < 15ms |
| JIT chunk packaging (2-second chunk, fMP4 mux + AES-128-CTR encryption) | < 30ms |
| Edge availability | 99.99% (this plane runs on every playback request) |
| QUIC/HTTP-3 connection establishment (0-RTT resumption) | Lower than TCP+TLS handshake latency by design — no head-of-line blocking across Wi-Fi/5G transitions |

**MEASURED:** `go test ./... -race -count=1` → **55 test functions passing**
across 8 packages (39 original + 16 new: AES round-trip encryption, QoE
filtering stage, real HTTP/3-over-QUIC round trip); `go vet` and `gofmt -l`
clean; manually verified live via `go run ./cmd/server` binding both a TCP
(`:18080`) and a UDP/QUIC (`:18443`) listener simultaneously, with curl and
a real `http3.Transport` request/response confirming both paths work and
returning correct manifest, DASH, and JIT-packaged fMP4 chunk output. AES
round-trip tests recover bytes bit-for-bit. In-process Go benchmarks exist
in `internal/manifest/bench_test.go` and `internal/httpapi/bench_test.go`
— these are in-process function timings, not real network/edge
measurements, which is called out explicitly.

---

## 4b. multi-cdn-edge-routing-service

| SLI | TARGET |
|---|---|
| CDN routing decision latency | p99 < 10ms (in-memory health registry lookup + scoring) |
| Failover trigger latency after a CDN crosses its health threshold | Next request after threshold breach routes away — no stale routing window by design |
| Pre-fetch prediction accuracy proxy (pattern-detection stddev threshold) | Tight-cluster viewing patterns (stddev below threshold) predicted; scattered patterns correctly declined |

**MEASURED:** `go build ./...`, `go vet ./...`, `go test ./... -race` →
**35/35 tests passing across 5 packages**, gofmt-clean. Hand-computed
fixtures verified: the weighted scoring formula
(`score = 0.5·latency + 0.3·errorRate + 0.2·cost`, min-max normalized),
deterministic alphabetical tie-break on an exact-tie fixture, and a
midnight-wraparound circular-mean fixture (23:00/01:00 sessions → mean
00:00, R≈0.9659, stddev≈60.35min) proving the time-of-day statistics don't
break at the day boundary the way a naive arithmetic mean would. Manually
smoke-tested all four HTTP endpoints against a running server.

---

## 5. analytics-telemetry-platform

| SLI | TARGET |
|---|---|
| Telemetry ingestion throughput | 2,000,000 beacons/sec sustained, via micro-batched writes (N rows every T ms) |
| QoE dashboard query (e.g. rebuffer ratio by ISP, last 1h) | p50 < 200ms, p95 < 800ms, p99 < 2s |
| Anomaly detection alert latency (firmware cohort z-score crossing 3σ) | < 60s from beacon ingestion to `AnomalyDetectedEvent` |
| Anomaly → manifest-filter-rule webhook delivery | p99 < 500ms including retry-with-backoff budget |
| A/B rollback verdict → control-plane webhook delivery | p99 < 500ms including retry-with-backoff budget |
| Real-time CCU aggregation query | p99 < 5ms (in-memory sliding-window lookup) |

**Schema design rationale (ClickHouse):** `ORDER BY (assetId,
toStartOfHour(eventTimestamp))` groups high-cardinality time-series columns
so that adjacent rows have small deltas, which is what MergeTree's
delta-of-delta-style compression exploits — documented in full in
`services/analytics-telemetry-platform/sql/schema.sql` and the service
README.

**MEASURED:** `npm test` → **80/80 tests passing across 10 suites** (43
original + 37 new covering the webhook retry client, A/B rollback
automation, anomaly manifest-filter-rule emitter, CCU sliding-window
aggregator, Pearson churn correlation, and the Redis-backed provenance
lookup). `npm run build` clean. Core statistics hand-verified in test
comments: Welch's t-test cross-checked against the textbook critical value
(t=2.086, df=20, p≈0.05); anomaly z-score engine uses Bessel-corrected
sample stddev; Pearson correlation fixture hand-verified to r≈0.869926; CCU
eviction proven leak-free via a 500-session-then-evict-to-0 test.

---

## 6. predictive-qoe-ml-service

| SLI | TARGET |
|---|---|
| Prediction inference latency | p99 < 20ms (to preserve the 10-second early-warning budget) |
| Bandwidth-degradation detection precision/recall | > 0.80 / > 0.80 on held-out data |

**MEASURED (real, on synthetic data — explicitly not production traffic):**
- `pytest -v` → **19 passed, 0 failed**
- IsolationForest trained on 150 stable + 150 degrading synthetic sessions
  (75/25 stratified split): **Precision 0.8614, Recall 0.9251, F1 0.8921**
- In-process pipeline latency (500 iterations, warmed up): mean **9.68ms**,
  p50 **8.76ms**, **p99 15.94ms** — under the 20ms target.

---

## Cross-Cutting Availability Targets

| Plane | Target Availability | Rationale |
|---|---|---|
| Edge Packaging & Manifest | 99.99% | On the critical path for every single playback request |
| Multi-CDN Routing | 99.99% | Also on the request-startup critical path; a routing-decision failure should fail open to a default CDN, not block playback |
| Control Plane API Gateway | 99.9% | Session creation is on the startup critical path; manifest resolution is cache-aside so brief control-plane blips don't stall active playback |
| Encoding Factory | 99.5% (per-job) | Asynchronous/batch; a failed job retries rather than causing user-facing impact — checkpoint-resume means a worker preemption no longer restarts a whole job |
| Analytics/Telemetry Ingestion | 99.9%, with graceful beacon-loss tolerance | Decoupled via Redpanda so a slow consumer never blocks playback; some beacon loss under extreme load is acceptable, sustained data-lake unavailability is not |
| Predictive QoE ML | 99.5% | Degrades gracefully — client ABR heuristics remain the fallback if predictions are unavailable |

## Closed-Loop Remediation SLIs

| SLI | TARGET |
|---|---|
| End-to-end: anomaly detected → manifest filter rule active at edge | < 90s (detection latency + webhook delivery + edge config reload, all budgeted above) |
| End-to-end: A/B experiment rollback verdict → control-plane registry updated | < 60s |
| Webhook delivery reliability (retry-with-backoff) | 3 attempts, exponential backoff, before an alert fires for manual intervention |

**Verification status:** both webhook paths are tested against fakes of
the counterparty service (per `docs/testing-and-benchmarking.md`), not
delivered end-to-end against live instances of both services in this
environment. `chaos-testing/` documents how to verify this end-to-end once
both services are running.
