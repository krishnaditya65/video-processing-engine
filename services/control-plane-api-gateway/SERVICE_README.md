# Control Plane & API Gateway

A NestJS + GraphQL microservice acting as the **Backend-for-Frontend (BFF) and control
plane** for a hyperscale video processing/streaming platform. It is the system client
applications talk to; it does not itself transcode video or serve segments.

---

## 1. Purpose

This service owns four responsibilities:

1. **Asset registry** — registers raw video assets (ProRes/mezzanine uploads), tracks
   their ingestion + encoding lifecycle, and stores the "structural map" (scene
   segmentation + complexity scoring) produced by the upstream ingestion service.
2. **GraphQL BFF** — a single GraphQL API aggregating asset metadata, playback session
   creation, device capability / DRM negotiation, and manifest URL resolution, so
   client apps (web, mobile, TV, set-top-box) make one call instead of orchestrating
   several backend services themselves.
3. **Playback session orchestration** — creates playback session records (device
   type, network hints, negotiated DRM), backed by Redis for session state and
   rate limiting.
4. **Distributed encoding orchestration** — starts and monitors
   `distributedEncodingWorkflow`, a Temporal.io workflow modelling the DAG:
   `ingest → generate structural map → fan out per-scene encode jobs → convex-hull
   bitrate selection → package → publish`.

---

## 2. Architecture

```
                                   ┌─────────────────────────────┐
                     GraphQL       │   Control Plane & API        │
   Client Apps  ───────────────►  │   Gateway (this service)     │
 (web/mobile/TV)                  │                               │
                                   │  ┌─────────────────────────┐  │
                                   │  │ GraphQL Resolvers (BFF) │  │
                                   │  └─────────┬───────────────┘  │
                                   │            │                  │
                    ┌──────────────────────────┼──────────────────────────┐
                    │              │            │            │            │
                    ▼              ▼            ▼            ▼            │
          ┌──────────────┐ ┌──────────────┐ ┌─────────┐ ┌────────────┐    │
          │ AssetRegistry│ │ PlaybackSess.│ │  DRM    │ │ Encoding   │    │
          │   Module     │ │ Orchestration│ │Strategy │ │Orchestration│   │
          │ (Repository  │ │   Module     │ │ Factory │ │  Module     │   │
          │  pattern,    │ │              │ │(Strategy│ │ (Circuit    │   │
          │  in-memory)  │ │              │ │ pattern)│ │  Breaker)   │   │
          └──────┬───────┘ └──────┬───────┘ └─────────┘ └──────┬─────┘   │
                  │                │                            │         │
                  │                ▼                            ▼         │
                  │         ┌─────────────┐            ┌──────────────────┴─┐
                  │         │  RedisModule │            │ Temporal Workflow  │
                  │         │ (rate limit, │            │ Client             │
                  │         │  cache-aside)│            └─────────┬──────────┘
                  │         └──────┬───────┘                      │
                  │                │                               │ gRPC (Temporal
                  │                ▼                               │ frontend)
                  │         ┌─────────────┐                        ▼
                  │         │    Redis    │              ┌──────────────────────┐
                  │         └─────────────┘              │  Temporal Server      │
                  │                                       │  (self-hosted /      │
                  ▼                                       │   Temporal Cloud)    │
     (in-memory map today;                                └──────────┬───────────┘
      swap for Postgres/                                             │ polls task queue
      DynamoDB in prod)                                              ▼
                                                            ┌──────────────────────┐
                                                            │ distributedEncoding   │
                                                            │ Worker process        │
                                                            │ (runDistributedEncoding│
                                                            │  Worker.ts) — executes │
                                                            │ workflow + activities  │
                                                            │ (ingest, structural    │
                                                            │ map, per-scene encode, │
                                                            │ convex-hull select,    │
                                                            │ package, publish)      │
                                                            └──────────────────────┘
```

This service sits **upstream of the CDN/origin and downstream of client apps**: it does
not stream bytes itself. It fits into the larger platform as follows:

- **Upstream ingestion service** (not in this repo) uploads raw mezzanine files and
  eventually reports back a structural map, consumed here via the asset registry.
- **Distributed transcode farm / Temporal workers** perform the actual per-scene
  encoding, orchestrated by the `distributedEncodingWorkflow` defined in this
  service but *executed* by worker processes (which may run in a completely separate
  fleet — see §6).
- **CDN / origin / packaging service** ultimately serves the manifests and segments
  this gateway resolves URLs for; those calls are simulated here (see §10).
- **Client applications** talk to this service exclusively via GraphQL.

---

## 3. API surface (GraphQL schema excerpt)

Full generated schema: `src/graphql/schema.gql` (auto-generated by the code-first
GraphQL driver on every build/boot from the TypeScript decorators — see
`src/app.module.ts`).

```graphql
type Mutation {
  "Registers a newly-uploaded raw mezzanine/ProRes video asset."
  registerRawVideoAsset(input: RegisterRawVideoAssetInput!): VideoAsset!

  "Creates a playback session, negotiating DRM for the requesting device."
  createPlaybackSession(input: CreatePlaybackSessionInput!): PlaybackSession!

  "Starts the distributedEncodingWorkflow Temporal workflow (circuit-breaker protected)."
  startEncodingForVideoAsset(videoAssetIdentifier: ID!): VideoAsset!
}

type Query {
  videoAsset(videoAssetIdentifier: ID!): VideoAsset!
  allVideoAssets: [VideoAsset!]!
  playbackSession(playbackSessionIdentifier: ID!): PlaybackSession!

  "Cache-aside (Redis) resolved manifest URL + bitrate ladder for a session."
  resolvePersonalizedManifestUrlForPlaybackSession(
    playbackSessionIdentifier: ID!
  ): PersonalizedManifestResolution!

  "Live progress of an asset's encoding workflow (null if unavailable)."
  encodingWorkflowProgressForVideoAsset(videoAssetIdentifier: ID!): EncodingWorkflowProgress
}
```

Mutations require a `Bearer` JWT (`JsonWebTokenAuthenticationGuard`); every resolver is
additionally protected by `RedisBackedRateLimitingGuard`.

---

## 4. How to run locally

**Prerequisites verified in this build:** Node v24.12.0, npm 11.6.2 (see §11 for what was
actually executed).

```bash
cd projects/custom-video-processing-engine/services/control-plane-api-gateway
npm install
npm run build      # compiles with `nest build` (tsc under the hood)
npm test           # runs the Jest unit test suite
npm run start:dev  # boots the GraphQL server on :4000 (default), watch mode
```

The service **boots successfully without a live Redis or Temporal server** — Redis
connection is attempted eagerly by `ioredis` but failures only affect requests that
actually touch Redis (rate limiting, session creation, manifest caching); GraphQL
queries/mutations that don't need Redis still work. To exercise the full path:

```bash
# Redis (required for rate limiting, session state, manifest cache-aside):
docker run -p 6379:6379 redis:7

# Temporal (required only for startEncodingForVideoAsset / encoding progress queries):
# see https://docs.temporal.io/cli#start-dev-server — the Temporal CLI's `temporal
# server start-dev` command is the fastest way to get a local dev server + Web UI.
temporal server start-dev

# In a second terminal, once a Temporal server is reachable, start the worker that
# actually executes distributedEncodingWorkflow:
npm run start:temporal-worker
```

Environment variables (all optional; see `src/config/applicationConfigurationService.ts`
for the full, authoritative list and defaults):

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_SERVER_LISTEN_PORT` | `4000` | GraphQL/HTTP listen port |
| `JSON_WEB_TOKEN_SIGNING_SECRET` | dev-only insecure default | HS256 JWT secret |
| `REDIS_CONNECTION_URL` | `redis://localhost:6379` | Redis connection |
| `MANIFEST_CACHE_ENTRY_TTL_SECONDS` | `30` | Manifest cache-aside TTL |
| `RATE_LIMIT_MAX_REQUESTS_PER_WINDOW` | `100` | Rate limit ceiling |
| `RATE_LIMIT_WINDOW_DURATION_SECONDS` | `60` | Rate limit window |
| `TEMPORAL_SERVER_FRONTEND_ADDRESS` | `localhost:7233` | Temporal frontend |
| `TEMPORAL_NAMESPACE` | `default` | Temporal namespace |
| `TEMPORAL_ENCODING_TASK_QUEUE_NAME` | `distributed-encoding-task-queue` | Task queue |
| `ENCODING_ORCHESTRATOR_CIRCUIT_BREAKER_FAILURE_THRESHOLD` | `5` | Breaker trip threshold |
| `ENCODING_ORCHESTRATOR_CIRCUIT_BREAKER_OPEN_DURATION_MS` | `30000` | Breaker cool-down |
| `AUTHENTICATION_STRATEGY_MODE` | `dev-jwt` | `dev-jwt` or `oidc` — see §7 |
| `OIDC_ISSUER_URL` | dummy, non-functional | OIDC issuer base URL — **must** be overridden in production before `AUTHENTICATION_STRATEGY_MODE=oidc` is used for real traffic |
| `OIDC_CLIENT_ID` | dummy, non-functional | OIDC client id registered with the IdP |
| `OIDC_CLIENT_SECRET` | dummy, non-functional | OIDC client secret — inject via secret manager in production |
| `MUTUAL_TLS_ENFORCEMENT_ENABLED` | `false` | Enables `MutualTlsClientCertificateGuard` — see §8 |
| `MUTUAL_TLS_VERIFIED_CLIENT_CERTIFICATE_HEADER_NAME` | `x-client-cert-verified` | Header the mTLS-terminating proxy sets |
| `INTERNAL_SERVICE_SHARED_SECRET` | dev-only insecure default | Shared secret for `/internal/remediation/*` — see §9 |
| `REMEDIATION_ACTION_ACTIVE_TTL_SECONDS` | `3600` | How long a received remediation action stays "active" in `activeRemediationActions` |

---

## 5. Design patterns applied (explicit, per requirement)

| Pattern | Where | Why |
|---|---|---|
| **Repository** | `asset-registry/repositories/*` | `VideoAssetRepositoryInterface` decouples `AssetRegistryService` from storage; `InMemoryVideoAssetRepository` is the swappable default binding (see `assetRegistryModule.ts`). |
| **Strategy** | `playback-session/drm/*` | One strategy class per DRM system (Widevine/FairPlay/PlayReady/ClearKey), each independently testable and addable without touching existing code. |
| **Factory** | `playback-session/drm/drmSystemSelectionStrategyFactory.ts`, `*/dto/*GraphQlType.ts` (`mapXEntityToGraphQlType` functions) | The DRM factory selects among Strategy instances at runtime; the DTO factory functions centralise entity → GraphQL-type construction. |
| **Circuit Breaker** | `common/circuit-breaker/circuitBreakerUtility.ts`, used by `encoding-orchestration/services/encodingOrchestratorClientService.ts` | Protects the gateway from a struggling/unreachable Temporal deployment; CLOSED/OPEN/HALF_OPEN state machine, unit tested with a fake clock. |
| **Cache-aside** | `redis/manifestCacheAsideService.ts` | Manifest metadata resolution: check Redis, compute + populate on miss. |
| **Dependency Inversion** | `redis/redisClientLike.interface.ts`, `common/auth/oidcRelyingPartyClientLike.interface.ts` | Business logic depends on a narrow interface, not a concrete client (`ioredis`, `openid-client`) — enables in-memory fakes across the unit test suite without live network dependencies. |
| **Strategy (auth)** | `common/auth/authenticationStrategy.interface.ts`, `devJwtAuthenticationStrategy.ts`, `oidcAuthenticationStrategy.ts` | Interchangeable bearer-token verification mechanisms behind one interface — see §7. |
| **Factory (auth)** | `common/auth/authenticationStrategyFactory.ts` | Selects the active `AuthenticationStrategyInterface` implementation from configuration, mirroring `DrmSystemSelectionStrategyFactory`. |

---

## 6. Temporal workflow: `distributedEncodingWorkflow`

Defined in `src/encoding-orchestration/temporal/workflows/distributedEncodingWorkflow.ts`
against the real `@temporalio/workflow` API (`proxyActivities`, `defineQuery`,
`defineSignal`, `setHandler`). DAG:

```
ingestRawVideoAsset
        │
        ▼
generateStructuralMap
        │
        ▼ (fan-out, Promise.all over scenes)
┌───────┴────────┬────────────────┬ ...
▼                ▼                ▼
encodeScene   encodeScene      encodeScene
(scene 0)     (scene 1)        (scene N)
   │              │                │
   ▼              ▼                ▼
selectConvexHull  selectConvexHull selectConvexHull
BitrateLadder     BitrateLadder    BitrateLadder
   └──────┬───────┴────────┬───────┘
          ▼
    packageEncodedAsset
          │
          ▼
    publishPackagedAssetToCdn
```

- **Retry policies**: two `proxyActivities` configurations — a conservative one for
  ingest/structural-map/package/publish (5 attempts, exponential backoff), and a more
  aggressive one for the high-fan-out per-scene encode step (8 attempts) since scene
  jobs are individually cheap to retry.
- **Convex-hull bitrate selection** (`selectConvexHullBitrateLadder`) is a genuine,
  non-simulated implementation of the monotone-chain upper-convex-hull algorithm over
  (bitrate, quality) points — mirroring per-title/per-shot encode-ladder optimisation.
- **Query/Signal**: `distributedEncodingWorkflowProgressQuery` lets the GraphQL layer
  poll live progress without blocking; `cancelRemainingSceneEncodingSignal` supports
  cooperative cancellation of not-yet-started scenes.
- The **client** (`temporalWorkflowClientService.ts`) starts/queries workflows from
  within the NestJS process. The **worker** (`worker/runDistributedEncodingWorker.ts`)
  is a separate, standalone Node.js entry point — this mirrors Temporal's required
  architecture where workers are independent long-running processes, typically an
  entirely separate deployable/fleet from the API gateway.

### Running against real Temporal + Redis

1. `temporal server start-dev` (or point `TEMPORAL_SERVER_FRONTEND_ADDRESS` at a real
   cluster / Temporal Cloud).
2. `npm run start:temporal-worker` in one terminal.
3. `docker run -p 6379:6379 redis:7` for Redis.
4. `npm run start:dev` for the gateway.
5. Call `registerRawVideoAsset`, then `startEncodingForVideoAsset` — this starts a real
   Temporal workflow execution, visible in the Temporal Web UI (`localhost:8233` by
   default with `start-dev`).

---

## 7. Authentication strategy architecture (dev-JWT + OIDC scaffold)

`JsonWebTokenAuthenticationGuard` (§3) remains the guard actually applied to
resolvers today, verifying locally-signed HS256 dev JWTs — this keeps local
development and the existing test suite working unchanged, with zero external
dependencies. Alongside it, this service now ships a documented **Strategy pattern
seam** for hardening authentication to a real OIDC identity provider, mirroring the
`DrmSystemSelectionStrategyFactory` / DRM-strategy pattern used elsewhere:

```
src/common/auth/
├── authenticationStrategy.interface.ts     # AuthenticationStrategyInterface
├── devJwtAuthenticationStrategy.ts         # 'dev-jwt' — wraps JwtService (HS256, shared secret)
├── oidcAuthenticationStrategy.ts           # 'oidc'    — delegates to OidcRelyingPartyClientLike
├── oidcRelyingPartyClientLike.interface.ts # narrow interface an openid-client-backed impl fulfils
└── authenticationStrategyFactory.ts        # picks the active strategy from AUTHENTICATION_STRATEGY_MODE
```

- **`AuthenticationStrategyInterface`** — one method,
  `validateBearerTokenAndResolveAuthenticatedUser(token) => Promise<AuthenticatedUserPayload>`.
  Both strategies implement it identically from a caller's perspective.
- **`DevJwtAuthenticationStrategy`** — real, fully-wired HS256 verification via
  `@nestjs/jwt`'s `JwtService` (the same mechanism `JsonWebTokenAuthenticationGuard`
  uses directly). This is the default (`AUTHENTICATION_STRATEGY_MODE=dev-jwt` or unset).
- **`OidcAuthenticationStrategy`** — verifies tokens by delegating to an injected
  `OidcRelyingPartyClientLike` (Dependency Inversion, same pattern as
  `RedisClientLike`). No implementation of that interface is wired into the running
  application by default: the guard-rail `UnimplementedOidcRelyingPartyClient` is
  bound instead, and **fails closed** (throws) rather than pretending to authenticate
  callers — a stub that silently accepted every token would be a serious security bug.

### What is real vs. scaffolded

| Real & unit-tested (no network required) | Scaffolded / requires production wiring |
|---|---|
| `AuthenticationStrategyFactory` mode selection (`authenticationStrategyFactory.spec.ts`) | OIDC discovery (`openid-client`'s `discover()` against `OIDC_ISSUER_URL`) |
| `DevJwtAuthenticationStrategy` token verification (`devJwtAuthenticationStrategy.spec.ts`) | JWKS-based ID-token signature verification / `/userinfo` calls |
| `OidcAuthenticationStrategy` claims mapping + error handling, against a fake `OidcRelyingPartyClientLike` (`oidcAuthenticationStrategy.spec.ts`) | A concrete `OidcRelyingPartyClientLike` implementation, bound in place of `UnimplementedOidcRelyingPartyClient` |
| Fail-closed behaviour when no real OIDC client is configured | `passport-openidconnect` wired as a Nest `PassportStrategy` for an authorization-code login flow (only needed if this service originates logins itself) |

`passport`, `@nestjs/passport`, `passport-openidconnect`, and `openid-client` are
already installed as real dependencies (see `package.json`) specifically so this
remaining wiring is additive, not a fresh dependency-resolution exercise. To finish
production wiring: implement `OidcRelyingPartyClientLike` using `openid-client`
(`Issuer.discover(oidcIssuerUrl)` once at startup, then JWKS-based verification or a
`/userinfo` call), provide it in `AuthModule`/`AppModule` keyed off
`authenticationStrategyMode === 'oidc'`, and set real `OIDC_ISSUER_URL` /
`OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` — the shipped defaults are dummy,
non-functional placeholders for local dev only (`https://dev-idp.example-invalid.local/oidc`, etc.), never valid for real traffic.

---

## 8. Mutual TLS (mTLS) guidance + proxy-trust guard

`MutualTlsClientCertificateGuard` (`src/common/auth/mutualTlsClientCertificateGuard.ts`)
implements the standard, widely-used pattern for mTLS in front of a service like this
one: **TLS termination and client-certificate cryptographic verification happen at a
reverse proxy or service-mesh sidecar** (nginx, Envoy, Istio/Linkerd), not inside this
Node process. The guard's only job is to trust a header the proxy sets *after* it has
itself verified the client certificate against a trusted CA, and reject any request
that reaches this process without that header — defending against someone bypassing
the proxy and hitting the service directly.

- **Disabled by default** (`MUTUAL_TLS_ENFORCEMENT_ENABLED=false`) so the service
  remains runnable locally with no proxy in front of it.
- When enabled, requires the `x-client-cert-verified` header (configurable via
  `MUTUAL_TLS_VERIFIED_CLIENT_CERTIFICATE_HEADER_NAME`) to equal exactly `SUCCESS`.
- Applied today to `InternalRemediationController` (§9), alongside
  `InternalServiceAuthGuard` and `RedisBackedRateLimitingGuard`, as defence-in-depth;
  it is a no-op guard everywhere else unless the flag is enabled.

### Required nginx configuration (production)

```nginx
server {
  listen 443 ssl;
  ssl_certificate     /etc/nginx/certs/server.crt;
  ssl_certificate_key /etc/nginx/certs/server.key;

  # Require and verify the client certificate against a trusted internal CA.
  ssl_client_certificate /etc/nginx/certs/internal-ca.crt;
  ssl_verify_client on;
  ssl_verify_depth 2;

  location / {
    # Only forwarded when nginx itself verified the presented cert against the CA
    # above ($ssl_client_verify is exactly the string "SUCCESS" on success).
    proxy_set_header x-client-cert-verified $ssl_client_verify;
    proxy_pass http://control-plane-api-gateway:4000;
  }
}
```

### Required Envoy configuration (production, equivalent intent)

```yaml
transport_socket:
  name: envoy.transport_sockets.tls
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
    require_client_certificate: true
    common_tls_context:
      validation_context:
        trusted_ca: { filename: /etc/envoy/certs/internal-ca.crt }
# Then, in the HTTP filter chain, inject the verified-cert header, e.g. via a Lua/
# header-mutation filter that sets `x-client-cert-verified: SUCCESS` only when
# request.sslPeerCertificatePresented() is true — mirroring the nginx snippet above.
```

**What this guard does not do:** parse/validate certificate contents itself (CN/SAN/
expiry — that is the proxy's job), or protect against a misconfigured/bypassed proxy
at the network layer (security-group/service-mesh network isolation is still
required).

---

## 9. Internal remediation webhook contract

`InternalRemediationController` (`src/internal-remediation/internalRemediationController.ts`)
receives automated remediation actions from the **`analytics-telemetry-platform`**
service's telemetry pipelines — this section is the authoritative contract that
service's outbound caller must match.

### Guarding

Both endpoints require, in order:

1. **`InternalServiceAuthGuard`** — the `x-internal-service-shared-secret` header must
   exactly match `INTERNAL_SERVICE_SHARED_SECRET`. This is a deliberately minimal
   placeholder — **replace with mTLS/service-mesh identity (SPIFFE/SPIRE,
   Istio/Linkerd peer authentication) in production**; a static shared secret proves
   *a* caller knows the secret, not *which* service is calling.
2. **`MutualTlsClientCertificateGuard`** (§8) — a no-op unless
   `MUTUAL_TLS_ENFORCEMENT_ENABLED=true`.
3. **`RedisBackedRateLimitingGuard`** — the same Redis-backed limiter every GraphQL
   resolver uses, reused here so a retry-loop bug in an internal caller can't
   overwhelm this service either.

### `POST /internal/remediation/rollback-experiment`

Sent when the analytics-telemetry-platform's experiment health-check pipeline
determines an in-flight experiment/feature-flag is degrading playback quality and
should be rolled back.

```json
{
  "experimentId": "exp-abc123",
  "reason": "Elevated rebuffer ratio (+18%) detected in treatment cohort over the last 15 minutes.",
  "verdict": "ELEVATED_REBUFFER_RATIO"
}
```

`verdict` is one of `NEGATIVE_GUARDRAIL_BREACH | ELEVATED_REBUFFER_RATIO |
DRM_NEGOTIATION_FAILURE_SPIKE | MANUAL_OPERATOR_OVERRIDE | OTHER` (see
`src/internal-remediation/dto/rollbackExperimentRequestBody.ts` —
`OTHER` is a forward-compatible escape hatch for verdicts introduced upstream before
this DTO is updated to know about them).

Response (`202 Accepted`):

```json
{
  "accepted": true,
  "remediationActionIdentifier": "5e1b1f2e-...-uuid",
  "activeUntilIsoTimestamp": "2026-08-11T13:00:00.000Z"
}
```

### `POST /internal/remediation/manifest-filter-rule`

Sent when the analytics-telemetry-platform's device-cohort-degradation-detection
pipeline determines a specific device cohort needs a mitigating manifest-resolution
rule applied.

```json
{
  "deviceCohortMatcher": "platform:ios;appVersion:<4.2.0",
  "action": "CAP_BITRATE_LADDER",
  "reason": "Sustained rebuffering on iOS app versions below 4.2.0 following a player regression."
}
```

`action` is one of `CAP_BITRATE_LADDER | FORCE_CLEARKEY_DRM |
BLOCK_MANIFEST_RESOLUTION | PIN_LAST_KNOWN_GOOD_MANIFEST_VERSION | OTHER` (see
`src/internal-remediation/dto/manifestFilterRuleRequestBody.ts`). `deviceCohortMatcher`
is stored verbatim (opaque to this receiver) for a future manifest-resolution filter
chain to interpret.

Response shape: identical `202 Accepted` body to the rollback-experiment endpoint.

### What happens on receipt

1. A structured `RemediationActionReceivedEvent` is logged (`actionType`,
   `remediationActionIdentifier`, `reason`, `receivedAtIsoTimestamp`) — see
   `src/internal-remediation/events/remediationActionReceivedEvent.ts`.
2. The action is recorded in the in-memory `ActiveRemediationActionsRegistry`
   (`src/internal-remediation/registry/activeRemediationActionsRegistry.ts`), tagged
   with an expiry timestamp `receivedAt + REMEDIATION_ACTION_ACTIVE_TTL_SECONDS`
   (default 1 hour). Expired records are lazily excluded from every read — no
   background sweep timer is needed for correctness.
3. Active actions are queryable via the GraphQL `activeRemediationActions` query
   (rate-limited like every other resolver):

```graphql
query {
  activeRemediationActions {
    remediationActionIdentifier
    actionType
    reason
    receivedAtIsoTimestamp
    expiresAtIsoTimestamp
    experimentId
    verdict
    deviceCohortMatcher
    filterAction
  }
}
```

**Known limitation:** the registry is in-memory and per-process — it resets on
restart and is not shared across horizontally-scaled replicas. A production rollout
serving more than one replica should back it with Redis (this service already
depends on `ioredis`) or a shared datastore, following the same seam
`VideoAssetRepositoryInterface` demonstrates for the asset registry.

---

## 10. Performance targets (SLOs — **targets, not measured benchmarks**)

These are architectural targets for a production deployment of this service pattern,
**not** numbers measured in this sandbox (no load-testing infrastructure or live
Redis/Temporal cluster is available here). They are included to document the
performance bar this design is intended to meet:

| Resolver | p50 target | p95 target | p99 target | Notes |
|---|---|---|---|---|
| `videoAsset` lookup (Redis-cached path) | < 10ms | < 30ms | < 50ms | Assumes asset metadata cache-aside hit; DB-backed cold path would be higher. |
| `allVideoAssets` | < 20ms | < 60ms | < 120ms | Unbounded list query; production should paginate. |
| `createPlaybackSession` | < 40ms | < 80ms | < 120ms | Includes one Redis write + DRM strategy selection (in-process, sub-millisecond). |
| `resolvePersonalizedManifestUrlForPlaybackSession` (cache hit) | < 5ms | < 15ms | < 50ms | Single Redis `GET`. |
| `resolvePersonalizedManifestUrlForPlaybackSession` (cache miss) | < 60ms | < 150ms | < 300ms | Bound by the simulated/real packaging-service lookup. |
| `startEncodingForVideoAsset` | < 50ms | < 150ms | < 400ms | Bound by Temporal `WorkflowClient.start()` round trip; circuit breaker adds negligible overhead when CLOSED. |

**Actual measured numbers from this build** (real, not targets):

- `npm run build`: succeeded, no errors (see §11).
- `npm test`: **54 tests passed across 11 suites** (the original 22 across 4 suites,
  plus 32 new tests across 7 new suites added for §7-§9: authentication strategy
  selection/verification, the mTLS guard, the internal-service shared-secret guard,
  the remediation registry, and the internal remediation controller).
- `npm run test:e2e`: **1/1 passed** (full `AppModule` dependency graph, including the
  new `InternalRemediationModule`, still compiles and resolves cleanly).
- No load testing or live-traffic latency measurement was performed in this sandbox.

---

## 11. What was actually verified to run

Node **v24.12.0** / npm **11.6.2** were available in this environment. The following
were **actually executed**, not just written:

- `npm install` — completed successfully, all dependencies (including
  `@temporalio/client`, `@temporalio/workflow`, `@temporalio/worker`,
  `@temporalio/activity`, `ioredis`, `@nestjs/graphql`, `@nestjs/apollo`) installed
  without native build failures.
- `npx tsc --noEmit` — zero type errors across the full `src/` tree.
- `npm run build` (`nest build`) — succeeded, emitted `dist/`.
- `npm test` (Jest via `ts-jest`) — **54/54 tests passed across 11 suites**: the
  original 22 (circuit breaker state machine, Redis rate limiter, manifest cache-aside,
  DRM strategy factory) plus 32 new tests added for the auth-strategy scaffold, mTLS
  guard, and internal remediation webhook (§7-§9) — `authenticationStrategyFactory.spec.ts`,
  `devJwtAuthenticationStrategy.spec.ts`, `oidcAuthenticationStrategy.spec.ts`,
  `mutualTlsClientCertificateGuard.spec.ts`, `internalServiceAuthGuard.spec.ts`,
  `activeRemediationActionsRegistry.spec.ts`, `internalRemediationController.spec.ts`.
- `node dist/main.js` — the compiled application **actually booted**: NestJS wired
  every module (`ApplicationConfigurationModule`, `RedisModule`, `JwtModule`,
  `AssetRegistryModule`, `EncodingOrchestrationModule`, `PlaybackSessionModule`,
  `GraphQLModule`), generated `src/graphql/schema.gql` from the code-first resolvers,
  and successfully bound the HTTP/GraphQL listener on port 4000. Redis connection
  attempts failed with `ECONNREFUSED` as expected (no Redis server running in this
  sandbox) — this confirms the service degrades to "GraphQL up, Redis-dependent
  operations will fail until Redis is reachable" rather than crash-looping, and is
  the expected behavior documented in §4.
- `npm run test:e2e` — the smoke test in `test/appModule.e2e-spec.ts` **passed**
  (compiles the entire `AppModule` dependency graph via Nest's testing module
  builder). Jest reports it "did not exit cleanly" afterwards because the `ioredis`
  client keeps retrying its connection in the background with no Redis server
  present in this sandbox — expected here, and not present when a real Redis is
  reachable.
- Temporal worker (`runDistributedEncodingWorker.ts`) and an actual live
  `distributedEncodingWorkflow` execution were **not** run end-to-end in this sandbox
  (no Temporal server available here) — the code is written against the real
  `@temporalio/*` APIs and type-checks/builds cleanly, but live execution against a
  running Temporal cluster has not been exercised. See §12.

---

## 12. Known limitations / what's simulated vs. production-real

**Simulated / stubbed (clearly marked in source with "SIMULATED" doc comments):**

- All Temporal **activities** (`distributedEncodingActivities.ts`) simulate ingest,
  structural-map generation, per-scene encoding, packaging, and publishing with timed
  delays and synthetic data instead of calling a real transcode farm/packager/CDN.
  The **convex-hull bitrate selection algorithm itself is real**, just operating on
  synthetic quality scores.
- `computeAuthoritativeManifestMetadata` in `playbackSessionOrchestrationService.ts`
  fabricates a manifest URL/bitrate ladder instead of calling a real packaging/origin
  service.
- `ClearKeyDrmSystemSelectionStrategy` provides no real content protection — it exists
  as the universal fallback strategy for local dev/testing only.
- The asset repository is **in-memory** (`InMemoryVideoAssetRepository`) and is lost on
  process restart; the `VideoAssetRepositoryInterface` seam is production-ready but the
  concrete database-backed implementation (Postgres/DynamoDB/etc.) is not included.
- JWT authentication uses a symmetric HS256 secret with no real user/identity backing
  store — production should use an OIDC/JWKS-validating strategy (RS256, key rotation)
  per the "keep it clean and pluggable" scope note.
- Rate limiting is **fixed-window**, which can admit up to ~2x the configured limit
  across a window boundary burst; a sliding-window or token-bucket algorithm would be
  a more precise (but more complex) production choice.
- `OidcAuthenticationStrategy` (§7) is bound to `UnimplementedOidcRelyingPartyClient`
  by default — it fails closed (rejects every token) rather than performing a real
  JWKS-based verification. `AUTHENTICATION_STRATEGY_MODE=oidc` should not be set in
  any environment accepting real traffic until a real `OidcRelyingPartyClientLike`
  implementation is wired in (see §7 for the exact remaining steps).
- `MutualTlsClientCertificateGuard` (§8) trusts a proxy-set header; it performs no
  certificate parsing itself and provides no protection if the proxy is bypassed at
  the network layer — real network-level isolation in front of this service is still
  required in production.
- `InternalServiceAuthGuard` (§9) uses a single static shared secret, not per-caller
  identity — a real production rollout should move to mTLS peer-certificate identity
  or service-mesh identity (SPIFFE/SPIRE), as documented in the guard's TSDoc.
- `ActiveRemediationActionsRegistry` (§9) is in-memory and per-process, like
  `InMemoryVideoAssetRepository` — it does not survive a restart and is not shared
  across horizontally-scaled replicas.
- No live handshake with an OIDC identity provider, an nginx/Envoy mTLS-terminating
  proxy, or the real `analytics-telemetry-platform` service was performed in this
  sandbox — all three integration points are exercised via unit tests against fakes/
  mocks (§7-§9), not live network calls, per the task's explicit scope.

**Production-real (not simulated):**

- The NestJS module structure, GraphQL schema generation, dependency injection wiring.
- The Circuit Breaker state machine (`circuitBreakerUtility.ts`).
- The Redis-backed fixed-window rate limiter's actual counting logic against real
  Redis commands (`INCR`/`EXPIRE`/`TTL`) — only the *server* is absent in this sandbox.
- The cache-aside manifest caching logic itself.
- The `distributedEncodingWorkflow` DAG structure, retry policy configuration, and
  query/signal handlers, written against real `@temporalio/workflow` /
  `@temporalio/client` / `@temporalio/worker` APIs — this would run correctly against
  a real Temporal server without code changes, only environment configuration.
- The DRM Strategy/Factory selection logic.
- `DevJwtAuthenticationStrategy`'s HS256 verification, `AuthenticationStrategyFactory`'s
  mode-selection logic, `MutualTlsClientCertificateGuard`'s header-trust enforcement,
  `InternalServiceAuthGuard`'s shared-secret check, and
  `ActiveRemediationActionsRegistry`'s record/list/TTL-expiry logic — all real,
  unit-tested business logic (§7-§9), independent of any live external system.
