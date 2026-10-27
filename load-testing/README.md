# Load Testing — k6

Real k6 scripts (JavaScript, using the actual k6 `http`/`check`/`Trend`
APIs) load-testing the two hottest request paths in the platform:

```
load-testing/
├── control-plane-api-gateway-graphql-load-test.js
├── edge-packaging-and-manifest-service-http-load-test.js
└── README.md   (this file)
```

| Script | Target | Flows | SLO source |
|---|---|---|---|
| `control-plane-api-gateway-graphql-load-test.js` | GraphQL BFF, default port `4000` | `videoAsset` query (asset lookup), `createPlaybackSession` mutation (session creation) | `docs/slo-sli-sla.md` §1 |
| `edge-packaging-and-manifest-service-http-load-test.js` | HTTP API, default port `8080` | `GET /manifest/{assetId}`, `GET /package/{assetId}/{chunkId}` | `docs/slo-sli-sla.md` §4 |

Both scripts use k6's classic single-`default`-function pattern with
`options.stages` for VU ramping and `options.thresholds` asserting p95/p99
targets copied verbatim from `docs/slo-sli-sla.md`. GraphQL operation
names, field names, and input-type shapes are copied from the real
resolvers (`services/control-plane-api-gateway/src/*/graphql/*.ts`); HTTP
routes and query parameters are copied from the real Go handler
(`services/edge-packaging-and-manifest-service/internal/httpapi/httpapi.go`).

## How to run

Prerequisite: [k6](https://k6.io/docs/get-started/installation/) installed
locally (confirmed present in this environment: `k6 v1.6.1`).

### 1. Start the target service(s) against local docker-compose infra

```bash
cd infra && docker compose up -d   # Redis, Temporal, Redpanda, ClickHouse, MinIO

# In a separate terminal, per service:
cd services/control-plane-api-gateway && npm install && npm run start:dev
cd services/edge-packaging-and-manifest-service && go run ./cmd/server
```

### 2. Run a script at full ramp profile

```bash
k6 run load-testing/control-plane-api-gateway-graphql-load-test.js
k6 run load-testing/edge-packaging-and-manifest-service-http-load-test.js
```

Point either script at a non-default host/port:

```bash
k6 run -e GRAPHQL_ENDPOINT_BASE_URL=http://localhost:4000 load-testing/control-plane-api-gateway-graphql-load-test.js
k6 run -e EDGE_PACKAGING_BASE_URL=http://localhost:8080 load-testing/edge-packaging-and-manifest-service-http-load-test.js
```

`createPlaybackSession` is behind `JsonWebTokenAuthenticationGuard`; supply
a real bearer token to exercise the full authenticated path:

```bash
k6 run -e GRAPHQL_LOAD_TEST_BEARER_TOKEN=<jwt> load-testing/control-plane-api-gateway-graphql-load-test.js
```

### 3. Quick syntax/connectivity smoke check

```bash
k6 run --vus 1 --duration 1s --summary-mode=disabled load-testing/control-plane-api-gateway-graphql-load-test.js
k6 run --vus 1 --duration 1s --summary-mode=disabled load-testing/edge-packaging-and-manifest-service-http-load-test.js
```

## How to interpret results against the SLO doc

- Compare the `http_req_duration{operation:...}` p95/p99 lines (and the
  custom `*_duration_ms` Trend metrics, which measure the same thing via
  `response.timings.duration` for clarity in the summary output) against
  the specific target in `docs/slo-sli-sla.md`:
  - asset lookup: p50 8ms / p95 25ms / p99 50ms
  - session creation: p50 40ms / p95 90ms / p99 120ms
  - manifest generation: p99 15ms
  - JIT chunk packaging: p99 30ms
- A **threshold failure** (k6 exits non-zero, prints
  `thresholds on metrics '...' have been crossed`) means the measured
  percentile exceeded the SLO target during that run — treat this as a
  real regression signal against a live target, not against the
  authoring-time dry run described below (which is *expected* to fail
  thresholds, because there is no live service to serve fast responses).
- `http_req_failed` is intentionally set to a loose `rate<0.05` ceiling in
  both scripts (SLA targets are 99.9%–99.99%, i.e. ~0.01%–0.1% failure) —
  tighten this once running against real infra with real traffic volume;
  a small-VU-count local run naturally has too few samples for a
  percentage that small to be statistically meaningful.
- The `createPlaybackSession` check accepts both `200` and `401` as
  "non-crash" outcomes specifically because a local run without a signed
  JWT will legitimately 401 at the auth guard — that is a script-honesty
  choice, not a bug: a 401 tells you the gateway is up and enforcing auth
  correctly, whereas a `0` status (transport-level failure, see below)
  tells you it isn't reachable at all.

## What was actually executed vs. authored-only in this environment

**No live instance of `control-plane-api-gateway` or
`edge-packaging-and-manifest-service` was running in this sandboxed
environment**, so neither script was run end-to-end against real
application traffic, and no real p95/p99 latency numbers were collected.
What **was** actually run and verified, with real command output:

1. **`k6 inspect <script>.js`** on both scripts — confirms the k6 JS
   runtime successfully parses and compiles each file, resolves the
   `options.stages`/`options.thresholds` configuration, and (before the
   scripts were restructured to the single-`default`-function pattern)
   resolved the named `exec` scenario functions. This is k6's built-in
   static-validation path.
2. **`k6 run --vus 1 --duration 1s --summary-mode=disabled -e <BASE_URL>=http://localhost:1 <script>.js`**
   — the exact dry-run pattern specified for this task, against a closed
   local port (`localhost:1`, guaranteed nothing is listening). Real,
   observed output for both scripts:
   - The k6 runtime started, ran the configured VU/iteration loop, and
     executed every `http.post`/`http.get` call in the script.
   - Each request failed with a real, well-formed transport error
     (`dial tcp 127.0.0.1:1: connect: connection refused`) — proving the
     URL construction (query-string building, path-templating for
     `{assetId}`/`{chunkId}`, JSON body encoding for the GraphQL POST)
     executes without a JavaScript runtime error.
   - k6 correctly evaluated the configured thresholds against the (100%
     failed) results and reported
     `thresholds on metrics 'http_req_failed' have been crossed` with a
     non-zero exit code — proving the threshold wiring is live, not just
     present in the config object.

This confirms **script correctness (syntax, API usage, control flow,
threshold/metric wiring)**, not **performance correctness** (i.e., it does
not tell you whether the real services would actually meet the SLO
targets under load) — that requires running these scripts against a live
`docker compose up -d` stack plus the two services started per step 1
above, which was outside what this environment could exercise.
