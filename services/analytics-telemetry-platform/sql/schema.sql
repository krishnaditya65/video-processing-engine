-- =============================================================================
-- ClickHouse DDL: playback_beacons columnar telemetry table
-- Service: analytics-telemetry-platform
--
-- Design rationale (see README.md "ClickHouse Schema Design Rationale" for
-- the full write-up) summarized inline here:
--
-- 1. ENGINE = MergeTree: the standard workhorse table engine for
--    append-heavy, rarely-updated analytical event data. We do not need
--    ReplacingMergeTree/CollapsingMergeTree because beacons are immutable,
--    write-once facts (no upserts/corrections modeled at this layer).
--
-- 2. PARTITION BY toYYYYMMDD(eventTimestamp): daily partitions. This keeps
--    each partition's on-disk part count and size manageable at expected
--    volume (~2M beacons/sec peak => ~170B rows/day at full global peak
--    sustained, though realistic sustained average is far lower -- see
--    README ingestion throughput targets), and lets ClickHouse prune whole
--    partitions instantly for the extremely common "last N days" dashboard
--    query pattern, and lets TTL-based retention (see TTL clause below)
--    drop entire partitions cheaply instead of deleting individual rows.
--
-- 3. ORDER BY (assetId, toStartOfHour(eventTimestamp), eventType, beaconId):
--    this is the single most consequential design decision in the schema.
--    ClickHouse physically sorts each part's rows by this tuple and builds
--    its sparse primary index over it. We choose (assetId, hour, eventType)
--    as the leading key because:
--      a. The overwhelmingly dominant dashboard/query access pattern is
--         "give me QoE metrics for asset X over time range Y" (per-title
--         playback health dashboards, title-launch war rooms) -- so
--         assetId as the leading column lets ClickHouse's sparse index
--         skip granules for all other assets entirely.
--      b. Bucketing by toStartOfHour(eventTimestamp) as the second key
--         column, *within* an assetId, means eventTimestamp values are
--         monotonically increasing runs within each (assetId, hour)
--         bucket. This is the classic delta-of-delta / delta encoding
--         sweet spot: consecutive timestamp values in a sorted run differ
--         by small, similar deltas, so ClickHouse's `Delta` and
--         `DoubleDelta` codecs (or the default LZ4/ZSTD generic
--         compressors, which also benefit enormously from low local
--         entropy in a sorted numeric column) compress the eventTimestamp
--         column extremely well -- often 10-20x smaller than an
--         unsorted/random-order equivalent. The same benefit compounds for
--         monotonically-drifting numeric columns like playbackPositionSeconds
--         when queries scan within a session.
--      c. eventType and beaconId as trailing key columns keep rows
--         belonging to the same logical sub-event grouped and give a
--         final uniqueness tiebreaker for the sparse index without materially
--         hurting compression of the leading columns.
--
-- 4. Explicit CODEC choices on hot numeric/timestamp columns lean on
--    Delta/DoubleDelta + ZSTD, which is the standard ClickHouse
--    time-series-compression recipe and is what the ORDER BY key above is
--    specifically chosen to make effective (delta encoding only pays off
--    when consecutive on-disk values are already numerically close, which
--    the sort order guarantees).
--
-- 5. LowCardinality(String) is used for bounded-cardinality dimension
--    columns (eventType, deviceOperatingSystem, hardwareDecoderModel,
--    internetServiceProviderName, deviceFirmwareVersion) -- ClickHouse
--    dictionary-encodes these, which both shrinks storage and speeds up
--    GROUP BY / filtering on them (exactly the columns the A/B test
--    evaluator and anomaly detector group by).
--
-- 6. TTL: telemetry at this volume cannot be retained indefinitely at full
--    row grain. We TTL raw rows out after 90 days (tunable), consistent
--    with the platform's typical "hot" analytical retention window;
--    longer-term trends are expected to be served from pre-aggregated
--    materialized views/rollup tables (not modeled in this DDL, but the
--    pattern is noted in README as a documented extension point).
-- =============================================================================

CREATE DATABASE IF NOT EXISTS telemetry_lake;

CREATE TABLE IF NOT EXISTS telemetry_lake.playback_beacons
(
    -- Identifiers -------------------------------------------------------
    beaconId               String,
    sessionId               String,
    assetId                  String,
    userId                   Nullable(String),
    traceId                  Nullable(String),
    sourceEncodingWorkerId   Nullable(String),

    -- Event classification ----------------------------------------------
    eventType                LowCardinality(String),

    -- Timing --------------------------------------------------------------
    eventTimestamp           DateTime64(3, 'UTC') CODEC(DoubleDelta, ZSTD(3)),
    ingestedAt               DateTime64(3, 'UTC') DEFAULT now64(3) CODEC(DoubleDelta, ZSTD(3)),

    -- QoE metric inputs ---------------------------------------------------
    timeToFirstFrameMillis   Nullable(UInt32) CODEC(Delta, ZSTD(1)),
    rebufferDurationMillis   Nullable(UInt32) CODEC(Delta, ZSTD(1)),
    bitrateKbps              Nullable(UInt32) CODEC(Delta, ZSTD(1)),
    renditionId              Nullable(LowCardinality(String)),
    frameDropCount           Nullable(UInt32) CODEC(Delta, ZSTD(1)),
    playbackPositionSeconds  Nullable(Float32) CODEC(Delta, ZSTD(1)),
    assetDurationSeconds     Nullable(Float32),

    -- Decoder / hardware anomaly detection inputs --------------------------
    decodeErrorCode          Nullable(LowCardinality(String)),
    decodeErrorMessage       Nullable(String),
    hardwareDecoderModel     Nullable(LowCardinality(String)),
    deviceFirmwareVersion    Nullable(LowCardinality(String)),
    deviceOperatingSystem    LowCardinality(String),

    -- Network dimensions ----------------------------------------------------
    networkAutonomousSystemNumber Nullable(UInt32),
    internetServiceProviderName   Nullable(LowCardinality(String))
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(eventTimestamp)
ORDER BY (assetId, toStartOfHour(eventTimestamp), eventType, beaconId)
TTL toDateTime(eventTimestamp) + INTERVAL 90 DAY DELETE
SETTINGS index_granularity = 8192;

-- -----------------------------------------------------------------------------
-- Example downstream materialized view sketch (documented, not required for
-- the service to function): hourly per-asset QoE rollup, kept forever (or a
-- much longer TTL than raw rows), to serve long-range trend dashboards
-- cheaply without scanning raw beacon rows.
-- -----------------------------------------------------------------------------
-- CREATE MATERIALIZED VIEW IF NOT EXISTS telemetry_lake.playback_qoe_hourly_rollup
-- ENGINE = SummingMergeTree
-- PARTITION BY toYYYYMM(hourBucket)
-- ORDER BY (assetId, hourBucket)
-- AS
-- SELECT
--     assetId,
--     toStartOfHour(eventTimestamp) AS hourBucket,
--     countIf(eventType = 'rebuffer_end') AS rebufferEventCount,
--     countIf(eventType = 'heartbeat') AS heartbeatEventCount,
--     avgIf(bitrateKbps, bitrateKbps IS NOT NULL) AS avgBitrateKbps
-- FROM telemetry_lake.playback_beacons
-- GROUP BY assetId, hourBucket;
