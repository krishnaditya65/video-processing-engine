# Testing & Benchmarking Report

This document records what was **actually run** while building this system,
with real output — as distinct from the target numbers in
[slo-sli-sla.md](slo-sli-sla.md). Every number below came from an actual
`npm test` / `pytest` / `go test` invocation during development, not from
estimation.

## Aggregate Test Results

| Service | Test Command | Result |
|---|---|---|
| control-plane-api-gateway | `npm test` (Jest, ts-jest) | **54/54 passed**, 11 suites (up from 22/4) |
| control-plane-api-gateway | `npm run test:e2e` | **1/1 passed** (full `AppModule` DI graph) |
| control-plane-api-gateway | `npx tsc --noEmit` + `npm run build` | Zero type errors; `dist/` emitted; app booted with GraphQL listener bound |
| ingestion-inspection-service | `pytest -v` | **141/141 passed** (up from 67) |
| distributed-encoding-orchestrator | `go build ./...` / `go vet ./...` / `go test ./... -race` | All clean; **119 test functions across 8 packages**, zero data races |
| edge-packaging-and-manifest-service | `go test ./... -race -count=1` | **55 test functions passed**, 8 packages (up from 39/6); `go vet` + `gofmt -l` clean |
| multi-cdn-edge-routing-service (new) | `go test ./... -race` | **35/35 passed**, 5 packages; `go vet` + `gofmt -l` clean |
| analytics-telemetry-platform | `npm test` (Jest) | **80/80 passed**, 10 suites (up from 43/4) |
| analytics-telemetry-platform | `npm run build` | Clean strict-TypeScript compile |
| predictive-qoe-ml-service | `pytest -v` | **19/19 passed** |

**Total: 470+ automated tests passing across 7 services** (three of which
gained substantial new capability during the "add advanced features" pass:
ingestion 67→141, control-plane 22→54, edge-packaging 39→55, analytics
43→80, plus a wholly new 35-test service).

## What Each Suite Actually Covers

### control-plane-api-gateway (original + new)
Circuit breaker state machine, Redis rate limiter, manifest cache-aside
logic, DRM strategy factory selection (original 22). **New (32 tests):**
`AuthenticationStrategyFactory` selection logic (dev-JWT vs OIDC by
config), `DevJwtAuthenticationStrategy` and `OidcAuthenticationStrategy`
(the latter fails closed via an `UnimplementedOidcRelyingPartyClient`
guard rail rather than silently succeeding — verified by test),
`MutualTlsClientCertificateGuard` (trusts a proxy-verified header),
`InternalServiceAuthGuard` (valid/invalid shared secret),
`ActiveRemediationActionsRegistry` (add/query), and the two
`InternalRemediationController` endpoints via NestJS testing module. The
compiled app was booted directly; Redis `ECONNREFUSED` errors appeared as
expected (no live Redis in this sandbox) without crashing the process.

### ingestion-inspection-service (original + new)
Original 67: SSIM/histogram scene-cut math, adaptive thresholding,
black-frame classification, loudness math, pipeline ordering, plus
ffmpeg/ffprobe-backed integration tests. **New (74 tests):** shot-type
classification thresholds (motion + Canny edge-density), dHash perceptual
hashing and Hamming-distance duplicate detection, subtitle-stream
detection/WebVTT extraction (self-skips without ffmpeg subtitle streams),
sprite-sheet coordinate math, and loudness remediation (`loudnorm`-based
correction) — plus an end-to-end CLI smoke test with all 6 new flags
together against a real ffmpeg-generated fixture, confirming shot type
`DIALOGUE`, sprite sheet + VTT written, subtitle self-skip on a
no-subtitle asset, and loudness moved from **-21.78 → -16.00 LUFS**.

### distributed-encoding-orchestrator (original + new)
Original: convex-hull correctness, A/B watermark round trips, exact ffmpeg
argument-slice assertions, worker-pool concurrency + goroutine-leak check,
retry/backoff paths, circuit-breaker transitions. **New:**
`ComputeGlobalPerTitleBitrateLadder` (duration-weighted per-scene hull
aggregation via piecewise-linear VMAF interpolation, hand-worked example in
comments), `JobCheckpointStore` + a simulated-preemption test proving zero
data loss / zero duplicate work across two dispatch phases,
`NvencArgumentBuilder`/`QuickSyncArgumentBuilder` exact hwaccel argument
assertions, `HdrMetadataPassthroughCodecArgumentBuilder` (Decorator
pattern, honest about the `hdr10plus_tool`/`dovi_tool` external-tooling
boundary), `CostAccountant` arithmetic, and Prometheus metrics wiring.
**119 test functions across 8 packages**, re-verified clean across 3
repeated `-count=3 -race` runs (an earlier-flagged flaky test in
`internal/worker` did not reproduce in the final state).

### edge-packaging-and-manifest-service (original + new)
Original 39: ISO-BMFF box byte-layout, SSAI splicing, DRM signaling per
format, low-end-device filtering, manifest structure. **New (16 tests):**
AES-128-CTR encryption round trip on both the raw payload (`internal/drm`)
and the full packaged fMP4 chunk (`internal/fmp4`, length-preserving,
`trun.data_offset` stays valid) — recovers bytes bit-for-bit, includes a
wrong-key negative test and a determinism test; the new QoE-driven variant
ceiling filtering stage; and a real HTTP/3-over-QUIC round trip via
`internal/quicedge` (`*http3.Server` + `http3.Transport`, genuine QUIC
handshake over an ephemeral UDP port). Manually verified `cmd/server`
binds both `:18080` (TCP) and `:18443` (UDP) simultaneously.

### multi-cdn-edge-routing-service (new service, 35 tests)
Hand-computed CDN scoring-formula fixtures (`score = 0.5·latency +
0.3·errorRate + 0.2·cost`, min-max normalized), automatic failover on a
health-threshold breach, an exact-tie fixture proving deterministic
alphabetical tie-breaking, total-CDN-collapse and no-data error paths, and
circular-mean/stddev time-of-day statistics — including a hand-computed
midnight-wraparound fixture (23:00/01:00 sessions → mean 00:00, R≈0.9659,
stddev≈60.35min) proving the predictor doesn't break at the day boundary
the way a naive arithmetic mean would. Manually smoke-tested all four HTTP
endpoints against a running server.

### analytics-telemetry-platform (original + new)
Original 43: QoE metrics, anomaly z-score engine, Welch's t-test, beacon
validation pipeline. **New (37 tests):** `webhookRetryHttpClient`
(exponential backoff, mocked fetch/timers), `EncodingExperimentAutoRemediationService`
(calls the control-plane rollback endpoint), `ManifestFilterRuleEmitter`
(calls the edge-packaging filter-rule endpoint), `ConcurrentViewerAggregator`
sliding-window eviction (proven leak-free via a 500-session-then-evict-to-0
test), `qoeChurnCorrelationAnalyzer`'s Pearson correlation (fixture
hand-verified to **r≈0.869926**), and `RedisBackedChunkProvenanceLookup`
against a fake Redis client.

### predictive-qoe-ml-service
Unchanged from the original build (not in scope for the "advanced
features" pass): rolling-window feature engineering, training/evaluation
harness, FastAPI endpoint via `TestClient`. Real sklearn-computed ML
metrics on synthetic data — see below.

**Real measured ML metrics** (150 stable + 150 degrading synthetic
sessions, 75/25 stratified split, `IsolationForest(n_estimators=100,
contamination=0.25, random_state=42)`):

| Metric | Value |
|---|---|
| Precision (anomalous class) | 0.8614 |
| Recall | 0.9251 |
| F1 | 0.8921 |

Labeled explicitly as *"measured on synthetic data, not production
traffic."* Measured in-process pipeline latency (500 iterations, warmed
up): mean 9.68ms, p50 8.76ms, **p99 15.94ms** — under the 20ms target.

## Cross-Cutting Infrastructure Verification

Beyond the seven services, the following was built and — where the
sandbox allowed — actually executed:

- **`docker compose config -q`** validates `infra/docker-compose.yml`,
  including the two new Prometheus/Grafana services.
- **YAML/JSON parsing** — all 5 new YAML files (prometheus.yml,
  docker-compose.yml diff, ci.yml, 2 Grafana provisioning files) and 3 JSON
  files (2 dashboards, 1 cost fixture) parse cleanly via `python3 -c
  "import yaml/json..."`.
- **k6 load-test scripts** — `k6 inspect` confirmed compilation; a real
  dry run (`--vus 1 --duration 1s`) against a closed port produced real
  connection-refused errors and correctly failed threshold evaluation,
  proving the scripts are live-wired. **Not** run against actual live
  services (no real p95/p99 numbers collected yet).
- **Chaos experiment scripts** — `bash -n` syntax-checked, and run in
  their default dry-run mode producing real hypothesis/procedure/pass-criteria
  output. Actual fault-injection commands (`docker kill`, `docker compose
  stop`) were not executed against a live service.
- **Cost-aggregation script** (`scripts/aggregate-cost-report.js`) —
  actually run against `scripts/fixtures/sample-per-title-cost-report.json`;
  produced a full per-title + aggregate cost breakdown, and its
  reconciliation logic caught a deliberately-seeded inconsistency in the
  fixture, proving it's not just pass-through printing.
- **CI workflow** (`.github/workflows/ci.yml`) — valid GitHub Actions YAML
  with one job per service running that service's real test command;
  never run on an actual GitHub Actions runner.

## What Was Not Exercised Live

None of the seven services were run against a live multi-node cluster in
this environment. Specifically not exercised end-to-end:

- A live Temporal server executing `distributedEncodingWorkflow`
- A live Redpanda/Kafka broker feeding the analytics ingestion pipeline
- A live ClickHouse cluster receiving real inserts/queries
- Real ffmpeg encoding of a real production-length video asset
- Real DRM license-server round trips, and real OIDC handshake against a
  live IdP
- The two closed-loop remediation webhooks delivered end-to-end between
  live instances of both services (each side is tested against a fake of
  the other)
- Prometheus actually scraping live `/metrics` endpoints, or the Grafana
  dashboards imported into a running Grafana instance
- The load-test and chaos-test scripts run against live services rather
  than dry-run/syntax-verified

`infra/docker-compose.yml` provisions all of the above locally (Redis,
Temporal+Postgres, Redpanda, ClickHouse, MinIO, Prometheus, Grafana) for
anyone who wants to exercise these integration paths for real.

## How to Reproduce These Results

```bash
# TypeScript services
cd services/control-plane-api-gateway && npm install && npm test && npm run test:e2e
cd services/analytics-telemetry-platform && npm install && npm test

# Python services
cd services/ingestion-inspection-service && python -m venv .venv && source .venv/bin/activate && pip install -r requirements.txt && pytest -v
cd services/predictive-qoe-ml-service && python -m venv .venv && source .venv/bin/activate && pip install -r requirements.txt && pytest -v

# Go services
cd services/distributed-encoding-orchestrator && go test ./... -race
cd services/edge-packaging-and-manifest-service && go test ./... -race -count=1
cd services/multi-cdn-edge-routing-service && go test ./... -race

# Cross-cutting infra
cd infra && docker compose config -q
node scripts/aggregate-cost-report.js
k6 run load-testing/control-plane-api-gateway-graphql-load-test.js
```
