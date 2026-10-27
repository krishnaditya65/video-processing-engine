# Observability Stack — Prometheus + Grafana

This directory adds cross-cutting metrics collection and visualization on
top of the six original services plus the incoming `multi-cdn-edge-routing-service`.
It does **not** modify anything inside `services/*` — it only scrapes the
`/metrics` endpoints sibling work is adding to each service and visualizes
them.

## What's here

```
infra/observability/
├── prometheus.yml                                  # scrape config, all 7 services
├── README.md                                        # this file
└── grafana/
    ├── dashboards/
    │   ├── platform-overview-dashboard.json          # RED dashboard, cross-service
    │   └── encoding-factory-dashboard.json            # USE dashboard, encoding factory
    └── provisioning/
        ├── datasources/prometheusDatasource.yml       # auto-registers the Prometheus datasource
        └── dashboards/dashboardProvider.yml            # auto-loads the two dashboards above
```

`infra/docker-compose.yml` now also runs `prometheusMetricsServer` (port
`9090`) and `grafanaDashboardServer` (port `3000`, `admin`/`admin`), wired
to mount the files above.

## Methodology: RED for request-driven services, USE for the worker pool

Two complementary methodologies are applied, matching the two dashboards:

- **RED** (Rate, Errors, Duration) — used for every service on the
  request/response path: `control-plane-api-gateway` (GraphQL),
  `edge-packaging-and-manifest-service` and `multi-cdn-edge-routing-service`
  (HTTP), `ingestion-inspection-service` and `predictive-qoe-ml-service`
  (HTTP/FastAPI), and the beacon-ingestion rate for
  `analytics-telemetry-platform`. This is the standard methodology for
  anything you can meaningfully describe as "how many requests, how many
  failed, how long did they take" — see the **Platform Overview** dashboard.
- **USE** (Utilization, Saturation, Errors) — used for the
  `distributed-encoding-orchestrator` worker pool, because that plane is
  not request/response-shaped: it is a bounded-goroutine fleet consuming a
  job queue, so the more useful question is "how full is the resource, how
  much is queued behind it, and how often does the resource itself fail" —
  see the **Encoding Factory** dashboard, which maps directly onto the
  `WorkerPool`, `FfmpegFailureCircuitBreaker`
  (`services/distributed-encoding-orchestrator/internal/ffmpeg/circuitBreaker.go`),
  and `retryPolicy`
  (`services/distributed-encoding-orchestrator/internal/worker/retryPolicy.go`)
  components described in `docs/architecture.md`.

## Dashboards

### `platform-overview-dashboard.json` — RED, cross-service

- **Rate**: `sum(rate(http_requests_total[...])) by (service)` across all
  seven services, plus a GraphQL-operation-level breakdown for
  `control-plane-api-gateway` (`createPlaybackSession`, `playbackSession`,
  `resolvePersonalizedManifestUrlForPlaybackSession`, `videoAsset`,
  `allVideoAssets` — the real resolver names in
  `services/control-plane-api-gateway/src/*/graphql/*.ts`).
- **Errors**: HTTP 5xx error-rate percentage per service, with a threshold
  line at the 0.1% GraphQL error budget from `docs/slo-sli-sla.md` §1, plus
  a `redisSessionAndCacheStore` connectivity stat panel that mirrors the
  degrade-gracefully-on-`ECONNREFUSED` behavior recorded in
  `docs/testing-and-benchmarking.md` ("Redis `ECONNREFUSED` errors appeared
  as expected... without crashing the process").
- **Duration**: p50/p95/p99 latency panels for playback-session creation,
  manifest/JIT-package generation, and predictive-QoE inference latency,
  each with the exact target value from `docs/slo-sli-sla.md` drawn in as a
  threshold line. Also includes the analytics beacon-ingestion
  throughput/drop-rate panel against the 2,000,000 beacons/sec target.

### `encoding-factory-dashboard.json` — USE, encoding factory plane

- **Utilization**: worker-pool active-vs-max-concurrency gauge and
  time series.
- **Saturation**: chunk-encoding job queue depth, and job-dispatch-overhead
  p99 (target: negligible relative to encode time, per
  `docs/slo-sli-sla.md` §3).
- **Errors**: `FfmpegFailureCircuitBreaker` state (CLOSED/HALF_OPEN/OPEN),
  ffmpeg subprocess failure rate, and per-attempt retry counts (target:
  worker resumes within 3 retry attempts, exponential backoff).
- **Job completion & throughput**: succeeded vs. permanently-failed job
  completion rate, rolling job wall-clock p50 against the < 8 minute
  2-hour-film target, and a forward-looking CostAccountant CPU/GPU-seconds
  panel (see `docs/cost-dashboard.md` — that component is being built
  concurrently, so its exact metric name is not yet confirmed).

## Port provenance — confirmed vs. assumed

`prometheus.yml`'s header comment documents this in full per target, but in
summary:

| Service | Port | Status |
|---|---|---|
| control-plane-api-gateway | 4000 | **Confirmed** — `HTTP_SERVER_LISTEN_PORT` default in `applicationConfigurationService.ts` |
| ingestion-inspection-service | 8080 | **Confirmed** — README's documented `uvicorn ... --port 8080` |
| edge-packaging-and-manifest-service | 8080 | **Confirmed** — `defaultListenAddress = ":8080"` in `cmd/server/main.go` |
| predictive-qoe-ml-service | 8000 | **Confirmed** — README's uvicorn command omits `--port`, so it binds uvicorn's default (8000) |
| distributed-encoding-orchestrator | 9100 | **Assumed** — no HTTP listener exists in the current `cmd/orchestrator/main.go` demo binary; no port documented anywhere |
| analytics-telemetry-platform | 9464 | **Assumed** — Kafka-consumer-only service today, no HTTP listener documented; 9464 is the OpenTelemetry-community-conventional Prometheus exporter port for Node |
| multi-cdn-edge-routing-service | 8082 | **Assumed** — go.mod vendors `prometheus/client_golang` and scaffolds `internal/httpapi`, but no `cmd/server/main.go` listener/port exists yet at authoring time |

**None of these targets were scraped live** — no service in this monorepo
was running with a bound `/metrics` endpoint at the time this file was
written (this observability stack was built concurrently with, and slightly
ahead of, the sibling work adding those endpoints). What was verified:

- `prometheus.yml` is syntactically valid YAML (`python3 -c "import yaml; yaml.safe_load(...)"`, see below).
- `infra/docker-compose.yml` is valid Compose syntax including the two new
  services (`docker compose config -q`, see below).
- Both dashboard JSON files are syntactically valid JSON
  (`python3 -c "import json; json.load(...)"`) and hand-checked against
  Grafana's dashboard schema (schemaVersion 39: `panels[]` with `gridPos`,
  `datasource`, `targets[].expr`, `fieldConfig`, etc.) — not yet imported
  into a running Grafana instance, since no Grafana container was started
  in this environment.
- The PromQL `expr` strings are hand-written against plausible, standard
  metric-naming conventions (`http_requests_total`,
  `http_request_duration_seconds_bucket`, Go/Node/Python Prometheus client
  library defaults) and the domain-specific metric names implied by real
  component names in the codebase (`encoding_circuit_breaker_state`,
  `encoding_worker_pool_active_workers`, etc.) — **these exact metric names
  are not guaranteed to match what the sibling `/metrics` implementations
  actually export**; treat them as a first draft to reconcile once those
  land, not as verified-correct queries.

## How to use this

```bash
cd infra
docker compose up -d prometheusMetricsServer grafanaDashboardServer
# Prometheus UI:  http://localhost:9090  (Status -> Targets to see scrape health)
# Grafana UI:     http://localhost:3000  (admin/admin; dashboards auto-load
#                  under the "Hyperscale Video Platform" folder)
```

If a target shows `context deadline exceeded` / connection refused in
Prometheus's Targets page, that means the corresponding service either
isn't running on the host at the documented/assumed port, or hasn't
finished getting its `/metrics` endpoint added yet — check that service's
`README.md`/`SERVICE_README.md` for its actual current port and update
`prometheus.yml` accordingly (the header comment explains how to retarget
`host.docker.internal:<port>` if the port has changed, or swap to a
container DNS name if the service gets containerized in this compose file
later).
