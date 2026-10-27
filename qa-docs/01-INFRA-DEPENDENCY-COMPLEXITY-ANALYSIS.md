# custom-video-processing-engine — Infra, Dependency & Complexity Analysis

Audience: LLM agents writing/running tests. Nothing here has been executed.

## 1. What it is
Polyglot video pipeline (ingest → inspect → encode ladder → package/manifest → multi-CDN routing → telemetry → QoE ML), 7 services: Go 83 files / 15.9k LOC, TypeScript 109 files / 9.6k LOC, Python 62 files / 10.8k LOC, 1 ClickHouse SQL schema. Existing: 76 test files, `chaos-testing/experiments/` (3 scripts), `load-testing/` (2 k6 scripts), CI in `.github/workflows`, docs `docs/{architecture,testing-and-benchmarking,slo-sli-sla,multi-region-dr,cost-dashboard}.md`.

## 2. Services (`services/`)
| Service | Lang / stack | Surface | Depends on |
|---|---|---|---|
| `control-plane-api-gateway` | NestJS, Apollo GraphQL, JWT/OIDC (passport-openidconnect, openid-client), ioredis, Temporal client+worker | GraphQL: queries `VideoAsset`, `[VideoAsset]`, `PlaybackSession`, `EncodingWorkflowProgress`, `PersonalizedManifestResolution`, `[ActiveRemediationAction]`; mutations returning `VideoAsset`, `PlaybackSession`; REST `internal/remediation`. Modules: asset-registry, encoding-orchestration, playback-session, internal-remediation, redis, common/auth | Redis, Temporal, downstream services |
| `distributed-encoding-orchestrator` | Go (stdlib) | `/metrics`; workers: `ffmpeg`, `ladder`, `checkpoint`, `cost`, `watermark`, `worker` | ffmpeg binary, MinIO, Temporal (via gateway worker) |
| `edge-packaging-and-manifest-service` | Go, prometheus, quic-go | `GET /manifest/{assetId}` (`format`, `drm`, `max_height`, `network`, `rendition`, `X-Session-Id`), `GET /manifest/{assetId}/variant/{renditionFile}`, `GET /package/{assetId}/{chunkId}`, `/healthz`, `/metrics`; packages: `fmp4`, `manifest` (HLS/DASH), `drm`, `ssai`, `filtering`, `catalog`, `quicedge` (HTTP/3) | origin storage |
| `multi-cdn-edge-routing-service` | Go (stdlib) | `GET /route/{assetId}` (`geo`, `asn`), `GET /prefetch-predictions/{userId}`, `/healthz`, `/metrics`; `cdnhealth`, `routing`, `prediction`, `viewinghistory`, `circularstats` | CDN health feeds |
| `analytics-telemetry-platform` | Node/TS, kafkajs, @clickhouse/client, ioredis, zod, OTel | ingestion, schemas, anomaly, abtest, webhooks, analytics, clickhouse, tracing | Redpanda, ClickHouse, Redis |
| `predictive-qoe-ml-service` | Python FastAPI, scikit-learn, numpy, joblib, websockets | `GET /health`, `WS /ws/session/{session_id}` | model artifacts in `models/` |
| `ingestion-inspection-service` | Python FastAPI + CLI (`ingestion_inspection_service.cli.main`), opencv-headless, numpy, prometheus_client | `/healthz`, `/metrics` | source media |

## 3. Infra (`infra/docker-compose.yml`)
Redis 7.2 (6379) · Postgres 16 for Temporal (5432) · Temporal auto-setup 1.24.2 (7233) · Redpanda v24.1.9 (9092) · ClickHouse 24.3 (8123 HTTP, 9000 native) · MinIO (9010 API, 9011 console) · Prometheus v2.53 (9090) · Grafana 11.1 (3000; dashboards in `infra/observability/grafana`). Service ports are not in compose; `:8080`, `:8443`, `:9090` appear in Go mains — resolve each service's port from its `cmd/*/main.go` / `src/main.ts` / uvicorn args before scripting.

## 4. Existing tests (extend, don't duplicate)
ingestion-inspection `tests/` (13), analytics `tests/` (10), orchestrator `internal/{ffmpeg(6),worker(5),ladder(3),watermark,metrics,domain}`, gateway `src/common/auth(4)`, `src/redis(2)`, `test/` (e2e), qoe `tests/` (3), edge-packaging `internal/{manifest(2),httpapi(2),fmp4(2),drm(2),ssai,quicedge,filtering}`, routing `internal/{routing,prediction,httpapi,circularstats,cdnhealth}`.
Chaos: `kill-encoding-worker-mid-job-verify-circuit-breaker-retry.sh`, `partition-redpanda-verify-analytics-consumer-survives.sh`, `stop-redis-verify-control-plane-graceful-degradation.sh`. Load: `control-plane-api-gateway-graphql-load-test.js`, `edge-packaging-and-manifest-service-http-load-test.js`. `scripts/fixtures`, `scripts/aggregate-cost-report.js`.

## 5. Complexity & risk hot-spots
| # | Area | Risk |
|---|---|---|
| 1 | Temporal encoding workflow | activity retries re-running ffmpeg (idempotency of outputs), heartbeat timeouts on long encodes, workflow determinism on code change (replay tests), signal/cancel handling, worker crash mid-activity, checkpoint resume correctness (`checkpoint`) |
| 2 | ffmpeg subprocess mgmt | zombie processes, stderr parsing, timeouts, disk fill by temp files, hostile inputs (parser exploits, decompression bombs, 8K/HDR/odd fps/VFR/rotated/audio-only/corrupt moov) |
| 3 | Ladder generation (`ladder`) + cost | never upscale; odd dimensions; aspect ratios; bitrate caps; cost estimate vs actual |
| 4 | fMP4/CMAF packaging | box structure validity, segment alignment across renditions (keyframe/GOP), timestamp continuity, init segment consistency |
| 5 | Manifest generation | HLS/DASH spec conformance, personalised filtering (`max_height`, `network`), DRM signalling (key ids, PSSH), SSAI ad insertion discontinuities, per-session manifests cacheability |
| 6 | DRM | key leakage in logs/manifests, licence token expiry, wrong key ⇒ undecodable |
| 7 | CDN routing | failover when CDN unhealthy, flapping/hysteresis, geo/ASN fallback, weighted split accuracy, all-CDNs-down |
| 8 | Telemetry pipeline | Kafka at-least-once ⇒ ClickHouse duplicates (dedupe/ReplacingMergeTree?), consumer rebalance, poison messages, schema evolution (zod), late events, back-pressure |
| 9 | Anomaly detection / A-B | false positive storms → `internal/remediation` automatic actions (closed loop!) — guard rails, oscillation |
| 10 | QoE WebSocket | many long-lived sessions, model load time, per-message latency, back-pressure, reconnect |
| 11 | Gateway auth | OIDC flow, JWT validation, session in Redis, Redis outage degradation (existing chaos script) |
| 12 | QUIC edge | HTTP/3 fallback to H2/H1, 0-RTT replay safety |
| 13 | Multi-region DR (`docs/multi-region-dr.md`) | failover drills, RPO/RTO claims |

## 6. Invariants
1. Every asset accepted reaches `READY` or `FAILED` (never stuck) with bounded retries; outputs for a rendition are written exactly once (idempotent paths).
2. Ladder renditions ≤ source resolution/bitrate; all renditions segment-aligned; durations equal ±1 frame.
3. Every manifest returned parses, and every URI it references returns 200 with decodable media.
4. Routing never returns an unhealthy CDN while a healthy one exists.
5. Telemetry: events acked by ingestion appear in ClickHouse exactly once (after dedupe window).
6. DRM keys/secrets never appear in logs, manifests (beyond key IDs), metrics or error bodies.
7. Automatic remediation actions are rate-limited, reversible, and audited.

## 7. Run
`docker compose -f infra/docker-compose.yml up -d`; then each service per its README/`package.json` scripts (`start:dev`, `start:temporal-worker`, `test`, `test:e2e`), `go run ./cmd/...`, `uvicorn`/CLI for Python. Tests: `go test ./...`, `npm test`, `pytest`.

## 8. Caveat
GraphQL operation names/arguments, service ports, Kafka topic names and ClickHouse table names were not read. Take them from `services/control-plane-api-gateway/src/**/**.resolver.ts`, `services/analytics-telemetry-platform/{src,sql}`, and the existing k6 scripts in `load-testing/` (authoritative request shapes).
