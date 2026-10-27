/**
 * @file Batch-insert stage of the beacon ingestion Pipeline pattern, and
 * the micro-batching accumulator that feeds it.
 *
 * Fourth/final stage of validate -> normalize -> enrich -> batch-insert.
 * `BeaconMicroBatchAccumulator` implements the size/time dual-trigger
 * micro-batching strategy described in README "Ingestion Throughput
 * Targets": it buffers enriched beacons in memory and flushes to
 * `PlaybackBeaconRepository.insertEnrichedPlaybackBeaconBatch` whenever
 * either the configured row-count threshold or the configured time
 * threshold is reached, whichever comes first.
 */
import type { PlaybackBeaconRepository } from '../../clickhouse/playbackBeaconRepository';
import type { EnrichedPlaybackBeacon } from '../../types/beaconTypes';

/** Configuration for `BeaconMicroBatchAccumulator`'s flush triggers. */
export interface BeaconMicroBatchAccumulatorConfig {
  /** Flush once the in-memory buffer reaches this many beacons. */
  maxBatchSizeRows: number;
  /** Flush once this many milliseconds have elapsed since the oldest buffered beacon arrived. */
  maxBatchLingerMillis: number;
}

/**
 * Accumulates enriched beacons in memory and flushes them to ClickHouse via
 * `PlaybackBeaconRepository` once either a row-count or a linger-time
 * threshold is crossed -- the standard micro-batching pattern used to trade
 * a small amount of write latency for dramatically higher ClickHouse
 * insert throughput (ClickHouse strongly prefers few, large inserts over
 * many, small ones -- see README "Ingestion Throughput Targets" for the
 * documented tradeoff and target numbers).
 */
export class BeaconMicroBatchAccumulator {
  private bufferedBeacons: EnrichedPlaybackBeacon[] = [];
  private oldestBufferedBeaconEnqueuedAtEpochMillis: number | null = null;
  private lingerFlushTimer: NodeJS.Timeout | null = null;

  /**
   * @param playbackBeaconRepository - Repository used to persist flushed
   * batches.
   * @param config - Flush-trigger configuration.
   * @param clock - Injectable clock function (defaults to `Date.now`).
   */
  public constructor(
    private readonly playbackBeaconRepository: PlaybackBeaconRepository,
    private readonly config: BeaconMicroBatchAccumulatorConfig,
    private readonly clock: () => number = Date.now,
  ) {}

  /**
   * Adds an enriched beacon to the in-memory buffer, triggering an
   * immediate synchronous flush if the row-count threshold is reached, and
   * (re)arming the linger timer otherwise.
   *
   * @param beacon - The enriched beacon to buffer.
   */
  public async addBeaconToBatch(beacon: EnrichedPlaybackBeacon): Promise<void> {
    if (this.bufferedBeacons.length === 0) {
      this.oldestBufferedBeaconEnqueuedAtEpochMillis = this.clock();
      this.armLingerFlushTimer();
    }

    this.bufferedBeacons.push(beacon);

    if (this.bufferedBeacons.length >= this.config.maxBatchSizeRows) {
      await this.flushBufferedBatchToClickHouse();
    }
  }

  /**
   * Immediately flushes the current buffer (if non-empty) to ClickHouse,
   * regardless of whether either trigger threshold has been reached. Used
   * both by the internal triggers and for graceful-shutdown draining.
   */
  public async flushBufferedBatchToClickHouse(): Promise<void> {
    this.clearLingerFlushTimer();

    if (this.bufferedBeacons.length === 0) {
      return;
    }

    const batchToFlush = this.bufferedBeacons;
    this.bufferedBeacons = [];
    this.oldestBufferedBeaconEnqueuedAtEpochMillis = null;

    await this.playbackBeaconRepository.insertEnrichedPlaybackBeaconBatch(batchToFlush);
  }

  /** @returns The number of beacons currently buffered but not yet flushed. */
  public getCurrentBufferSize(): number {
    return this.bufferedBeacons.length;
  }

  private armLingerFlushTimer(): void {
    this.clearLingerFlushTimer();
    this.lingerFlushTimer = setTimeout(() => {
      void this.flushBufferedBatchToClickHouse();
    }, this.config.maxBatchLingerMillis);
    this.lingerFlushTimer.unref?.();
  }

  private clearLingerFlushTimer(): void {
    if (this.lingerFlushTimer !== null) {
      clearTimeout(this.lingerFlushTimer);
      this.lingerFlushTimer = null;
    }
  }
}
