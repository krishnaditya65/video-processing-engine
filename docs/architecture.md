# System Architecture

## The Four Architectural Planes

```
┌─────────────────────────────────────────────────────────────────────────┐
│  1. INGESTION & INSPECTION PLANE                                         │
│     services/ingestion-inspection-service (Python)                       │
│                                                                            │
│     Raw ProRes/mezzanine asset                                           │
│        │  ffprobe metadata extraction                                    │
│        ▼                                                                  │
│     Chain of Responsibility pipeline:                                    │
│       BlackFrameDetectionStage                                           │
│         → SceneCutDetectionStage (SSIM / histogram-diff Strategy)        │
│           → ColorSpaceExtractionStage (BT.709/BT.2020/HDR10/DV heuristic)│
│             → LoudnessAnalysisStage (EBU R128-style LUFS estimate)       │
│        │                                                                  │
│        ▼                                                                  │
│     StructuralMap JSON  (scene boundaries, complexity scores,            │
│                           color-space profile, loudness result)          │
└─────────────────────────────┬─────────────────────────────────────────────┘
                               │ StructuralMap
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  2. DISTRIBUTED ENCODING FACTORY                                         │
│     services/distributed-encoding-orchestrator (Go)                      │
│                                                                            │
│     Fan out scene boundaries → ChunkEncodingTask[]                       │
│        │                                                                  │
│        ▼                                                                  │
│     Dispatcher → JobQueue → WorkerPool (bounded goroutine concurrency)   │
│        │            each worker:                                         │
│        │              CodecArgumentBuilder Strategy (H264/HEVC/AV1)      │
│        │              → SubprocessRunner (ffmpeg) with progress parsing  │
│        │              → FfmpegFailureCircuitBreaker + exponential backoff│
│        ▼                                                                  │
│     Per-scene (bitrate, VMAF) sample sets                                │
│        │                                                                  │
│        ▼                                                                  │
│     computeUpperConvexHullFromRateDistortionSamples()                    │
│        → Pareto-optimal bitrate ladder per scene                         │
│        │                                                                  │
│        ▼                                                                  │
│     A/B forensic watermark scheduling (deterministic hash-chain PRNG)    │
│        → per-chunk Variant A/B assignment                                │
└─────────────────────────────┬─────────────────────────────────────────────┘
                               │ Elementary streams (video/audio tracks) → origin storage
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  3. EDGE ROUTING & PACKAGING ENGINE                                      │
│     services/edge-packaging-and-manifest-service (Go)                    │
│                                                                            │
│     GET /manifest/{assetId}  (PlaybackContext: device, network, DRM)     │
│        │                                                                  │
│        ▼                                                                  │
│     Chain of Responsibility variant-ladder filtering                     │
│       (codec support → HDR support → resolution cap → network hint)      │
│        │                                                                  │
│        ▼                                                                  │
│     SSAI ad-break splicing (SCTE-35-style cue points)                    │
│        │                                                                  │
│        ▼                                                                  │
│     DrmKeyProvider Strategy (Widevine/FairPlay/PlayReady)                │
│       + deterministic key rotation every N segments                      │
│        │                                                                  │
│        ▼                                                                  │
│     ManifestFormatGenerator Strategy → HLS (.m3u8) / DASH (.mpd)         │
│                                                                            │
│     GET /package/{assetId}/{chunkId}                                     │
│        → JIT fMP4 muxer with real AES-128-CTR mdat-payload encryption    │
│        → served over both HTTP/1.1 and real HTTP/3-over-QUIC listeners   │
│     Optional RecentQoeSignal input downgrades the offered variant        │
│     ceiling for a struggling session (hook point for predictive-qoe-ml)  │
└─────────────────────────────┬─────────────────────────────────────────────┘
                               │ playback request routed to the best-scoring CDN
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  3b. MULTI-CDN ROUTING & PREDICTIVE PRE-FETCH                            │
│      services/multi-cdn-edge-routing-service (Go)                        │
│                                                                            │
│      GET /route/{assetId}                                                │
│        → WeightedCdnHealthScoringStrategy over mock CloudFront/Akamai/   │
│          Fastly health reports (latency/error-rate/cost), automatic      │
│          failover with deterministic tie-break                          │
│                                                                            │
│      GET /prefetch-predictions/{userId}                                  │
│        → ViewingPatternPredictor: circular-mean/stddev over a user's     │
│          historical viewing-session time-of-day (midnight-safe)          │
│        → CachePreWarmCommand emitted ahead of the predicted next session │
└─────────────────────────────┬─────────────────────────────────────────────┘
                               │ Playback beacons (TTFF, rebuffer events, bitrate switches, decode errors)
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  4. PREDICTIVE ANALYTICS & TELEMETRY LAKE                                │
│     services/analytics-telemetry-platform (TypeScript)                   │
│     services/predictive-qoe-ml-service (Python)                          │
│                                                                            │
│     Kafka/Redpanda consumer                                              │
│        │                                                                  │
│        ▼                                                                  │
│     Pipeline: validate (Zod) → normalize → enrich → batch insert         │
│        │                                                                  │
│        ▼                                                                  │
│     ClickHouse MergeTree (ORDER BY assetId, toStartOfHour(eventTimestamp))│
│        │                                                                  │
│        ├─→ QoE metrics: rebuffer ratio, TTFF p50/p95/p99, avg bitrate    │
│        ├─→ Firmware-cohort anomaly detection (z-score vs rolling baseline)│
│        └─→ Encoding A/B experiment evaluator (Welch's t-test)            │
│                                                                            │
│     ── separately ──                                                     │
│     predictive-qoe-ml-service:                                           │
│       rolling network-jitter features → IsolationForest → prediction     │
│       → WebSocket downshift command pushed to client player              │
│         BEFORE the buffer empties                                        │
│                                                                            │
│     ── closed-loop remediation (new) ──                                  │
│     EncodingExperimentAutoRemediationService                             │
│       → POST {controlPlane}/internal/remediation/rollback-experiment    │
│     ManifestFilterRuleEmitter (on confirmed firmware-cohort anomaly)     │
│       → POST {edgePackaging}/internal/manifest-filters/rules            │
│     (both: real outbound webhook callers, retry-with-backoff)           │
└─────────────────────────────┬─────────────────────────────────────────────┘
                               │ session/asset control, cache, workflow state, remediation actions
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  CONTROL PLANE & API GATEWAY                                             │
│     services/control-plane-api-gateway (NestJS + GraphQL)                │
│                                                                            │
│     GraphQL BFF aggregating: asset registry, playback session creation,  │
│     manifest URL resolution (Redis cache-aside), DRM system negotiation  │
│        │                                                                  │
│        ▼                                                                  │
│     distributedEncodingWorkflow (Temporal DAG):                          │
│       ingest → structural map → fan-out scene encodes                    │
│         → convex-hull select → package → publish                        │
│       (state persistence + automatic retry on worker crash)              │
│                                                                            │
│     AuthenticationStrategy (Strategy): DevJwt | Oidc, selected via config │
│     MutualTlsClientCertificateGuard: trusts proxy-terminated mTLS header │
│     InternalRemediationController: receives the two webhooks above,      │
│       guarded by InternalServiceAuthGuard, tracked in an                 │
│       ActiveRemediationActionsRegistry, queryable via GraphQL            │
└─────────────────────────────────────────────────────────────────────────┘
```

## Why This Decomposition

Each plane has a fundamentally different resource profile and failure mode,
which is why they are separate deployable services rather than a monolith:

- **Ingestion & Inspection** is CPU-bound, bursty (only runs once per asset
  upload), and Python's ecosystem (OpenCV/NumPy) is the right tool.
- **Encoding Factory** is the most compute-intensive plane by orders of
  magnitude (thousands of concurrent ffmpeg subprocesses) and needs Go's
  lightweight concurrency model to orchestrate a large worker fleet
  cheaply.
- **Edge Packaging** must be the lowest-latency plane in the system — it
  runs on every single playback request — so it is a small, dependency-light
  Go binary that can be deployed to edge PoPs.
- **Analytics/Telemetry** is a completely different scaling axis: extremely
  high write throughput (millions of small events/sec) against a columnar
  store, decoupled from playback via a stream broker so a slow analytics
  pipeline never blocks a user's stream.
- **Control Plane** is the one place state and orchestration logic belong —
  it is the only service that talks to Temporal, and it is intentionally
  kept out of the hot playback path (manifest resolution is cached via
  Redis cache-aside specifically so the control plane is not a bottleneck).
  It is also the natural home for the internal remediation webhook receiver,
  since it already owns cross-service state.
- **Multi-CDN Routing** is a separate small service from edge packaging
  because it answers a fundamentally different question ("which network
  path" vs. "what bytes") on a different cache/update cadence (CDN health
  reports refresh continuously; manifest personalization is per-request).

## Data Flow Contracts Between Services

| Producer | Artifact | Consumer |
|---|---|---|
| `ingestion-inspection-service` | `StructuralMap` JSON (scene boundaries, color/HDR profile, loudness) | `distributed-encoding-orchestrator`, `control-plane-api-gateway` (via Temporal activity) |
| `distributed-encoding-orchestrator` | Elementary video/audio streams + per-scene bitrate ladder + `TitleCostReport` | Origin object storage (MinIO/S3) → `edge-packaging-and-manifest-service`; cost report → `docs/cost-dashboard.md` pipeline |
| `edge-packaging-and-manifest-service` | Personalized, AES-128-CTR-encrypted HLS/DASH manifest + JIT-packaged fMP4 chunks over HTTP/1.1 and HTTP/3 | Client player |
| `multi-cdn-edge-routing-service` | CDN routing decision, `CachePreWarmCommand` | Client player redirect; edge cache warmers |
| Client player | Playback beacons (TTFF, rebuffer, bitrate switch, decode error) | `analytics-telemetry-platform` (via Redpanda) |
| `analytics-telemetry-platform` | `AnomalyDetectedEvent` → `ManifestFilterRule` webhook; A/B rollback verdict webhook | `edge-packaging-and-manifest-service` (filter rule); `control-plane-api-gateway` (rollback, via `InternalRemediationController`) |
| `predictive-qoe-ml-service` | Bandwidth-degradation prediction | WebSocket downshift command → client player |

## What Is Production-Real vs. Simulated

This is stated precisely per service in each README, but the general rule
across the system:

- **Real and tested**: all pure algorithmic logic — SSIM scene detection,
  convex-hull and per-title bitrate optimization, ISO-BMFF box construction,
  AES-128-CTR fMP4 payload encryption (bit-for-bit round-trip verified),
  HTTP/3-over-QUIC request/response (real `quic-go` server, genuine
  round-trip test), DRM signaling formats, QoE metric math, anomaly
  z-scores, Welch's t-test, Isolation Forest training/inference, Pearson
  churn correlation, circular-mean time-of-day statistics, weighted CDN
  health scoring, rate limiting, circuit breakers, cache-aside logic,
  manifest generation, SSAI splicing, checkpoint-resume (simulated
  preemption proven data-loss-free), encode cost accounting.
- **Structurally real, not run against live infrastructure in this
  environment**: Temporal workflow execution against a live Temporal
  server, Kafka/Redpanda consumption against a live broker, ClickHouse
  inserts against a live cluster, real ffmpeg subprocess invocation against
  real video files, real DRM license-server integration, real OIDC
  handshake against a live IdP, the two remediation webhooks delivered
  end-to-end (each side is tested against a fake of the other). All of this
  code is written against the real client SDKs/APIs and compiles/type-checks
  correctly; `infra/docker-compose.yml` (now including Prometheus + Grafana)
  provides the infrastructure needed to exercise it end-to-end.
- **Explicitly out of scope, documented as needing external tooling**:
  HDR10+/Dolby Vision *dynamic* metadata multiplexing needs `hdr10plus_tool`/
  `dovi_tool` as separate processing steps outside this codebase; BBR-style
  congestion control for QUIC would require forking `quic-go`'s unexported
  `internal/congestion` package (verified, not assumed).
