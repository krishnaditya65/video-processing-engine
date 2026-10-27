# Multi-Region Disaster Recovery Design

This document designs an active-active multi-region topology for the
platform: control-plane availability, ClickHouse telemetry replication,
origin object-storage replication, and the RPO/RTO targets and failover
procedure that follow from them. It is a design document — no
multi-region infrastructure was stood up or exercised in this
environment (this repository's `infra/docker-compose.yml` is explicitly
a single-node development topology; see its header comment). Every design
decision below is grounded in real components/files in this codebase
rather than generic boilerplate.

## Why active-active, and at which layer

Per `docs/architecture.md`, the four planes have very different state
profiles:

| Plane | State | Multi-region strategy |
|---|---|---|
| Ingestion & Inspection | Stateless per request (produces a `StructuralMap` artifact, then exits) | **Active-active trivially** — run in every region, no coordination needed |
| Encoding Factory | Stateless per job beyond in-flight `WorkerPool` state (`internal/worker`) + the new `JobCheckpointStore` (`internal/checkpoint`) | **Active-active with region-local job assignment** — a job started in one region completes in that region; checkpoint state does not need cross-region replication because a failed region's in-flight jobs are simply re-submitted (idempotent at the job level) |
| Edge Packaging & Manifest | Stateless per request (reads from origin storage + a static device-capability catalog) | **Active-active trivially, deployed to every PoP** — this is explicitly the point of this plane per `docs/architecture.md`: "the lowest-latency plane in the system... a small, dependency-light Go binary that can be deployed to edge PoPs" |
| Control Plane & API Gateway | Session state in Redis, workflow state in Temporal | **Active-active with per-region Redis + a single active Temporal cluster (see below)** |
| Analytics/Telemetry | ClickHouse OLAP store, Redpanda/Kafka broker | **Active-active ingestion, async cross-region ClickHouse replication** |

The two genuinely hard cases are the **control plane's Temporal
dependency** and **ClickHouse replication** — everything else in the
system is either stateless or has state that's cheap to lose and
re-derive (Redis session cache, checkpoint state for an in-flight job).

## Control plane: active-active with a single active Temporal cluster

`control-plane-api-gateway` is "the only service that talks to Temporal"
(`docs/architecture.md`), and it is "intentionally kept out of the hot
playback path" — manifest resolution is Redis cache-aside specifically so
control-plane blips don't stall active playback (per the same doc, and
`docs/slo-sli-sla.md`'s availability table: 99.9% for control plane vs.
99.99% for edge packaging).

Design:

- **GraphQL BFF layer**: deploy `control-plane-api-gateway` NestJS
  instances active-active in every region, behind a global anycast/GeoDNS
  load balancer. Each region's instances connect to a **region-local**
  Redis (`redisSessionAndCacheStore`) for session state and the manifest
  cache-aside layer (`services/control-plane-api-gateway/src/redis/manifestCacheAsideService.ts`)
  — session state does not need cross-region replication because a
  client's playback session is pinned to the region it was created in via
  session-affinity routing at the load balancer (a session created in
  `us-east-1` should not need to fail over to `eu-west-1` mid-playback;
  if the region fails, the client re-establishes a new session in a
  healthy region, which `createPlaybackSession`
  (`playbackSessionResolver.ts`) already supports as a normal operation,
  not a special DR code path).
- **Temporal**: run **one active Temporal cluster per geography**
  (e.g. US, EU) rather than one truly global cluster, each backed by its
  own regional `temporalWorkflowServerDatabase` (Postgres) with
  synchronous replicas within the geography and async cross-geography
  replication for DR purposes only. `distributedEncodingWorkflow`
  instances are geography-pinned to wherever the ingest request landed;
  they are not migrated mid-flight across geographies on failover — a
  failed geography's in-flight workflows are resumed from Temporal's own
  durable workflow history once that geography's Temporal cluster and
  Postgres are restored (this is exactly what Temporal's persistence
  layer is designed for, and is why `docs/architecture.md` calls out
  "state persistence + automatic retry on worker crash" as the reason
  this system uses Temporal in the first place — the same durability
  guarantee extends to a full control-plane-instance failure, not just a
  single worker crash).
- **Why not a single global Temporal cluster**: Temporal's own
  operational guidance is that cross-region synchronous replication for a
  single cluster adds workflow-decision latency proportional to
  inter-region RTT on every workflow event — acceptable for infrequent,
  long-running DAGs in principle, but this system's
  `distributedEncodingWorkflow` fans out to potentially thousands of
  `ChunkEncodingTask` activities per job (see `docs/slo-sli-sla.md`'s "2-hour
  film → 3,600 two-second chunks" throughput math), so per-geography
  clusters avoid that fan-out becoming cross-region-latency-bound.

## ClickHouse replication strategy

`analytics-telemetry-platform`'s ClickHouse schema
(`services/analytics-telemetry-platform/sql/schema.sql`) uses a
`MergeTree` engine with `ORDER BY (assetId, toStartOfHour(eventTimestamp))`
per `docs/slo-sli-sla.md` §5's schema-design rationale. For multi-region
DR:

- **Within a region**: switch the table engine from plain `MergeTree` to
  `ReplicatedMergeTree`, backed by a region-local ClickHouse Keeper
  (or ZooKeeper) ensemble — this is the standard ClickHouse HA pattern
  and requires no change to the `ORDER BY` key or any application code in
  `analytics-telemetry-platform`, since `ReplicatedMergeTree` is
  API-compatible with `MergeTree` for inserts/selects.
- **Cross-region**: each region's `PlaybackBeaconKafkaConsumer`
  (`services/analytics-telemetry-platform/src/ingestion/kafkaConsumer.ts`)
  consumes from its **region-local** Redpanda broker (beacons are
  produced by clients to whichever region they're connected to — no
  cross-region beacon shipping at ingest time, to keep ingestion latency
  low and avoid making the hot beacon-ingestion path depend on
  inter-region network health). Cross-region durability instead comes
  from **`clickhouse-copier`-style asynchronous batch replication** (or a
  scheduled `INSERT ... SELECT` via `remote()`/`remoteSecure()` table
  functions) running on a fixed interval (target: every 5 minutes) that
  copies each region's newly-inserted rows to every other region's
  ClickHouse cluster, keyed by the same `(assetId,
  toStartOfHour(eventTimestamp))` ordering so the copy job can efficiently
  identify new partitions.
- **Why async, not synchronous, cross-region**: `docs/slo-sli-sla.md` §5
  explicitly documents "graceful beacon-loss tolerance" as an accepted
  tradeoff ("some beacon loss under extreme load is acceptable, sustained
  data-lake unavailability is not") — the same philosophy extends
  naturally to cross-region replication lag: a QoE dashboard being 5
  minutes stale after a regional failover is acceptable; blocking
  beacon ingestion on synchronous cross-region ClickHouse writes (which
  would turn a 2,000,000 beacons/sec target into a cross-region-RTT-bound
  number) is not.

## Origin storage cross-region replication

`minioOriginObjectStorage` is the origin store for elementary
video/audio streams produced by `distributed-encoding-orchestrator` and
read by `edge-packaging-and-manifest-service`'s JIT packager
(`docs/architecture.md`'s data-flow-contracts table). Design:

- Each region runs its own MinIO (or equivalently, a real deployment
  would use a managed multi-region object store — S3 Cross-Region
  Replication, GCS multi-region buckets, etc. — MinIO here stands in for
  "an S3-API-compatible object store" per `infra/docker-compose.yml`'s
  own framing).
- **Write-once, replicate-async**: elementary streams are immutable once
  written (an encode job produces them once; they are never mutated in
  place — the `distributed-encoding-orchestrator`'s A/B forensic
  watermark scheduling produces distinct per-chunk variant assets rather
  than mutating a shared asset, per `docs/architecture.md`), which makes
  this the easiest replication case in the whole design: bucket-level
  async cross-region replication (MinIO's own bucket replication
  feature, or S3 CRR in a real deployment) with no conflict-resolution
  logic needed at all, since there are never two regions writing to the
  same object key.
- **Read-path fallback**: `edge-packaging-and-manifest-service` instances
  in region B should read from region B's local origin store first, and
  fall back to region A's store (cross-region read, higher latency but
  correct) if the requested `assetId`'s elementary streams have not yet
  replicated to region B — this bounds the "a title just finished
  encoding in region A" window's user-facing impact to "temporarily
  higher manifest-to-first-chunk latency in region B" rather than a hard
  404, at the cost of a fallback code path not present in the current
  single-region `internal/httpapi/httpapi.go` handler (an authored design
  choice for a future multi-region build, not implemented here).

## RPO / RTO targets

| System | RPO (max acceptable data loss) | RTO (max acceptable time to recover) | Rationale |
|---|---|---|---|
| Control-plane session state (Redis) | Full loss acceptable (0 retention target) | < 30s (new region picks up new session creates immediately) | Sessions are cheap to re-create; `docs/slo-sli-sla.md` already treats control-plane availability as 99.9%, not 99.99% — some session churn on failover is within the existing error budget |
| Temporal workflow state | 0 (durable persistence within a geography; async cross-geography backup only for full-geography-loss DR, not routine failover) | < 15 min for full geography restoration from backup, in the rare full-geography-loss case; effectively 0 for single-instance failures (Temporal's own HA) | `distributedEncodingWorkflow`'s whole reason for existing is durable retry — DR design should not weaken that guarantee for the common case |
| ClickHouse telemetry (cross-region copy) | ≤ 5 min (matches the async replication interval above) | < 10 min (re-point `analytics-telemetry-platform`'s query-side connections at a healthy region's ClickHouse cluster) | Matches the "graceful beacon-loss tolerance" SLA philosophy already in `docs/slo-sli-sla.md` §5 |
| Origin object storage (elementary streams) | ≤ 15 min (matches typical async bucket-replication lag for large objects) | < 5 min (edge-packaging's cross-region read fallback bridges the gap live, so RTO here is really "how fast does traffic get re-routed to a healthy region's edge-packaging instances", which is a load-balancer health-check interval, not a data-restoration operation) | Immutable, write-once objects make this the cheapest RPO/RTO pair in the system — see "Origin storage" section above |
| Edge packaging & manifest generation | N/A (stateless) | < 30s (load-balancer health-check + DNS/anycast convergence) | 99.99% availability target in `docs/slo-sli-sla.md` demands the fastest failover of any plane, achievable specifically because this plane carries no state |

## Failover procedure

1. **Detection**: regional health-check failures (edge-packaging
   `/healthz` — already implemented,
   `services/edge-packaging-and-manifest-service/internal/httpapi/httpapi.go`'s
   `GET /healthz` route — plus Prometheus alerting on the RED-metric error
   rate per region, per `infra/observability/README.md`'s methodology)
   cross an alerting threshold for a sustained window (avoid failing over
   on a transient blip).
2. **Traffic**: the anycast/GeoDNS layer stops routing new client
   connections to the failed region; in-flight playback sessions in the
   failed region degrade per each plane's own resilience mechanism
   (edge-packaging is stateless so its failure is immediately visible as
   connection failures the client's own ABR/retry logic handles; control
   -plane session loss surfaces as a session the client must re-create,
   exactly the `docs/testing-and-benchmarking.md`-documented
   "`ECONNREFUSED` without crashing" pattern generalized to a whole-region
   failure).
3. **Control plane**: if the failure is geography-wide (not just one
   instance), the affected geography's Temporal cluster is restored from
   its most recent durable state (target RTO above); `control-plane-api-gateway`
   instances in the healthy geography continue serving all traffic in the
   interim — this is the "active-active" property: the healthy region
   does not wait on the failed region's recovery to keep serving its own
   users.
4. **Analytics**: `analytics-telemetry-platform` instances in the healthy
   region(s) are unaffected (region-local Redpanda + region-local
   `ReplicatedMergeTree`); QoE dashboard queries are re-pointed at a
   healthy region's ClickHouse cluster if the query layer itself was
   region-pinned, accepting up to the 5-minute cross-region replication
   RPO for any rows that hadn't yet copied out of the failed region.
5. **Origin storage**: edge-packaging in every healthy region continues
   serving already-replicated assets normally; the cross-region read
   fallback (see above) covers assets that were mid-replication out of
   the failed region at the moment of failure.
6. **Failback**: once the failed region is restored, it rejoins
   active-active rotation only after (a) its Temporal cluster has caught
   up on durable workflow state, (b) its ClickHouse replica has caught up
   to the other regions' latest replicated partitions, and (c) its origin
   store's replication backlog has drained — gated health checks, not a
   time-based guess, to avoid re-admitting a region that looks "up" but
   is still catching up on state.

## What this document does not cover (explicitly out of scope)

- Concrete Terraform/Kubernetes manifests for any of the above — this is
  an architecture document, not an infrastructure-as-code deliverable.
  See "Requirements" in the top-level task this document was produced
  under for why: `infra/` in this repository is explicitly a
  single-node development topology, and multi-region IaC was not part of
  this task's scope.
- A specific choice of cloud provider / managed-service equivalents for
  MinIO, ClickHouse Keeper, or Temporal Cloud vs. self-hosted Temporal —
  those are deployment-environment decisions orthogonal to this design's
  region-topology reasoning.
- Real RPO/RTO numbers measured against a live multi-region deployment —
  the table above states engineering targets, in the same spirit as
  `docs/slo-sli-sla.md`'s explicit `TARGET` vs. `MEASURED` distinction;
  none of these targets have been measured in this environment.
