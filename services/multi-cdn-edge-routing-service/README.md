# multi-cdn-edge-routing-service

The **Multi-CDN Edge Routing & Predictive Pre-Fetching Engine**: simulates
two of the traffic-steering decisions a hyperscale video platform's edge
control plane makes continuously, at massive request volume:

1. *"Given this client's network context and the requested asset, which of
   our (mock) CDN providers — CloudFront, Akamai, or Fastly — should serve
   this playback session right now?"* — a real, documented, swappable
   composite-scoring routing decision with automatic failover away from
   unhealthy providers.
2. *"Given this user's viewing history for a series, do they have a
   recurring daily habit, and if so, when should we pre-warm the next
   episode into an edge cache before they start watching?"* — a real,
   documented circular-statistics pattern detector (with correct
   midnight-wraparound handling) driving proactive cache pre-warming.

Everything here is a real, structurally-correct implementation of the
decision logic that matters for those two questions — a real weighted
scoring formula, a real health-threshold failover rule, real circular mean
statistics — built against small **mock CDN health feeds** and a **synthetic
viewing-history generator** instead of a real CDN telemetry pipeline or
analytics warehouse. See [Limitations](#limitations--whats-simulated).

## Table of contents

- [Architecture](#architecture)
- [Multi-CDN routing decision engine](#multi-cdn-routing-decision-engine)
- [Predictive pre-fetching engine](#predictive-pre-fetching-engine)
- [Design patterns applied](#design-patterns-applied)
- [HTTP API](#http-api)
- [Latency / accuracy targets](#latency--accuracy-targets)
- [Running it](#running-it)
- [Test output](#test-output)
- [Limitations / what's simulated](#limitations--whats-simulated)

## Architecture

```
  GET /route/{assetId} ──▶┌───────────────────────────┐◀── GET /prefetch-predictions/{userId}
                          │      httpapi (net/http)    │◀── GET /metrics, GET /healthz
                          └─────────────┬───────────────┘
                ┌──────────────────────┼──────────────────────┐
                ▼                      ▼                      ▼
      ┌──────────────────┐   ┌──────────────────┐   ┌───────────────────┐
      │      routing       │   │    cdnhealth      │   │    prediction      │
      │ (Strategy:          │◀──│ (Registry: latest  │   │ ViewingPattern-     │
      │  CdnScoringStrategy, │  │  CdnHealthReport   │   │ Predictor over      │
      │  WeightedCdnHealth-  │  │  per provider)     │   │ circularstats +     │
      │  ScoringStrategy) +   │  └──────────────────┘   │ viewinghistory       │
      │  SelectOptimalCdn-    │                          │ (synthetic dataset) │
      │  ProviderForPlayback- │                          └───────────┬─────────┘
      │  Request              │                                      │
      └──────────────────────┘                                      ▼
                                                            ┌───────────────────┐
                                                            │   circularstats     │
                                                            │ (circular mean /    │
                                                            │  stddev of time-    │
                                                            │  of-day, wraparound-│
                                                            │  safe)              │
                                                            └───────────────────┘
                          ▲
                  ┌───────────────┐
                  │    domain      │  (PlaybackRequest, CdnHealthReport,
                  │ (shared types) │   RoutingDecision, ViewingSession,
                  └───────────────┘   CachePreWarmCommand, ...)
```

Package layout:

```
cmd/
  server/          - production HTTP server entrypoint
internal/
  domain/          - shared, dependency-free data model
  cdnhealth/       - Registry ingesting periodic CDN health reports
  routing/         - Strategy CdnScoringStrategy (WeightedCdnHealthScoringStrategy)
                     + SelectOptimalCdnProviderForPlaybackRequest decision function
  circularstats/   - circular mean/stddev of time-of-day values (wraparound-safe)
  prediction/      - ViewingPatternPredictor + CachePreWarmCommand generation
  viewinghistory/  - deterministic synthetic per-user viewing-history generator
  metrics/         - Prometheus metric definitions
  httpapi/         - net/http handlers wiring it all together
```

## Multi-CDN routing decision engine

### Health data: `cdnhealth.CdnHealthRegistry`

Each mock CDN provider periodically reports a `domain.CdnHealthReport`:
rolling-window edge latency (ms), error rate (%), and cost ($/GB). The
`CdnHealthRegistry` (Registry/Repository pattern — see [Design
patterns](#design-patterns-applied)) retains only the *latest* report per
provider, mirroring how a real "current health" table works; rolling
averages are computed upstream by whatever produces the report. Reports
arrive via `IngestHealthReport` / `IngestHealthReportFeed`; tests and the
demo server's `internal/httpapi.BuildSyntheticCdnHealthFeed` simulate a
periodic monitoring feed without a real scheduler.

### The scoring formula: `WeightedCdnHealthScoringStrategy`

For a candidate provider `c` among the full set of candidates being
compared in one routing decision, each raw metric is **min-max normalized**
across that candidate set (so three metrics with wildly different
units/scales combine meaningfully, and the score adapts to whatever
providers are actually reporting right now):

```
normalizedLatency(c)   = (c.latency   - min(latency))   / (max(latency)   - min(latency))
normalizedErrorRate(c) = (c.errorRate - min(errorRate)) / (max(errorRate) - min(errorRate))
normalizedCost(c)      = (c.cost      - min(cost))      / (max(cost)      - min(cost))

score(c) = w1 * normalizedLatency(c) + w2 * normalizedErrorRate(c) + w3 * normalizedCost(c)
```

Default weights: `w1 = 0.5` (latency), `w2 = 0.3` (error rate), `w3 = 0.2`
(cost) — latency dominates because it drives perceived playback quality;
error rate is next because outright failures are worse than sub-optimal
latency but rarer in steady state; cost is weighted least, since this
service prioritizes viewer experience over margin when the two trade off.
**Lower score wins.** If every candidate shares the same raw value on a
dimension (`max == min`), that dimension's normalized term is defined as 0
for everyone (no information to discriminate on, rather than a divide-by-zero).

Worked example (also the fixture in
`internal/routing/strategy_test.go`):

| Provider | latency | errorRate | cost |
|---|---|---|---|
| CloudFront | 100ms | 1.0% | $0.05 |
| Akamai | 150ms | 2.0% | $0.03 |
| Fastly | 200ms | 0.5% | $0.04 |

Normalization ranges: latency `[100,200]`, errorRate `[0.5,2.0]`, cost `[0.03,0.05]`.

```
normLatency:  CF=0,     Akamai=0.5,   Fastly=1
normError:    CF=1/3,   Akamai=1,     Fastly=0
normCost:     CF=1,     Akamai=0,     Fastly=0.5

score(CF)     = 0.5*0   + 0.3*(1/3) + 0.2*1   = 0.30   ← winner
score(Akamai) = 0.5*0.5 + 0.3*1     + 0.2*0   = 0.55
score(Fastly) = 0.5*1   + 0.3*0     + 0.2*0.5 = 0.60
```

### Automatic failover

A provider is **unhealthy** (excluded from selection regardless of its
score) if its latest report crosses either threshold:

- `EdgeLatencyMilliseconds > 250` (`routing.MaxAcceptableEdgeLatencyMilliseconds`)
- `ErrorRatePercent > 5.0` (`routing.MaxAcceptableErrorRatePercent`)

`SelectOptimalCdnProviderForPlaybackRequest` computes a score for *every*
candidate (so the decision can explain what would have won on score alone),
then selects the lowest-scoring **healthy** candidate. If the
numerically-best-scoring provider is unhealthy,
`RoutingDecision.FailoverTriggered = true` and the reasoning text explains
both why that provider was skipped and why the actual winner was chosen. If
every known provider is unhealthy, the function returns
`ErrNoHealthyCdnProviderAvailable` rather than silently routing traffic to
a known-bad provider — in a real deployment this is a page-worthy incident
(multi-CDN redundancy has fully collapsed).

Tested in `internal/routing/strategy_test.go`: the hand-computed scoring
fixture above, an error-rate-threshold failover fixture, a
latency-threshold failover fixture, the "everyone is unhealthy" case, and
the "no health data at all" case.

## Predictive pre-fetching engine

### Why arithmetic mean fails for time-of-day: circular statistics

`internal/circularstats` computes the mean and standard deviation of a set
of clock times **correctly across the midnight boundary**. An ordinary
arithmetic mean of "seconds since midnight" gets this wrong: averaging
23:50 and 00:10 (85800s and 600s) arithmetically gives `(85800+600)/2 =
43200s` = **12:00 noon** — exactly twelve hours from either real
observation.

The fix: map each time-of-day onto the unit circle (`θ = fraction_of_day *
2π`), average the `(cos θ, sin θ)` vectors, and map the resultant vector's
angle back onto the clock:

```
x̄ = mean(cos θᵢ),  ȳ = mean(sin θᵢ)
meanAngle = atan2(ȳ, x̄)                    → converted back to a clock time
R = sqrt(x̄² + ȳ²)                          (mean resultant length, 0..1: concentration)
circularStdDevRadians = sqrt(-2 * ln(R))    (Mardia & Jupp; Fisher, 1993)
```

`R = 1` means every observation shares an identical time-of-day (zero
spread); `R → 0` means the observations are scattered with no common
time-of-day (e.g. four sessions spaced 6h apart). The circular stddev
formula is monotonic in `R` and closely approximates an ordinary linear
stddev for the small spreads (minutes, not hours) a real recurring habit
produces.

**Worked midnight-wraparound example** (from
`internal/circularstats/circularstats_test.go`): two sessions at 23:00 and
01:00, each exactly one hour from midnight on opposite sides.

```
θ(23:00) = -15°,  θ(01:00) = +15°
meanAngle = 0°  → meanTimeOfDay = 00:00:00   (correct — NOT the naive 12:00:00)
R ≈ 0.965926
circularStdDev ≈ sqrt(-2 * ln(0.965926)) / (2π) * 24h ≈ 60.35 minutes
```

### Pattern detection: `prediction.ViewingPatternPredictor`

Given a user's `[]domain.ViewingSession` history (one session per night,
each a real timestamp — see `internal/viewinghistory` for the deterministic
synthetic dataset generator), `DetectRecurringDailyViewingPattern`:

1. Requires at least `MinimumSessionsRequiredForPatternDetection` (3)
   sessions — enough to distinguish a real habit from coincidence.
2. Computes the circular mean/stddev of session start times-of-day.
3. Declares a pattern **detected** if the circular stddev is under
   `MaxCircularStdDevForPatternDetection` (45 minutes, a deliberately
   generous default — false positives here are cheap, a wasted cache-warm;
   false negatives leave a cache cold at the moment of real demand).

### Next-occurrence projection and pre-warm lead time

If a pattern is detected, `PredictNextCachePreWarmCommand` projects the
circular mean time-of-day forward to the next absolute timestamp strictly
after `now` — combining *today's* calendar date with the mean time-of-day,
and rolling forward one day only if that candidate is not still in the
future. This is midnight-wraparound-safe on the projection side too: it
compares absolute timestamps, not raw duration-since-midnight values, so a
mean time-of-day of 00:05 with `now` at 23:50 correctly predicts "15
minutes from now," not "almost 24 hours from now."

The resulting `domain.CachePreWarmCommand` sets:

```
RecommendedPreWarmAtUTC = PredictedViewingStartUTC - LeadTime
```

with `LeadTime` defaulting to 5 minutes
(`prediction.DefaultPreWarmLeadTime`) — chosen to comfortably exceed
typical edge cache-fill time for one asset's initial rendition set without
being so early the cached content risks eviction before use.

Tested in `internal/prediction/predictor_test.go` and
`internal/circularstats/circularstats_test.go`: the midnight-wraparound
fixture above, a tight-nightly-cluster detection fixture, a
scattered-sessions non-detection fixture, exact lead-time arithmetic, both
next-day-rollover and midnight-wraparound-rollover projection fixtures,
and a maximally-spread (`R≈0`) fixture confirming the stddev cap.

## Design patterns applied

- **Strategy** — `routing.CdnScoringStrategy`
  (`WeightedCdnHealthScoringStrategy` is the concrete implementation;
  `SelectOptimalCdnProviderForPlaybackRequest` is written entirely against
  the interface, so a different scoring philosophy can be swapped in
  without touching selection/failover logic).
- **Registry / Repository** — `cdnhealth.CdnHealthRegistry`: decouples "how
  health data arrives" (push feed, synthetic test feed, in a real
  deployment a Kafka consumer) from "how it's consumed" (routing only ever
  calls `LatestReportForProvider` / `LatestReportsForAllProviders`).

## HTTP API

| Endpoint | Description |
|---|---|
| `GET /route/{assetId}` | Routes a playback request. Query params: `geo=<region>` (default `us-east`), `asn=<int>`. Returns the chosen CDN provider, the scoring strategy used, whether failover was triggered, full reasoning text, and every candidate's score/health breakdown. `503` if no health data is available or every provider is unhealthy. |
| `GET /prefetch-predictions/{userId}` | Builds a deterministic synthetic 10-night viewing history for the user, runs the pattern predictor, and returns the resulting `CachePreWarmCommand` (if a pattern was detected) with reasoning. |
| `GET /metrics` | Prometheus text-exposition metrics (routing decisions, failovers, pre-warm predictions, HTTP request counts). |
| `GET /healthz` | Liveness probe. |

## Latency / accuracy targets

- **Routing decision: p99 < 5ms in-process.** The scoring/selection logic
  is pure in-memory arithmetic over at most 3 candidates — no I/O — so this
  target is dominated by noise in any real benchmark; it is included as the
  target a real edge-deployed version of this decision function would need
  to hit (multi-CDN routing decisions sit on the playback-request critical
  path).
- **Prediction accuracy: pattern detection requires ≥3 historical sessions
  and a circular stddev under 45 minutes** before this service will act on
  it — a deliberately conservative bar chosen to keep false-positive
  pre-warms cheap relative to the cost of a cold cache at real demand time
  (see [Pattern detection](#pattern-detection-predictionviewingpatternpredictor)).
  This is a policy threshold, not a measured accuracy figure — there is no
  real-world ground-truth viewing dataset behind it (see
  [Limitations](#limitations--whats-simulated)).

Reproduce the correctness fixtures behind both of the above with:

```
go test ./internal/routing/... ./internal/circularstats/... ./internal/prediction/... -v
```

## Running it

```
go build ./...
go vet ./...
go test ./... -race

# start the server (default :8080)
go run ./cmd/server

curl 'http://localhost:8080/route/stranger-things-s5e1'
curl 'http://localhost:8080/route/stranger-things-s5e1?geo=eu-west&asn=12345'
curl 'http://localhost:8080/prefetch-predictions/user-42'
curl 'http://localhost:8080/metrics'
curl 'http://localhost:8080/healthz'
```

## Test output

`go test ./... -race` (all packages, race detector on):

```
?   	.../cmd/server	[no test files]
ok  	.../internal/cdnhealth
ok  	.../internal/circularstats
?   	.../internal/domain	[no test files]
ok  	.../internal/httpapi
?   	.../internal/metrics	[no test files]
ok  	.../internal/prediction
ok  	.../internal/routing
?   	.../internal/viewinghistory	[no test files]
```

35 tests across 5 packages, all passing:

- `internal/cdnhealth` — replace-on-ingest semantics, feed ingestion,
  deterministic sorted ordering, concurrent-access race safety.
- `internal/routing` — the hand-computed scoring-formula fixture, the
  degenerate equal-candidates case, error-rate-threshold failover,
  latency-threshold failover (an exact-tie fixture engineered so the
  deterministic alphabetical tie-break exercises the unhealthy-top-scorer
  exclusion path), total-collapse and no-data error cases.
- `internal/circularstats` — identical-times (R=1, stddev≈0), the
  hand-computed midnight-wraparound fixture, a denser wraparound cluster, a
  tight nightly cluster, the maximally-spread/R≈0 capped-stddev case,
  calendar-date independence, empty-input panic behavior.
- `internal/prediction` — tight-cluster detection, scattered-session
  non-detection, too-few-sessions non-detection, a midnight-wraparound
  detection fixture, exact lead-time arithmetic, next-day rollover
  projection, midnight-wraparound rollover projection, no-pattern
  zero-value return, custom-threshold override behavior.
- `internal/httpapi` — end-to-end HTTP handler tests for all four routes,
  including the 503 error paths and a Prometheus-scrape smoke test.

## Limitations / what's simulated

- **No real CDN telemetry.** `internal/httpapi.BuildSyntheticCdnHealthFeed`
  hard-codes plausible-looking latency/error-rate/cost numbers for three
  mock providers; there is no real CDN account, edge PoP, or synthetic
  monitoring integration anywhere in this service. The periodic
  re-ingestion (`StartPeriodicSyntheticHealthFeed`) applies small
  deterministic jitter, not real measured variance.
- **No real viewing-history data store.** `internal/viewinghistory`
  synthesizes a deterministic (FNV-hash-seeded) per-user nightly-viewing
  dataset; there is no real analytics warehouse, event pipeline, or actual
  user behavior anywhere in this service. Every synthetic user has a
  similar recurring pattern by construction — the predictor's non-detection
  path is exercised only by hand-built fixtures in tests, not by the demo
  API's synthetic data generator.
- **Failover thresholds are static, not hysteresis-based.** A real
  deployment would require N consecutive breaches before failing over and M
  consecutive healthy samples before failing back, to avoid flapping when a
  provider's metrics oscillate near the threshold; this simulation uses a
  simple instantaneous per-report threshold for clarity and testability.
- **No real geo/ASN-aware routing signal.** `PlaybackRequest`'s
  `ClientGeographicRegion` / `ClientAutonomousSystemNumber` fields are
  accepted and threaded through the API but do not currently influence the
  scoring formula — a real multi-CDN router would incorporate per-ASN
  historical CDN performance as a scoring input; this is a documented
  simplification, not an oversight.
- **Circular-stddev-based pattern detection is a real, correct statistical
  method, but the 45-minute / 3-session thresholds are hand-chosen policy
  defaults**, not values tuned against a real labeled dataset of actual
  recurring-vs-coincidental viewing behavior.
- **No persistence.** Both the CDN health registry and viewing-history
  generation are entirely in-memory / computed-on-request; nothing survives
  a process restart, and there is no multi-instance state sharing.
