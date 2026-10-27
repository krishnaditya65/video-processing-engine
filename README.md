# Custom Video Processing Engine — Hyperscale Streaming Platform

A Netflix-tier video processing, delivery, and analytics platform, built as a
polyglot microservices monorepo. Seven independently deployable services
cover the full lifecycle: raw-asset inspection → distributed scene-aware
encoding → edge packaging/DRM/SSAI/QUIC → multi-CDN routing & predictive
pre-fetch → control-plane orchestration → real-time QoE analytics with
closed-loop remediation → predictive bitrate-degradation ML.

This is a **reference-grade implementation**: every service contains real,
runnable, tested code — not pseudocode or stubs — implementing the core
algorithms (SSIM scene detection, VMAF convex-hull and per-title bitrate
optimization, ISO-BMFF box muxing + real AES-128-CTR encryption, real
HTTP/3-over-QUIC, Welch's t-test for A/B experiments, Isolation Forest
anomaly detection, circular-mean time-of-day statistics, Pearson churn
correlation, etc.). Infrastructure integrations that need a live cluster
(Temporal, Kafka/Redpanda, ClickHouse, a real IdP) are written against the
real client SDKs and are structurally correct, but were not exercised
against a live cluster in this environment — this is called out explicitly
per service in each `README.md` / `SERVICE_README.md` under "Limitations."

> **Read [docs/architecture.md](docs/architecture.md) first** for how the
> seven services fit together, then [docs/slo-sli-sla.md](docs/slo-sli-sla.md)
> for performance targets, and [docs/testing-and-benchmarking.md](docs/testing-and-benchmarking.md)
> for the actual, measured test/benchmark results collected while building this.

---

## Services

| # | Service | Language | Domain | Test Status |
|---|---------|----------|--------|--------------|
| 1 | [`control-plane-api-gateway`](services/control-plane-api-gateway) | TypeScript (NestJS + GraphQL) | Session/asset control plane, Temporal DAG orchestration, Redis cache-aside, DRM strategy selection, OIDC/mTLS auth, internal remediation webhook receiver | 54/54 unit (11 suites) + 1/1 e2e passing |
| 2 | [`ingestion-inspection-service`](services/ingestion-inspection-service) | Python | Scene-cut detection (SSIM), black-frame/color-space/HDR extraction, loudness analysis + remediation, shot-type classification, perceptual dedup, subtitle extraction, sprite-sheet generation | 141/141 passing |
| 3 | [`distributed-encoding-orchestrator`](services/distributed-encoding-orchestrator) | Go | Worker-pool fan-out, FFmpeg subprocess control, VMAF convex-hull + per-title bitrate ladder, A/B forensic watermarking, checkpoint-resume, HW encoder strategies, cost accounting, HDR metadata passthrough | Full suite passing under `-race` (119 test functions, 8 packages) |
| 4 | [`edge-packaging-and-manifest-service`](services/edge-packaging-and-manifest-service) | Go | JIT fMP4/ISO-BMFF packaging with real AES-128-CTR encryption, personalized HLS/DASH manifests, SSAI splicing, multi-DRM key rotation, QUIC/HTTP-3, QoE-driven personalization | 55 test functions passing under `-race` (8 packages) |
| 5 | [`multi-cdn-edge-routing-service`](services/multi-cdn-edge-routing-service) | Go | Weighted multi-CDN routing/failover, predictive viewing-pattern pre-fetch (circular-mean statistics) | 35/35 passing under `-race` (5 packages) |
| 6 | [`analytics-telemetry-platform`](services/analytics-telemetry-platform) | TypeScript | Kafka/Redpanda ingestion, ClickHouse OLAP schema, QoE metrics, firmware-cohort anomaly detection, A/B significance testing, live rollback automation, anomaly auto-remediation, real-time CCU aggregation, churn correlation, Redis-backed distributed tracing | 80/80 passing (10 suites) |
| 7 | [`predictive-qoe-ml-service`](services/predictive-qoe-ml-service) | Python | Isolation Forest bandwidth-degradation prediction, WebSocket downshift commands | 19/19 passing |

**Aggregate: 470+ automated tests across 7 services, all passing**, verified
by actually running each suite (`npm test`, `pytest`, `go test ./... -race`)
rather than assumed. See [docs/testing-and-benchmarking.md](docs/testing-and-benchmarking.md)
for the full breakdown and real numbers (latencies, throughput math, ML
metrics) collected per service.

## Closed-Loop Remediation

Three services now form a feedback loop rather than one-way telemetry:

```
analytics-telemetry-platform            control-plane-api-gateway
  (detects anomaly / A/B rollback)  ──▶   /internal/remediation/*
                                           (InternalServiceAuthGuard,
                                            ActiveRemediationActionsRegistry,
                                            GraphQL activeRemediationActions)

analytics-telemetry-platform            edge-packaging-and-manifest-service
  (ManifestFilterRuleEmitter)       ──▶   RecentQoeSignal / filtering hook
```

Both are real outbound webhook callers with retry-with-backoff on the
analytics side and a real guarded receiver + in-memory registry on the
control-plane side; see each service's README for the exact JSON contract.

## Local Orchestration & Observability

`infra/docker-compose.yml` stands up the shared dependency graph for local
development: Redis, Temporal (+ Postgres), Redpanda, ClickHouse, MinIO,
**and now Prometheus + Grafana**, scraping `/metrics` from all seven
services per `infra/observability/prometheus.yml`, with two hand-authored
dashboards (Platform Overview / RED, Encoding Factory / USE).

```bash
cd infra
docker compose up -d
```

Each service's own `README.md` documents how to run that service against
this local infrastructure. See [`infra/observability/README.md`](infra/observability/README.md)
for the RED/USE methodology behind the dashboards.

## Testing Beyond Unit Tests

- **[`load-testing/`](load-testing)** — k6 scripts for the control-plane
  GraphQL endpoints and edge-packaging HTTP endpoints, with thresholds
  matching [docs/slo-sli-sla.md](docs/slo-sli-sla.md).
- **[`chaos-testing/`](chaos-testing)** — documented fault-injection
  experiments (kill a worker mid-job, stop Redis, partition Redpanda) each
  tied to a specific circuit-breaker/retry mechanism in the actual code.
- **[`.github/workflows/ci.yml`](.github/workflows/ci.yml)** — one CI job
  per service running each service's real test command.

## Naming & Code Conventions

Every service follows one rule: **names are long, descriptive, and
contextual — never abbreviated for brevity.** The casing convention follows
each language's own idiom (documented per-service):

- **TypeScript/Go services** use `camelCase`/`mixedCaps` throughout, e.g.
  `resolvePersonalizedManifestUrlForPlaybackSession`,
  `computeUpperConvexHullFromRateDistortionSamples`,
  `detectFirmwareCohortDecodeErrorAnomaly`,
  `selectOptimalCdnProviderForPlaybackRequest`.
- **Python services** use `snake_case` per PEP 8 (e.g.
  `detect_scene_boundaries_via_structural_similarity`), while still following
  the same "descriptive over terse" philosophy — this is called out
  explicitly in each Python service's README to reconcile it with the
  system-wide camelCase convention used elsewhere.

## Design Patterns Applied

Documented in-code and in each service's README:

- **Repository** — video asset persistence, ClickHouse beacon access,
  encoding-job checkpoint store, Redis-backed distributed-tracing provenance
  lookup, CDN health registry
- **Strategy** — DRM systems (Widevine/FairPlay/PlayReady), codec argument
  builders (H.264/HEVC/AV1 + NVENC/QuickSync hardware variants), scene-detection
  algorithms, manifest format generators (HLS/DASH), anomaly-detection
  algorithms, authentication strategies (dev JWT / OIDC), CDN scoring strategy
- **Builder** — encoding job specs, manifest construction
- **Decorator** — HDR metadata passthrough wrapping codec argument builders
- **Chain of Responsibility** — inspection pipeline stages, device-capability
  and QoE-driven variant-ladder filtering
- **Circuit Breaker** — Temporal client calls, FFmpeg worker failure handling
- **Worker Pool** — bounded-concurrency chunk encoding across a Go worker fleet
- **Observer/EventEmitter** — anomaly-detection event propagation, manifest
  filter rule emission
- **Pipeline** — beacon validation → normalization → enrichment → batch insert;
  network samples → features → model inference → decision; inspection
  black-frame → scene-cut → color-space → loudness → shot-type

## Documentation Map

- [`docs/architecture.md`](docs/architecture.md) — system architecture, data
  flow between all seven services, the closed-loop remediation paths
- [`docs/slo-sli-sla.md`](docs/slo-sli-sla.md) — service-level objectives,
  indicators, and agreements, with p50/p95/p99 targets per service
- [`docs/testing-and-benchmarking.md`](docs/testing-and-benchmarking.md) —
  actual test results, benchmark numbers, and ML metrics measured while
  building this system
- [`docs/multi-region-dr.md`](docs/multi-region-dr.md) — active-active
  control plane, ClickHouse/origin replication, RPO/RTO targets
- [`docs/cost-dashboard.md`](docs/cost-dashboard.md) — per-title
  unit-economics design, backed by a runnable cost-aggregation script
- Per-service `README.md`/`SERVICE_README.md` — architecture, algorithms,
  API surface, run instructions, and known limitations for that service
