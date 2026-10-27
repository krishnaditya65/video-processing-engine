/**
 * @file manifestCacheAsideService.ts
 * Implements the cache-aside (a.k.a. lazy-loading) pattern for streaming manifest
 * metadata (e.g. resolved HLS/DASH manifest URLs + variant/bitrate ladders keyed by
 * asset + device capability profile). On a cache miss, the caller-supplied loader
 * function is invoked to compute the authoritative value (in production, this would
 * query the packaging/origin service), the result is written back to Redis with a
 * TTL, and then returned — so subsequent lookups within the TTL window are served
 * entirely from Redis without recomputation.
 *
 * Why cache-aside (vs. write-through/read-through at the datastore layer): manifest
 * resolution here depends on caller context (device capability, network hints) that
 * isn't naturally modeled as a single row in a backing store, so the application
 * explicitly owns population — the canonical shape of the cache-aside pattern.
 */

import { Injectable } from '@nestjs/common';
import { RedisClientLike } from './redisClientLike.interface';

/** Outcome metadata returned alongside the resolved value, useful for logging/metrics (cache hit ratio). */
export interface CacheAsideResolutionResult<TValue> {
  /** The resolved value, whether served from cache or freshly computed. */
  resolvedValue: TValue;
  /** `true` if the value was served from Redis without invoking the loader; `false` if the loader ran. */
  wasServedFromCache: boolean;
}

/**
 * Generic cache-aside helper specialised for JSON-serialisable manifest metadata
 * values. Not tied to any single manifest shape (`TValue` is generic) so it can cache
 * both the "resolved manifest URL" lookups and richer structural-map style payloads.
 */
@Injectable()
export class ManifestCacheAsideService {
  public constructor(private readonly redisClient: RedisClientLike) {}

  /**
   * Resolves `cacheKey` via the cache-aside pattern: try Redis first; on a miss, call
   * `loadAuthoritativeValueFunction`, populate Redis with the result (subject to
   * `timeToLiveInSeconds`), then return it.
   *
   * Edge cases:
   * - Cache hit but value fails JSON parsing (corrupted/incompatible entry from a
   *   prior schema version): treated as a miss — falls through to recompute and
   *   overwrite the bad entry, rather than throwing and failing the whole request.
   * - Loader throws: the exception propagates to the caller and nothing is written
   *   to the cache (we never cache failures).
   * - `timeToLiveInSeconds <= 0`: the value is still returned but intentionally not
   *   persisted to Redis, since a non-positive TTL has no meaningful caching effect.
   *
   * @param cacheKey - Fully-namespaced Redis key identifying this manifest lookup.
   * @param loadAuthoritativeValueFunction - Invoked on a cache miss to compute the authoritative value.
   * @param timeToLiveInSeconds - How long the freshly computed value should remain cached.
   * @returns A {@link CacheAsideResolutionResult} wrapping the resolved value and whether it was a cache hit.
   */
  public async resolveWithCacheAsideStrategy<TValue>(
    cacheKey: string,
    loadAuthoritativeValueFunction: () => Promise<TValue>,
    timeToLiveInSeconds: number,
  ): Promise<CacheAsideResolutionResult<TValue>> {
    const cachedRawValue = await this.redisClient.get(cacheKey);
    if (cachedRawValue !== null) {
      const parsedCachedValue = this.tryParseJsonOrUndefined<TValue>(cachedRawValue);
      if (parsedCachedValue !== undefined) {
        return { resolvedValue: parsedCachedValue, wasServedFromCache: true };
      }
      // Corrupted/incompatible cache entry: fall through and recompute below.
    }

    const freshlyComputedValue = await loadAuthoritativeValueFunction();

    if (timeToLiveInSeconds > 0) {
      await this.redisClient.set(cacheKey, JSON.stringify(freshlyComputedValue), timeToLiveInSeconds);
    }

    return { resolvedValue: freshlyComputedValue, wasServedFromCache: false };
  }

  /**
   * Explicitly evicts a manifest cache entry, e.g. when an asset's encoding output
   * changes and previously cached manifest URLs would now be stale.
   * @param cacheKey - The key to evict.
   * @returns The number of keys actually removed (0 or 1).
   */
  public async invalidateCachedManifestEntry(cacheKey: string): Promise<number> {
    return this.redisClient.del(cacheKey);
  }

  /**
   * Safely parses a JSON string, returning `undefined` instead of throwing on
   * malformed input so callers can treat parse failures as cache misses.
   * @param rawJsonValue - The raw string read from Redis.
   * @returns The parsed value, or `undefined` if parsing failed.
   */
  private tryParseJsonOrUndefined<TValue>(rawJsonValue: string): TValue | undefined {
    try {
      return JSON.parse(rawJsonValue) as TValue;
    } catch {
      return undefined;
    }
  }
}
