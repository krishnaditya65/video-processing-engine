/**
 * @file Repository pattern implementation for ClickHouse-backed access to
 * the `telemetry_lake.playback_beacons` table.
 *
 * Encapsulates all SQL/insert-shape knowledge behind a typed method
 * surface (`insertEnrichedPlaybackBeaconBatch`,
 * `queryRebufferingRatioForAssetOverTimeRange`, etc.) so callers never
 * construct raw SQL themselves. This is the canonical Repository-pattern
 * boundary referenced in README's "Design Patterns" section: callers
 * (ingestion pipeline, QoE dashboard API handlers) depend only on this
 * interface, not on `@clickhouse/client` directly, which keeps ClickHouse
 * swappable and keeps every non-repository file free of SQL strings.
 *
 * Structurally correct against the real `@clickhouse/client` API
 * (`.insert`, `.query`) but -- as with `clickhouseClient.ts` -- not
 * exercised against a live cluster by the automated test suite. See
 * README.md "Testing Strategy".
 */
import type { ClickHouseClient } from '@clickhouse/client';
import type { EnrichedPlaybackBeacon } from '../types/beaconTypes';

/** Row shape returned by `queryRebufferingRatioForAssetOverTimeRange`. */
export interface RebufferingRatioQueryRow {
  assetId: string;
  rebufferEventCount: string;
  heartbeatEventCount: string;
}

/** Row shape returned by `queryTimeToFirstFramePercentilesForAsset`. */
export interface TimeToFirstFramePercentileQueryRow {
  assetId: string;
  p50TimeToFirstFrameMillis: number;
  p95TimeToFirstFrameMillis: number;
  p99TimeToFirstFrameMillis: number;
}

/**
 * Repository providing parameterized insert and query access to the
 * `playback_beacons` ClickHouse table. Consumers should depend on this
 * class (or, in a fuller DI setup, an interface it implements) rather than
 * touching `ClickHouseClient` directly.
 */
export class PlaybackBeaconRepository {
  /**
   * @param clickHouseClient - Configured `ClickHouseClient` instance, from
   * `createConfiguredClickHouseClient`.
   * @param tableName - Fully qualified target table name (defaults to the
   * production table defined in `sql/schema.sql`).
   */
  public constructor(
    private readonly clickHouseClient: ClickHouseClient,
    private readonly tableName: string = 'telemetry_lake.playback_beacons',
  ) {}

  /**
   * Batch-inserts a micro-batch of enriched playback beacons using
   * ClickHouse's native JSON insert format. This is the sole write path
   * used by the ingestion pipeline's batch-insert stage
   * (`beaconBatchInsertStage.ts`); see README "Ingestion Throughput
   * Targets" for the batch-size/flush-interval tuning this call site is
   * designed around.
   *
   * @param beacons - Non-empty array of enriched beacons to persist.
   * @throws {Error} If `beacons` is empty (callers should not invoke this
   * with an empty batch; the pipeline's flush trigger guarantees this).
   */
  public async insertEnrichedPlaybackBeaconBatch(beacons: EnrichedPlaybackBeacon[]): Promise<void> {
    if (beacons.length === 0) {
      throw new Error('insertEnrichedPlaybackBeaconBatch was called with an empty batch');
    }

    const rows = beacons.map((beacon) => ({
      beaconId: beacon.beaconId,
      sessionId: beacon.sessionId,
      assetId: beacon.assetId,
      userId: beacon.userId,
      traceId: beacon.traceId,
      sourceEncodingWorkerId: beacon.sourceEncodingWorkerId,
      eventType: beacon.eventType,
      eventTimestamp: formatEpochMillisAsClickHouseDateTime64(beacon.eventTimestampEpochMillis),
      ingestedAt: formatEpochMillisAsClickHouseDateTime64(beacon.ingestedAtEpochMillis),
      timeToFirstFrameMillis: beacon.timeToFirstFrameMillis,
      rebufferDurationMillis: beacon.rebufferDurationMillis,
      bitrateKbps: beacon.bitrateKbps,
      renditionId: beacon.renditionId,
      frameDropCount: beacon.frameDropCount,
      playbackPositionSeconds: beacon.playbackPositionSeconds,
      assetDurationSeconds: beacon.assetDurationSeconds,
      decodeErrorCode: beacon.decodeErrorCode,
      decodeErrorMessage: beacon.decodeErrorMessage,
      hardwareDecoderModel: beacon.hardwareDecoderModel,
      deviceFirmwareVersion: beacon.deviceFirmwareVersion,
      deviceOperatingSystem: beacon.deviceOperatingSystem,
      networkAutonomousSystemNumber: beacon.networkAutonomousSystemNumber,
      internetServiceProviderName: beacon.internetServiceProviderName,
    }));

    await this.clickHouseClient.insert({
      table: this.tableName,
      values: rows,
      format: 'JSONEachRow',
    });
  }

  /**
   * Queries raw rebuffer/heartbeat event counts for an asset over a time
   * range, the raw inputs `computeRebufferingRatioFromPlaybackBeaconBatch`
   * needs when computed server-side over already-aggregated ClickHouse
   * data rather than from an in-process beacon array.
   *
   * @param assetId - Target asset identifier.
   * @param startTimeIso - Inclusive ISO-8601 range start.
   * @param endTimeIso - Exclusive ISO-8601 range end.
   * @returns Parameterized query result rows.
   */
  public async queryRebufferingRatioForAssetOverTimeRange(
    assetId: string,
    startTimeIso: string,
    endTimeIso: string,
  ): Promise<RebufferingRatioQueryRow[]> {
    const resultSet = await this.clickHouseClient.query({
      query: `
        SELECT
          assetId,
          countIf(eventType = 'rebuffer_end') AS rebufferEventCount,
          countIf(eventType = 'heartbeat') AS heartbeatEventCount
        FROM ${this.tableName}
        WHERE assetId = {assetId:String}
          AND eventTimestamp >= {startTime:DateTime64(3)}
          AND eventTimestamp <  {endTime:DateTime64(3)}
        GROUP BY assetId
      `,
      query_params: { assetId, startTime: startTimeIso, endTime: endTimeIso },
      format: 'JSONEachRow',
    });
    return resultSet.json<RebufferingRatioQueryRow>();
  }

  /**
   * Queries p50/p95/p99 Time-To-First-Frame percentiles for an asset over
   * a time range using ClickHouse's native `quantile` aggregate functions.
   *
   * @param assetId - Target asset identifier.
   * @param startTimeIso - Inclusive ISO-8601 range start.
   * @param endTimeIso - Exclusive ISO-8601 range end.
   * @returns Parameterized query result rows (typically zero or one row).
   */
  public async queryTimeToFirstFramePercentilesForAsset(
    assetId: string,
    startTimeIso: string,
    endTimeIso: string,
  ): Promise<TimeToFirstFramePercentileQueryRow[]> {
    const resultSet = await this.clickHouseClient.query({
      query: `
        SELECT
          assetId,
          quantile(0.50)(timeToFirstFrameMillis) AS p50TimeToFirstFrameMillis,
          quantile(0.95)(timeToFirstFrameMillis) AS p95TimeToFirstFrameMillis,
          quantile(0.99)(timeToFirstFrameMillis) AS p99TimeToFirstFrameMillis
        FROM ${this.tableName}
        WHERE assetId = {assetId:String}
          AND eventType = 'first_frame'
          AND eventTimestamp >= {startTime:DateTime64(3)}
          AND eventTimestamp <  {endTime:DateTime64(3)}
        GROUP BY assetId
      `,
      query_params: { assetId, startTime: startTimeIso, endTime: endTimeIso },
      format: 'JSONEachRow',
    });
    return resultSet.json<TimeToFirstFramePercentileQueryRow>();
  }
}

/**
 * Formats an epoch-milliseconds numeric timestamp as a ClickHouse
 * `DateTime64(3, 'UTC')`-compatible string (`"YYYY-MM-DD HH:MM:SS.mmm"`).
 *
 * @param epochMillis - Epoch-milliseconds timestamp.
 * @returns ClickHouse-formatted datetime string.
 */
function formatEpochMillisAsClickHouseDateTime64(epochMillis: number): string {
  return new Date(epochMillis).toISOString().replace('T', ' ').replace('Z', '');
}
