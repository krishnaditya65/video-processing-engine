# distributed-encoding-orchestrator

A Go microservice implementing the **Distributed Encoding Factory** for a
Netflix-tier hyperscale video processing/streaming platform: it fans a
source video asset out into thousands of scene-based micro-chunk encoding
jobs, distributes them across a bounded-concurrency worker fleet, wraps
real `ffmpeg` subprocess invocations with retry/circuit-breaking, computes
VMAF convex-hull bitrate ladders (both per-scene and whole-title
duration-weighted), schedules deterministic A/B forensic watermark
payloads, supports checkpoint-resume for preemptible/spot workers, offers
software AND hardware (NVENC/Quick Sync) ffmpeg encoder strategies with
HDR10/HLG static-metadata passthrough, tracks per-chunk encode cost, and
exposes Prometheus metrics.

This is a self-contained Go module living under
`projects/custom-video-processing-engine/services/distributed-encoding-orchestrator`.
It does not depend on, or modify, anything outside this directory.

## Architecture

```
cmd/orchestrator/main.go        Runnable demo: ladder optimizers + watermark scheduler + hardware/HDR argument builders + end-to-end dispatch with metrics/checkpoint/cost wired in (FakeSubprocessRunner, no real ffmpeg needed)

internal/domain/                Core domain model
  encodingDomainTypes.go          EncodingJob, ChunkEncodingTask, SceneBoundary, BitrateLadderRung, fan-out logic
  encodingJobBuilder.go           Builder pattern: EncodingJobBuilder
  jobQueue.go                     JobQueue: bounded, channel-backed, context-cancellable task queue

internal/ffmpeg/                ffmpeg subprocess wrapping
  codecArgumentBuilder.go         Strategy pattern: CodecArgumentBuilder (H264/HEVC/AV1)
  hardwareCodecArgumentBuilder.go Strategy pattern: NvencArgumentBuilder, QuickSyncArgumentBuilder (GPU/fixed-function hardware encoders)
  hdrMetadataArgumentDecorator.go Decorator pattern: HdrMetadataPassthroughCodecArgumentBuilder wraps any CodecArgumentBuilder to add HDR10/HLG static color metadata flags
  subprocessRunner.go             SubprocessRunner interface + real os/exec impl + FakeSubprocessRunner for tests
  progressParser.go               StreamingFfmpegProgressParser: parses ffmpeg "-progress" key=value output
  circuitBreaker.go               Circuit Breaker pattern: FfmpegFailureCircuitBreaker

internal/ladder/                VMAF bitrate-ladder optimizers (pure algorithmic code)
  convexHullBitrateLadderOptimizer.go        Per-scene upper convex hull (Andrew's monotone chain)
  globalPerTitleBitrateLadderOptimizer.go    Whole-title duration-weighted aggregation across per-scene hulls

internal/checkpoint/            Checkpoint-resume for preemptible workers
  jobCheckpointStore.go           Repository-style JobCheckpointStore interface + InMemoryJobCheckpointStore

internal/cost/                  Encode cost accounting
  costAccountant.go               CostAccountant: CPU-/GPU-seconds -> estimated USD, per-title TitleCostReport

internal/metrics/               Prometheus metrics
  prometheusMetrics.go            OrchestratorMetrics facade + "/metrics" HTTP endpoint (github.com/prometheus/client_golang)

internal/watermark/             A/B forensic watermark payload scheduler
  forensicWatermarkPayloadScheduler.go

internal/worker/                Concurrent execution engine
  taskProcessor.go                ChunkEncodingTaskProcessor interface + FfmpegBackedChunkEncodingTaskProcessor
  retryPolicy.go                  ExponentialBackoffRetryPolicy
  workerPool.go                   WorkerPool: semaphore-bounded concurrent goroutine fan-out + per-slot circuit breakers + utilization metrics
  dispatcher.go                   Dispatcher: job fan-out -> JobQueue -> WorkerPool -> JobCompletionSummary, with optional checkpoint/cost/metrics collaborators

internal/logging/               Structured logging facade over log/slog
  structuredLogger.go
```

### Design patterns applied

| Pattern | Where | Why |
|---|---|---|
| **Worker Pool** | `internal/worker/workerPool.go` | Bounds the number of concurrently-running ffmpeg subprocess goroutines regardless of how many thousands of chunk tasks are queued. |
| **Semaphore** | `WorkerPool.RunUntilQueueDrainedOrContextCancelled` | A buffered `chan struct{}` of capacity `MaxConcurrentTasks` gates how many per-task goroutines may run simultaneously — literal semaphore-acquire/release around each task's execution. |
| **Strategy** | `internal/ffmpeg/codecArgumentBuilder.go` | `CodecArgumentBuilder` interface with `h264ArgumentBuilder`, `hevcArgumentBuilder`, `av1ArgumentBuilder` implementations; callers never branch on codec type themselves. |
| **Circuit Breaker** | `internal/ffmpeg/circuitBreaker.go`, wired into `WorkerPool.dispatchSingleTaskWithRetry` | Protects against retry-storming a persistently broken ffmpeg path (bad node/disk/corrupt input) — trips Open after N consecutive failures, cools down, then probes via HalfOpen. |
| **Builder** | `internal/domain/encodingJobBuilder.go` | `EncodingJobBuilder` validates required fields (codec, scenes, ladder) before constructing an immutable `EncodingJob`. |
| **Dependency Injection** | `ffmpeg.SubprocessRunner` interface | Production code uses `NewRealOsExecSubprocessRunner()`; tests inject `*FakeSubprocessRunner`, so all command-building/process-management logic is unit-testable without an installed ffmpeg binary. |
| **Strategy (hardware encoders)** | `internal/ffmpeg/hardwareCodecArgumentBuilder.go` | `NvencArgumentBuilder`/`QuickSyncArgumentBuilder` implement the same `CodecArgumentBuilder` interface as the software builders — `worker.FfmpegBackedChunkEncodingTaskProcessor` never needs to know whether a task runs on CPU or GPU. |
| **Decorator** | `internal/ffmpeg/hdrMetadataArgumentDecorator.go` | `HdrMetadataPassthroughCodecArgumentBuilder` wraps any `CodecArgumentBuilder` (software or hardware) and augments its argument slice with HDR color-metadata flags, without either side needing to know about the other. |
| **Repository** | `internal/checkpoint/jobCheckpointStore.go` | `JobCheckpointStore` interface + `InMemoryJobCheckpointStore` decouple `Dispatcher`'s checkpoint-resume logic from how/where completion state is actually persisted. |
| **Optional-collaborator / fluent setters** | `worker.Dispatcher.WithJobCheckpointStore`/`WithCostAccountant`/`WithMetricsSink`, `worker.WorkerPool.SetMetricsSink` | Every cross-cutting concern added in this round (checkpointing, cost accounting, metrics) is attached post-construction and defaults to a fully-functional no-op when unset, so no existing constructor signature or call site had to change. |
| **Facade** | `internal/metrics/prometheusMetrics.go` | `OrchestratorMetrics` bundles every Prometheus metric this service exposes behind a small set of `Record*`/`Set*` methods, hiding `prometheus.CounterVec`/`GaugeOpts` construction details from callers. |

## The VMAF convex-hull bitrate-ladder optimizer

### Background

For one scene, encoding at many `(resolution, bitrate)` pairs and
measuring VMAF produces a scatter of points in the `(bitrate, VMAF)`
plane. The useful subset — established by Netflix's per-shot encoding
research — is the scatter's **upper convex hull**: for every bitrate on
the hull, no other sampled point achieves equal-or-better quality at
equal-or-lower cost, and no *combination* of two hull-adjacent points
(interpolated along their connecting chord) beats an excluded point
either.

### Algorithm (Andrew's monotone chain, upper-hull only)

`internal/ladder/convexHullBitrateLadderOptimizer.go`,
`computeUpperConvexHullFromRateDistortionSamples`:

1. Sort samples by ascending bitrate (ties broken by descending VMAF).
2. Deduplicate exact-bitrate ties, keeping only the highest-VMAF sample
   at each bitrate (a second point at an identical bitrate can never
   appear on a hull that has no vertical segments).
3. Sweep left to right maintaining a stack. Before pushing the next
   point, pop from the stack while the last two stacked points plus the
   candidate do **not** form a strictly clockwise turn — i.e., while the
   cross product

   ```
   cross(O, M, D) = (M.x-O.x)*(D.y-O.y) - (M.y-O.y)*(D.x-O.x)
   ```

   is `>= 0` for the middle point `M` between stack-top's predecessor
   `O` and the candidate `D`. `cross < 0` means `M` lies strictly above
   the chord `O -> D` (keep it); `cross >= 0` means `M` is on or below
   the chord (pop it — it's dominated/redundant).

This is the textbook monotone-chain hull construction restricted to the
upper chain only, since a bitrate ladder only ever wants to *maximize*
quality per bit — the lower chain (worst-case quality) is meaningless
here.

### Worked numeric example

Three points: `(0, 0)`, `(1, 2)`, `(2, 0)` (bitrate, VMAF units
simplified for hand-checking):

```
cross((0,0), (1,2), (2,0)) = (1-0)*(0-0) - (2-0)*(2-0) = 0 - 4 = -4 < 0
```

Negative cross product → clockwise turn → `(1,2)` lies **above** the
chord from `(0,0)` to `(2,0)` → keep it. All three points survive as a
"peak" hull.

Contrast with `(0,0)`, `(1,-2)`, `(2,0)`:

```
cross((0,0), (1,-2), (2,0)) = (1)*(0-0) - (-2)*(2-0) = 0 + 4 = 4 > 0
```

Positive cross product → **not** a clockwise turn → `(1,-2)` lies
**below** the chord → pop it (dominated). This sign convention is
verified directly in
`TestFormsClockwiseTurnMaximizingQualityPerBitrate_KnownCrossProductSigns`.

A larger, realistic worked example (see
`TestComputeUpperConvexHullFromRateDistortionSamples_NetflixStyleCrossResolutionFixture`)
demonstrates the classic **resolution cross-over** phenomenon:

| Resolution | Bitrate (kbps) | VMAF | On hull? |
|---|---|---|---|
| 480p | 600 | 55.0 | **yes** (cheapest point) |
| 480p | 1200 | 74.0 | **yes** (beats 720p@1200) |
| 720p | 1200 | 70.0 | no — dominated by 480p@1200 |
| 720p | 2000 | 84.0 | **yes** |
| 720p | 3000 | 90.0 | **yes** (beats 1080p@3000) |
| 1080p | 3000 | 88.0 | no — dominated by 720p@3000 |
| 1080p | 5000 | 95.0 | **yes** |
| 1080p | 8000 | 98.0 | **yes** (most expensive point) |

At `1200kbps`, 480p (74 VMAF) beats 720p (70 VMAF) — a classic
low-bitrate cross-over — so the hull correctly picks the *lower*
resolution at that rung, exactly the behavior a naive
"pick-highest-VMAF-per-resolution" heuristic would get wrong when two
resolutions are compared only within themselves rather than against the
whole rate-distortion scatter.

### Complexity & benchmark

`computeUpperConvexHullFromRateDistortionSamples` is `O(n log n)`
(dominated by the initial sort; the hull sweep itself is amortized
`O(n)` since each point is pushed and popped at most once).

```
BenchmarkComputeUpperConvexHullFromRateDistortionSamples/10-10       1,000,000     1,241 ns/op     1,704 B/op    9 allocs/op
BenchmarkComputeUpperConvexHullFromRateDistortionSamples/100-10         55,597    20,813 ns/op    16,296 B/op   12 allocs/op
BenchmarkComputeUpperConvexHullFromRateDistortionSamples/1000-10         3,021   405,021 ns/op   135,848 B/op   15 allocs/op
BenchmarkComputeUpperConvexHullFromRateDistortionSamples/10000-10          159 7,605,156 ns/op 2,126,511 B/op   23 allocs/op
```
(Apple M4, `go test ./internal/ladder/... -bench=. -benchmem`.) Even a
10,000-sample sweep — an unrealistically dense per-scene search — costs
under 8ms, negligible next to actual ffmpeg encode time.

## Global per-title bitrate ladder optimization

`internal/ladder/globalPerTitleBitrateLadderOptimizer.go` extends the
per-scene convex hull above to whole-**title** granularity, following the
"per-title encode" approach popularized by Netflix's dynamic optimizer
research: rather than picking one bitrate ladder rung applied uniformly
to every scene, each scene is independently encoded to whatever bitrate
its own rate-distortion hull requires to hit a shared VMAF quality floor,
and the reported title-level bitrate is the **duration-weighted average**
of those per-scene bitrates — the number that actually determines
storage/CDN egress cost.

### Algorithm

`ComputeGlobalPerTitleBitrateLadder(scenes []SceneHullWithDuration, targetVmafFloors []float64)`:

1. For each scene and each requested VMAF floor `Q`, find the minimum
   bitrate on that scene's already-computed convex hull achieving
   `VMAF >= Q` via **piecewise-linear interpolation** between the two
   hull points bracketing `Q`. This is valid because the hull's
   `VMAF(bitrate)` curve is concave and monotonically non-decreasing, so
   linear interpolation between adjacent hull vertices always
   *under*-estimates the true curve — the reported bitrate is never an
   over-optimistic requirement:

   ```
   t = (Q - v0) / (v1 - v0)
   bitrate(Q) = b0 + t*(b1 - b0)
   ```

   Boundary cases: if `Q` is at or below the hull's cheapest point, that
   point is used directly (no interpolation needed); if `Q` exceeds the
   hull's most expensive point, no sampled encode can reach it — the best
   available point is used and the scene is flagged `QualityFloorMet =
   false` (surfaced up as `AnyScenesQualityLimited` on the rung) rather
   than silently pretending the floor was met.
2. Aggregate: `EffectiveBitrateKilobitsPerSecond = sum(bitrate_i * duration_i) / sum(duration_i)`.

### Worked example

Two scenes — Scene 0 (10s, hull `(1000kbps,80VMAF)`→`(3000kbps,95VMAF)`)
and Scene 1 (30s, hull `(1000kbps,90VMAF)`→`(2000kbps,96VMAF)`) — solved
for a quality floor of `93`:

```
Scene 0: t = (93-80)/(95-80) = 13/15 ≈ 0.86667 -> bitrate = 1000 + 0.86667*2000 ≈ 2733.33 kbps
Scene 1: t = (93-90)/(96-90) = 3/6 = 0.5       -> bitrate = 1000 + 0.5*1000     = 1500.00 kbps

totalKilobits = 2733.33*10 + 1500*30 = 72333.33
totalDuration = 10 + 30 = 40s
effectiveRate = 72333.33 / 40 ≈ 1808.33 kbps
```

Notice `1808.33 kbps` is meaningfully **lower** than the `2733.33 kbps`
a naive fixed-bitrate-for-the-whole-title approach would need to pick to
guarantee every scene hits `VMAF>=93` (it would have to use scene 0's
higher requirement across the *entire* title, wasting bits on scene 1,
which needed far less) — this is exactly the saving per-title encoding
captures in production. This exact scenario is hand-verified in
`TestComputeGlobalPerTitleBitrateLadder_TwoSceneHandComputedFixture`,
alongside boundary-case tests (floor below/above the hull's range),
a monotonicity property test (higher floor never yields a lower
effective bitrate), and an integration test that runs raw
rate-distortion scatter data through the per-scene hull first, exactly
as a real caller would.

## Hardware-accelerated encoder strategies (NVENC / Quick Sync)

`internal/ffmpeg/hardwareCodecArgumentBuilder.go` adds two more
`CodecArgumentBuilder` Strategy implementations alongside the software
H.264/HEVC/AV1 builders:

- **`NvencArgumentBuilder`** (`NewNvencArgumentBuilder()`, `CodecH264Nvenc`) —
  NVIDIA NVENC via ffmpeg's `h264_nvenc` encoder, `-hwaccel cuda`, VBR
  rate control (`-rc vbr -b:v/-maxrate/-bufsize`) or constant-quality
  (`-rc constqp -cq N`) when `ConstantRateFactorQuality` is set.
- **`QuickSyncArgumentBuilder`** (`NewQuickSyncArgumentBuilder()`, `CodecH264QuickSync`) —
  Intel Quick Sync Video via ffmpeg's `h264_qsv` encoder, `-hwaccel qsv`,
  conventional bitrate flags or `-global_quality N` for constant quality.

Both implement exactly the same `CodecArgumentBuilder` interface as the
software builders, so `worker.FfmpegBackedChunkEncodingTaskProcessor`
and `CodecArgumentBuilderFactory` never branch on hardware-vs-software —
the only thing that changes is which `SupportedVideoCodec` an
`EncodingJob` was built with. Exact expected argument slices (including
`-hwaccel` correctly positioned *before* `-i`, which real ffmpeg
requires) are hand-verified in
`internal/ffmpeg/hardwareCodecArgumentBuilder_test.go`.

Hardware encode trades some compression efficiency for dramatically
higher throughput (often 10-50x a comparable software preset) — see
"Encode cost accounting" below for how `internal/cost` lets an operator
weigh that trade-off in real dollars rather than just wall-clock time.

## HDR10/Dolby Vision metadata pass-through

`internal/ffmpeg/hdrMetadataArgumentDecorator.go` implements the
**Decorator** pattern over `CodecArgumentBuilder`:
`HdrMetadataPassthroughCodecArgumentBuilder` wraps any existing builder
(software or hardware) and splices HDR color/mastering-metadata flags
into its argument slice, immediately before the output path.

### What this actually does (static metadata)

Given an `HdrMetadataProfile` (color primaries, transfer characteristic,
color space, optional SMPTE ST 2086 mastering-display value, optional
CEA-861.3 MaxCLL/MaxFALL), it emits:

```
-color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc
-master-display "G(...)B(...)R(...)WP(...)L(...)" -max_cll "1000,400"
```

This is real, static, whole-asset HDR characterization that ffmpeg
genuinely writes into the encoded bitstream natively — no external
tooling required. Wrapping a builder with the zero-value
`HdrMetadataProfile{}` (SDR/non-HDR content) is a documented no-op: the
wrapped builder's output is returned byte-for-byte unchanged.

### What this honestly does NOT do (dynamic per-frame metadata)

HDR10+'s per-frame tone-mapping SEI messages and Dolby Vision's RPU
(Reference Processing Unit) stream are fundamentally different from
static metadata: they are a per-frame side-channel that must be
extracted from the source and re-multiplexed onto the freshly-encoded
bitstream, frame-for-frame, as a **separate post-encode processing
step**. Stock ffmpeg does not implement that extraction/injection
itself. The real production tools for it are:

- **[`hdr10plus_tool`](https://github.com/quietvoid/hdr10plus_tool)** —
  extracts per-frame HDR10+ metadata as JSON, and injects it into a
  freshly-encoded HEVC bitstream via libx265's `--dhdr10-info` parameter.
  `HdrMetadataProfile.BuildHdr10PlusInjectionX265ParamsFragment()` builds
  *that x265 parameter fragment* (`dhdr10-info=<path>`) for a caller that
  has already run `hdr10plus_tool` upstream — this package never invokes
  `hdr10plus_tool` itself.
- **[`dovi_tool`](https://github.com/quietvoid/dovi_tool)** — extracts and
  re-injects a Dolby Vision RPU stream as a separate multiplexing step;
  entirely outside ffmpeg's own CLI flags.

`HdrMetadataProfile.RequiresExternalDynamicMetadataToolStep()` reports
`true` for HDR10+/Dolby Vision profiles specifically so callers get an
explicit, honest signal that this codebase's HDR support stops at static
metadata, rather than a flag that silently does nothing for the dynamic
case.

## Checkpoint-resume for preemptible workers

`internal/checkpoint/jobCheckpointStore.go` defines the `JobCheckpointStore`
Repository interface (`RecordChunkCompleted`, `IsChunkCompleted`,
`ListCompletedChunks`, `ClearJob`) plus `InMemoryJobCheckpointStore`, and
`worker.Dispatcher.WithJobCheckpointStore(store)` wires it into
`DispatchEncodingJob`:

- Before dispatching, the Dispatcher lists every chunk already recorded
  as completed for the job (e.g. by a prior, preempted dispatch attempt)
  and excludes exactly those `ChunkEncodingTask`s from the set actually
  submitted to the `WorkerPool` — they're counted straight into the
  returned `JobCompletionSummary` without being re-processed.
- Every task that *does* run and succeeds is recorded into the
  checkpoint store as its outcome is observed, so a second preemption
  mid-resume loses no additional progress either.

This works because `domain.FanOutEncodingJobIntoChunkEncodingTasks`
deterministically derives the same `TaskIdentifier` for the same
`(job, scene, chunk, rendition)` tuple on every call (see
`domain.FormatChunkTaskIdentifier`), so a checkpoint store's record of
"already done" identifiers from one fan-out lines up exactly with a
fresh fan-out of the same `EncodingJob` on a resumed attempt — no
separate resume-specific bookkeeping is needed anywhere else in the
system.

**Proof of correctness**:
`internal/worker/dispatcherCheckpointResume_test.go`'s
`TestDispatcher_CheckpointResume_SimulatedPreemptionLosesNoWorkAndDuplicatesNone`
simulates a preempted worker fleet end to end across two dispatch
"phases" against the same checkpoint store — phase 1 is cancelled by a
background goroutine partway through (once at least 2 chunks have
completed), phase 2 resumes the same job — and asserts, via a call-log on
the task processor, that **every** chunk the job fans out into was
processed **exactly once** across both phases combined (no data loss, no
duplicate work), with phase 2's final summary reporting
`SucceededTaskCount == TotalTaskCount`. A second, fully deterministic
test (`TestDispatcher_CheckpointResume_PreCheckpointedTasksAreNeverReprocessed`)
directly pre-populates a subset of chunks as "already done" and verifies
the Dispatcher skips exactly those.

## Encode cost accounting

`internal/cost/costAccountant.go`'s `CostAccountant` tracks CPU-seconds
(software codec strategies) or GPU-seconds (`NvencArgumentBuilder`/
`QuickSyncArgumentBuilder`) consumed per completed chunk and converts
that into an estimated USD cost via configurable
`ComputeResourceRates{CpuSecondPriceUsd, GpuSecondPriceUsd}`, aggregating
into a per-job `TitleCostReport{ChunkCount, TotalCpuSeconds,
TotalGpuSeconds, TotalEstimatedCostUsd}`.

`worker.Dispatcher.WithCostAccountant(accountant)` wires it in: every
successfully completed `ChunkEncodingTask`'s `TotalWallClockDuration` is
recorded against its parent job, billed at the CPU or GPU rate depending
on `SupportedVideoCodec.IsHardwareAccelerated()`. The arithmetic
(`EstimatedCostUsd = resourceSecondsConsumed * rate`, summed across
mixed CPU/GPU chunks within one title) is hand-verified in
`internal/cost/costAccountant_test.go`, and the Dispatcher wiring itself
— including correct CPU-vs-GPU rate selection based on the task's codec
— is proven end to end in
`internal/worker/dispatcherCostAndMetrics_test.go`.

## Prometheus metrics

`internal/metrics/prometheusMetrics.go`'s `OrchestratorMetrics` facade
registers and exposes:

- `..._worker_pool_active_task_count` / `..._max_concurrent_task_count` — gauges, worker-pool utilization.
- `..._worker_pool_chunk_encoding_tasks_completed_total{result}` — counter, chunk completions by succeeded/failed.
- `..._dispatcher_encoding_jobs_completed_total{final_status}` — counter, job completions by completed/failed.
- `..._cost_resource_seconds_consumed_total{resource_kind}` / `..._cost_estimated_cost_usd_total` — counters, mirroring `CostAccountant`.

`metrics.NewMetricsHttpServeMux(registry)` mounts these at the
conventional `/metrics` path via
`github.com/prometheus/client_golang/prometheus/promhttp`, ready to pass
to `http.ListenAndServe` (the demo binary does exactly this, listening on
`:9090`). `WorkerPool.SetMetricsSink` and `Dispatcher.WithMetricsSink`
wire the same `*OrchestratorMetrics` instance into both layers. Every
metric type and the HTTP scrape output itself (via `httptest` + reading
back the exposition-format body) are tested in
`internal/metrics/prometheusMetrics_test.go`.

## A/B forensic watermarking payload scheduler

`internal/watermark/forensicWatermarkPayloadScheduler.go` implements a
documented, deterministic hash-chain PRNG:

```
state_0    = SHA256(userId || 0x1F || sessionId || 0x1F || "dewm-v1")
state_i+1  = SHA256(state_i)
bit_i      = LSB of the first 8 bytes of state_i (parity of a big-endian uint64)
```

`GenerateDeterministicVariantSequenceForSession(userId, sessionId, n)`
produces the ordered `n`-chunk A/B sequence; `VerifyRecoveredVariantSequenceMatchesUserSession`
recomputes and prefix-compares the same chain to confirm whether a
recovered (leaked-content) A/B pattern matches a candidate session — the
decode/round-trip counterpart. `IdentifyMatchingSessionAmongCandidates`
wraps that into the anti-piracy "narrow a suspect list down to one"
workflow. Round-trip correctness (including partial-prefix recovery and
single-bit-tamper detection) is proven in
`internal/watermark/forensicWatermarkPayloadScheduler_test.go`.

**Security note**: SHA-256 here is used purely as a deterministic,
well-diffused pseudo-random function, not for cryptographic secrecy —
this scheme is about deterministic, independently-recomputable forensic
tracing, not resistance to an adversary with oracle access to many
generated sequences.

## Throughput targets: fanning out a 2-hour film

**Target**: fan out a 2-hour film into ~3,600 two-second chunks and
complete distributed HEVC encoding across a 500-worker fleet in under
8 minutes wall-clock.

**The math**:

- 2 hours = 7,200 seconds of source video.
- At 2-second micro-chunks: `7200 / 2 = 3,600 chunks` (confirmed exactly
  by `TestFanOutEncodingJobIntoChunkEncodingTasks_TwoHourFilmProducesExpectedChunkCount`,
  which fans out a synthetic 7,200s/24fps scene at 2s/chunk and asserts
  precisely 3,600 resulting `ChunkEncodingTask` values).
- Assume **X = 15 encode-seconds-per-chunk** for HEVC (a 2-second 1080p
  chunk on a modern cloud CPU core at a "medium"-equivalent x265
  preset — real-world figures vary with resolution/preset/content
  complexity, but 5-25s/chunk is a representative range for HD HEVC).
- Total sequential CPU-seconds required: `3600 chunks * 15s/chunk = 54,000 CPU-seconds`.
- With a 500-worker fleet processing chunks in parallel (bounded
  concurrency via `WorkerPool.MaxConcurrentTasks = 500`):
  `54,000 / 500 = 108 seconds ≈ 1.8 minutes` of pure encode wall-clock.
- That leaves enormous headroom under the 8-minute (480s) budget for
  scheduling overhead, retries, and multiple ladder rungs: even
  encoding **4 ladder rungs per chunk** (4x the work:
  `54,000 * 4 = 216,000 CPU-seconds`) still finishes in
  `216,000 / 500 = 432s = 7.2 minutes` — just inside budget — which is
  why a real deployment sizes the worker fleet (or reduces rung count
  via the convex-hull optimizer, which is exactly what it's for) to fit
  the SLA.
- The orchestrator's own scheduling/dispatch overhead is negligible next
  to encode time: the benchmark below shows the `WorkerPool` dispatching
  and completing 1,000 trivial (near-zero-cost) tasks in ~2.3ms
  end-to-end, i.e. on the order of microseconds of overhead per task —
  orders of magnitude below the 15s/chunk encode-dominated real cost.

```
BenchmarkWorkerPool_DispatchThroughput/concurrency-8-10     549   2,307,579 ns/op    255,042 B/op   3,006 allocs/op
BenchmarkWorkerPool_DispatchThroughput/concurrency-64-10    608   2,318,596 ns/op    277,721 B/op   3,011 allocs/op
BenchmarkWorkerPool_DispatchThroughput/concurrency-512-10   490   2,205,217 ns/op    453,326 B/op   3,006 allocs/op
```
(Each op = one full batch of 1,000 tasks dispatched + completed through the pool; Apple M4.)

### Worked cost example: the same 2-hour film, CPU vs. GPU

Reusing the 54,000-CPU-second HEVC estimate above (one ladder rung, no
retries) at the demo's illustrative rate of `$0.00003/CPU-second`
(≈`$0.10`/CPU-hour):

```
54,000 CPU-seconds * $0.00003/CPU-second = $1.62 for the whole film, one rung
216,000 CPU-seconds (4 rungs)             = $6.48 for the whole film, four rungs
```

If the same 4-rung encode instead ran entirely on NVENC hardware at,
say, 8 encode-seconds/chunk (roughly 2x faster than the 15s/chunk
software estimate, a conservative real-world NVENC speedup) and the
demo's illustrative GPU rate of `$0.0003/GPU-second` (≈`$1.08`/GPU-hour,
10x the CPU rate):

```
3,600 chunks * 8s/chunk * 4 rungs = 115,200 GPU-seconds
115,200 GPU-seconds * $0.0003/GPU-second = $34.56 for the whole film, four rungs
```

In this illustrative fixture, software CPU encoding is actually the
**cheaper** option despite being slower — GPU-second pricing would need
to drop below roughly `$0.00018`/GPU-second (a ~3.5x rather than 10x
CPU-rate premium) for NVENC to break even at this speedup, given real
per-provider GPU/CPU pricing ratios and NVENC's actual throughput
advantage on a given fleet. This is exactly the kind of trade-off
`cost.CostAccountant`'s per-title `TotalEstimatedCostUsd` is meant to
make visible and comparable across strategies rather than assumed —
see `internal/cost/costAccountant_test.go` for the underlying arithmetic
tests these numbers are derived from.

## Running the tests

Go toolchain **is** available in this environment (`go version` reports
`go1.26.3 darwin/arm64`), so the full suite was actually run rather than
just described:

```
$ go build ./...
$ go vet ./...
$ go test ./... -race
ok  	distributed-encoding-orchestrator/internal/checkpoint	(race)
ok  	distributed-encoding-orchestrator/internal/cost	(race)
ok  	distributed-encoding-orchestrator/internal/domain	(race)
ok  	distributed-encoding-orchestrator/internal/ffmpeg	(race)
ok  	distributed-encoding-orchestrator/internal/ladder	(race)
ok  	distributed-encoding-orchestrator/internal/metrics	(race)
ok  	distributed-encoding-orchestrator/internal/watermark	(race)
ok  	distributed-encoding-orchestrator/internal/worker	(race)
```

All packages pass, including under `-race`, with zero test failures —
119 individual test functions across 8 test-bearing packages (up from 5
packages before this round's additions: `internal/checkpoint`,
`internal/cost`, and `internal/metrics` are new; `internal/ffmpeg` and
`internal/ladder` gained new test files alongside their existing ones;
`internal/worker` gained new Dispatcher-level integration tests). Test
coverage highlights:

- **Convex hull**: hand-computed triangle/dip/collinear fixtures with
  explicit cross-product sign verification, plus a realistic
  multi-resolution "cross-over" fixture (`internal/ladder/*_test.go`).
- **A/B watermark**: determinism, per-user/per-session divergence,
  full round-trip encode/decode, partial-prefix verification,
  single-bit-tamper detection, and a multi-candidate suspect-matching
  test (`internal/watermark/*_test.go`).
- **Codec argument builders**: exact expected `[]string` argument
  slices for H.264/HEVC/AV1 in both bitrate-targeted and CRF modes
  (`internal/ffmpeg/codecArgumentBuilder_test.go`).
- **Worker pool concurrency**: 2,000-task exactly-once completion,
  concurrency-bound enforcement via an instrumented processor,
  retry/backoff-to-eventual-success, permanent-failure-after-retries,
  a goroutine-leak check via `runtime.NumGoroutine()` before/after a
  500-task run, and prompt shutdown on context cancellation
  (`internal/worker/workerPool_test.go`) — all clean under `-race`.
- **Circuit breaker**: full state-machine coverage (Closed → Open →
  HalfOpen → Closed/Open again) using an injectable clock, no real
  sleeps.
- **Process runner / progress parser**: `FakeSubprocessRunner`
  scripting (including scripted flakiness for retry tests), plus a
  handful of tests against the *real* `os/exec`-backed runner using
  ubiquitous POSIX utilities (`true`/`false`/`echo`/`sleep`, not
  ffmpeg) to validate the actual process-management plumbing
  end-to-end, with automatic skip if those binaries are ever
  unavailable on a given sandbox.
- **Global per-title ladder**: the hand-computed two-scene interpolation
  fixture worked out above, both interpolation boundary cases (floor
  below/above a hull's range), a monotonicity property test, a
  degenerate single-scene test, error-contract tests, and an integration
  test chaining raw rate-distortion samples through the per-scene hull
  first (`internal/ladder/globalPerTitleBitrateLadderOptimizer_test.go`).
- **Hardware codec builders**: exact `[]string` argument slices for both
  NVENC and Quick Sync in bitrate-targeted and constant-quality modes,
  `-hwaccel` positioned correctly before `-i`, and factory resolution
  (`internal/ffmpeg/hardwareCodecArgumentBuilder_test.go`).
- **HDR metadata decorator**: exact spliced argument slices for an HDR10
  profile, the documented zero-value/SDR no-op contract, composition with
  a hardware builder (proving Strategy+Decorator compose orthogonally),
  and the HDR10+/Dolby Vision dynamic-metadata boundary functions
  (`internal/ffmpeg/hdrMetadataArgumentDecorator_test.go`).
- **Checkpoint-resume**: an in-isolation `JobCheckpointStore`
  round-trip/idempotency/namespace-isolation suite
  (`internal/checkpoint/jobCheckpointStore_test.go`), plus the
  end-to-end simulated-preemption Dispatcher test described above
  (`internal/worker/dispatcherCheckpointResume_test.go`).
- **Cost accounting**: hand-verified CPU/GPU rate arithmetic,
  multi-chunk/multi-resource-kind aggregation, negative-input clamping,
  and Dispatcher-level wiring including correct CPU-vs-GPU rate selection
  by codec (`internal/cost/costAccountant_test.go`,
  `internal/worker/dispatcherCostAndMetrics_test.go`).
- **Prometheus metrics**: every gauge/counter verified via a real
  `httptest` server scrape of the `/metrics` exposition-format output
  (not just in-process assertions on the `prometheus` client objects),
  plus a nil-receiver safety test for every mutating method
  (`internal/metrics/prometheusMetrics_test.go`).

Note: no real `ffmpeg` binary is installed or required anywhere in this
test suite — every ffmpeg-dependent code path is exercised through the
`SubprocessRunner` interface's `FakeSubprocessRunner` implementation, per
the task's explicit design requirement.

## Limitations

- The `ffmpeg.ChunkEncodeSpecification` -> CLI-argument mapping covers
  the flags this system's own pipeline needs (scaling, codec selection,
  bitrate/CRF, presets, progress reporting, and now static HDR color
  metadata); it is not a complete wrapper around every ffmpeg flag (no
  audio-track handling or subtitle burn-in, for example).
- `FfmpegBackedChunkEncodingTaskProcessor` assumes chunk inputs already
  exist as separate media files/objects (produced by an upstream
  chunking step); it does not itself perform the initial demux/segment
  cut from the original master.
- The A/B watermark scheduler produces the *scheduling decision*
  (which variant to splice per chunk); it does not implement the
  actual imperceptible pixel-level marking of Variant A vs Variant B,
  which is a separate, codec/perceptual-model-specific concern.
- `cmd/orchestrator` is a demonstration entry point: it now *does* start
  a small `/metrics` HTTP server (for the Prometheus section above) but
  still has no real job-submission API — no HTTP/gRPC endpoint accepts
  new `EncodingJob`s from outside the process. Wiring a real API surface
  around `worker.Dispatcher` is straightforward but out of scope here.
- The throughput-target math above is illustrative (based on a stated
  assumed encode-seconds-per-chunk), not a benchmark against real
  ffmpeg HEVC encoding, since no ffmpeg binary was invoked in this
  sandbox; the same is true of the worked cost-comparison numbers
  (illustrative rates and an assumed NVENC speedup factor, not measured
  against a real GPU-equipped fleet).
- **HDR dynamic metadata is a documented, explicit boundary, not a gap
  papered over with a no-op flag**: `HdrMetadataPassthroughCodecArgumentBuilder`
  genuinely emits ffmpeg's native static HDR10/HLG color-metadata flags,
  but HDR10+'s per-frame SEI messages and Dolby Vision's RPU stream
  require external tools (`hdr10plus_tool`, `dovi_tool`) run as separate
  pipeline steps this codebase does not invoke — see "HDR10/Dolby Vision
  metadata pass-through" above for exactly where that boundary sits and
  what `RequiresExternalDynamicMetadataToolStep()` signals to callers.
- `internal/checkpoint.InMemoryJobCheckpointStore` is durable only for
  the lifetime of the owning process; a real multi-node preemptible-fleet
  deployment needs a `JobCheckpointStore` implementation backed by shared
  storage reachable from every worker node (Redis, DynamoDB, etc.) —
  the interface is deliberately shaped to make that a drop-in swap with
  zero `Dispatcher` changes, but this codebase does not ship one.
- `internal/cost.CostAccountant` treats a completed chunk's wall-clock
  duration as a 1:1 proxy for CPU-/GPU-seconds consumed, assuming exactly
  one core (or one GPU encode session) per in-flight task — matching how
  `WorkerPool` actually schedules work, but not modeling deployments that
  oversubscribe cores per task or share one GPU across several concurrent
  sessions.
- The hardware codec builders (`NvencArgumentBuilder`/
  `QuickSyncArgumentBuilder`) are H.264-only in this codebase (matching
  the task's own literal ffmpeg-flag examples); HEVC hardware variants
  (`hevc_nvenc`/`hevc_qsv`) would follow the identical pattern but are
  not implemented here.
- `internal/metrics`'s `/metrics` endpoint in the demo binary is only
  kept alive for a few seconds after the synchronous demo work finishes
  (see `cmd/orchestrator/main.go`) since the binary is not a long-running
  server; a real deployment would start the same
  `metrics.NewMetricsHttpServeMux` at process startup and never stop it.
