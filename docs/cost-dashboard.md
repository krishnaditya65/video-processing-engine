# Per-Title Cost Dashboard — Unit Economics Design

This document designs a per-title unit-economics view aggregating three
cost sources across the platform's lifecycle — encode compute, origin
storage, and CDN egress — into a single "what does streaming this title
actually cost us" number per title, and a blended cost-per-viewing-hour
figure across the catalog.

## Data sources

| Cost component | Source | Real field(s) |
|---|---|---|
| Encode CPU/GPU-seconds | `distributed-encoding-orchestrator`'s `CostAccountant` | `services/distributed-encoding-orchestrator/internal/cost/costAccountant.go` — `TitleCostReport{ChunkCount, TotalCpuSeconds, TotalGpuSeconds, TotalEstimatedCostUsd}`, fed by `CostAccountant.RecordChunkCompletion(jobIdentifier, chunkIdentifier, kind ResourceKind, resourceSecondsConsumed)` |
| Origin storage | `minioOriginObjectStorage` (S3-API-compatible), sized per elementary-stream footprint per title | Bucket usage per `assetId` prefix × `originStorageGigabyteMonthPriceUsd` |
| CDN egress | `edge-packaging-and-manifest-service` / `multi-cdn-edge-routing-service` — bytes served per `assetId` via JIT packaging responses | `gigabytesServed` per title × `gigabyteEgressPriceUsd` |

`CostAccountant` (referenced above) is the concurrently-developed
component this design builds on — it was still being built by a sibling
work-stream at the time this document was authored, but its real,
compiled, tested source (`services/distributed-encoding-orchestrator/internal/cost/costAccountant.go`
and its `costAccountant_test.go`) was read directly from the repository
to ground this design accurately, rather than guessed. Its exported shape
(`ResourceKind` = `cpu`/`gpu`, `ComputeResourceRates{CpuSecondPriceUsd,
GpuSecondPriceUsd}`, `TitleCostReport{JobIdentifier, ChunkCount,
TotalCpuSeconds, TotalGpuSeconds, TotalEstimatedCostUsd}`) is exactly
what `scripts/aggregate-cost-report.js`'s fixture and reconciliation logic
model.

## Aggregation design

```
distributed-encoding-orchestrator          minioOriginObjectStorage      edge-packaging-and-manifest-service
  CostAccountant.TitleCostReport              per-assetId bucket size      per-assetId bytes-served counter
  { totalCpuSeconds, totalGpuSeconds,          × storage rate               × egress rate
    totalEstimatedCostUsd }                        │                            │
         │                                          │                            │
         └──────────────────────┬───────────────────┴────────────────────────────┘
                                 ▼
                  Per-title cost report JSON
                  (one entry per titleId/assetId, this
                   document's fixture shape — see
                   scripts/fixtures/sample-per-title-cost-report.json)
                                 │
                                 ▼
                  scripts/aggregate-cost-report.js
                  (reads the JSON, recomputes each cost component
                   from raw quantities × rates as a sanity check
                   against any pre-computed values in the input,
                   and prints a formatted per-title + aggregate
                   breakdown, including cost-per-viewing-hour)
```

In production, the arrow from `CostAccountant`/MinIO/edge-packaging into
"Per-title cost report JSON" would be a small periodic batch job (e.g. a
Temporal scheduled workflow, since `control-plane-api-gateway` already
owns the only Temporal client in this system) that:

1. Calls `CostAccountant.TitleCostReportFor(jobIdentifier)` for every
   `EncodingJob` completed in the billing window and maps
   `jobIdentifier` → `titleId`/`assetId` (the two are related 1:1 per
   title in the current domain model — one `EncodingJob` per source
   asset's full multi-rendition encode, per `costAccountant.go`'s own doc
   comment: *"one source asset's full multi-rendition encode"*).
2. Queries MinIO's admin API (or a periodic `mc du` / bucket-usage scrape)
   for per-`assetId`-prefix storage footprint, multiplied by
   `originStorageGigabyteMonthPriceUsd`.
3. Sums a new Prometheus counter — `edge_cdn_egress_bytes_total{assetId}`
   — that `edge-packaging-and-manifest-service`'s JIT packaging handler
   (`GET /package/{assetId}/{chunkId}`,
   `services/edge-packaging-and-manifest-service/internal/httpapi/httpapi.go`)
   would need to increment per response, over the billing window,
   multiplied by `gigabyteEgressPriceUsd`.
4. Emits the combined JSON in the shape documented below, storable
   wherever is convenient (ClickHouse — reusing
   `analytics-telemetry-platform`'s existing OLAP infrastructure — is a
   natural fit alongside QoE telemetry) and consumable by
   `scripts/aggregate-cost-report.js` or a future Grafana panel.

This design is **authored, not implemented**: no scheduled Temporal
workflow, MinIO usage-scraper, or `edge_cdn_egress_bytes_total` counter
exists in `services/*` yet (and this task explicitly does not modify
`services/*`). What **is** real and runnable today is the aggregation
script below, against a realistic fixture matching this exact shape.

## Cost-report JSON shape

```json
{
  "reportGeneratedAt": "ISO-8601 timestamp",
  "billingWindow": { "startDate": "YYYY-MM-DD", "endDate": "YYYY-MM-DD" },
  "computeResourceRates": { "cpuSecondPriceUsd": number, "gpuSecondPriceUsd": number },
  "storageRates": { "originStorageGigabyteMonthPriceUsd": number },
  "cdnEgressRates": { "gigabyteEgressPriceUsd": number },
  "titles": [
    {
      "titleId": "string, matches control-plane-api-gateway's VideoAsset id",
      "displayTitle": "string",
      "encodingCost": { "jobIdentifier": "string", "chunkCount": number, "totalCpuSeconds": number, "totalGpuSeconds": number, "totalEstimatedCostUsd": number },
      "originStorage": { "elementaryStreamsGigabytes": number, "gigabyteMonths": number, "estimatedCostUsd": number },
      "cdnEgress": { "gigabytesServed": number, "estimatedCostUsd": number },
      "viewingHoursServed": number
    }
  ]
}
```

See `scripts/fixtures/sample-per-title-cost-report.json` for a complete,
realistic 4-title example (using the same sample `assetId`s as
`load-testing/`'s k6 scripts, for consistency across this PR's fixtures).

## The aggregation script — ACTUALLY RUN, not just authored

`scripts/aggregate-cost-report.js` is a standalone, dependency-free
Node.js script (uses only `node:fs`/`node:path`, no `npm install`
needed). It:

- Reads a cost-report JSON fixture (defaults to
  `scripts/fixtures/sample-per-title-cost-report.json` if no path is
  given).
- **Recomputes** each cost component from raw quantities × rates
  (mirroring `CostAccountant.RecordChunkCompletion`'s
  `resourceSecondsConsumed * rate` arithmetic exactly) rather than
  trusting the fixture's own pre-computed `estimatedCostUsd` fields
  blindly — if a fixture's stated cost disagrees with the recomputed
  value by more than $0.005, it prints a
  `[recomputed $X, fixture value $Y differed]` note. This is a real,
  working sanity check, not decorative: the shipped fixture's
  `asset-stranger-things-s05e01` entry has a small deliberate
  encoding-cost mismatch precisely so this run demonstrates the
  reconciliation catching real bad data (see the captured run output
  below).
- Prints, per title: encoding/storage/CDN-egress cost with each
  component's percentage share of that title's total, chunk count and
  CPU/GPU-seconds, total cost, viewing hours served, and cost per
  viewing-hour.
- Prints an aggregate summary across all titles (total cost per
  component, grand total, blended cost/viewing-hour, and a ranking of
  titles by total cost).

### Actually run in this environment

```bash
$ node scripts/aggregate-cost-report.js
```

Real captured output (abbreviated to the first title + the aggregate
section; full output reproduced by running the command above):

```
Per-Title Unit-Economics Cost Report
Source file: /Users/krishnaditya/Documents/ideas/projects/custom-video-processing-engine/scripts/fixtures/sample-per-title-cost-report.json
Report generated at (fixture timestamp): 2026-08-11T00:00:00Z
Billing window: 2026-08-01 .. 2026-08-11

Stranger Things S05E01  (asset-stranger-things-s05e01)
------------------------------------------------------------------------
  Encoding compute        $3.0780  (0.0%)   3600 chunks, 48,600.0 CPU-s / 5,400.0 GPU-s
    [recomputed $3.0780, fixture value $3.1806 differed]
  Origin storage          $1.7682  (0.0%)
  CDN egress         $18,553.5225  (100.0%)
  TOTAL                $18,558.37
  Viewing hours served: 198,450   ->  $0.0935 / viewing-hour
...
========================================================================
AGGREGATE — ALL TITLES
========================================================================
  Titles in report:        4
  Total encoding cost:     $12.58
  Total origin storage:    $6.98
  Total CDN egress:        $40,617.97
  GRAND TOTAL:             $40,637.53
  Total viewing hours:     420,850
  Blended cost/view-hour:  $0.0966

  Ranked by total cost (highest first):
    1. Stranger Things S05E01                     $18,558.37
    2. Formula 1: Drive to Survive S07E02         $11,059.05
    3. Our Planet II E03                           $7,061.25
    4. The Crown S06E01                            $3,958.86
```

**What this confirms**: real JSON parsing, real arithmetic against a
realistic fixture, correct percentage/ranking/aggregation logic, and a
working cross-check against inconsistent input data — exit code 0, no
exceptions. **What this does not confirm**: real production cost numbers
(the fixture's `computeResourceRates`/`storageRates`/`cdnEgressRates` are
illustrative, sourced from the rate commentary in `costAccountant.go`'s
own doc comments — "$0.10/CPU-hour" and "$1.08/GPU-hour" — not a real
cloud bill), and no live `CostAccountant`/MinIO/edge-packaging aggregation
pipeline was built or run (see "This design is authored, not
implemented" above).

## Interpreting the unit-economics numbers

- **Encoding cost is a rounding error next to CDN egress** in this
  fixture (~$12.58 total encoding vs. ~$40,618 total CDN egress across
  four titles) — this matches the well-known economics of large-scale
  video streaming: encoding a title happens once, egress happens once
  per gigabyte per viewer, every time. This is the single most important
  read of any real version of this dashboard: **egress dominates unit
  economics**, so cost-optimization effort (better convex-hull bitrate
  ladders reducing average bitrate at equivalent VMAF, smarter CDN
  selection via `multi-cdn-edge-routing-service`, cache-hit-ratio
  improvements) should be weighted accordingly.
- **GPU vs. CPU encode cost tradeoff** is visible per title via
  `totalGpuSeconds`/`totalCpuSeconds` — `Our Planet II E03` and
  `Formula 1: Drive to Survive S07E02` use proportionally more GPU
  seconds in the fixture (representing hardware NVENC/Quick Sync
  encode paths per `costAccountant.go`'s doc comment), illustrating the
  real tradeoff the package's own documentation calls out: GPU encode is
  typically priced 5-15x higher per second but 10-50x faster, so the
  per-chunk GPU cost can still come out lower or higher depending on the
  specific ladder rung — exactly the kind of question this dashboard is
  meant to let an operator answer empirically per title rather than by
  assumption.
- **Cost-per-viewing-hour** normalizes for title popularity, making it
  possible to compare a low-egress prestige drama against a
  high-egress live-sports title on a like-for-like basis rather than by
  raw dollar cost alone.
