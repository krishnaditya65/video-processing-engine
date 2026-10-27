/**
 * @file Redis-backed `ChunkProvenanceLookup` implementation.
 *
 * `InMemoryChunkProvenanceLookup` (`chunkProvenanceLookup.ts`) is documented
 * there as "not suitable for multi-instance production deployment (state is
 * not shared across service replicas)". `RedisBackedChunkProvenanceLookup`
 * is that production implementation: it stores each `ChunkProvenanceRecord`
 * as a JSON string under key `provenance:{traceId}` with a TTL, so any
 * replica in the consumer fleet can resolve a `traceId -> encodingWorkerId`
 * lookup regardless of which replica originally registered the record --
 * this is the Repository pattern referenced in README "Design Patterns"
 * applied to the provenance store (the interface is the contract;
 * `InMemoryChunkProvenanceLookup` and this class are two interchangeable
 * concrete repositories behind it).
 *
 * Follows the same Dependency Inversion approach as the
 * control-plane-api-gateway service's `RedisClientLike`
 * (`services/control-plane-api-gateway/src/redis/redisClientLike.interface.ts`):
 * rather than depending on the concrete `ioredis` `Redis` class directly,
 * this module defines its own minimal `ChunkProvenanceRedisClientLike`
 * interface capturing only the handful of commands actually used
 * (`set` with expiry, `get`, `del`). This means:
 *   1. `RedisBackedChunkProvenanceLookup` is unit-testable against a trivial
 *      in-memory fake client, with zero network/process dependency on a
 *      real Redis instance (see `tests/redisBackedChunkProvenanceLookup.test.ts`).
 *   2. The concrete client implementation (`ioredis` in production) can be
 *      swapped without touching this class -- Dependency Inversion
 *      Principle, matching the sibling service's pattern.
 *
 * `ioredis`'s real `Redis` client structurally satisfies
 * `ChunkProvenanceRedisClientLike` as-is (its `set(key, value, 'EX', ttl)`
 * and `get`/`del` methods match the shapes below), so wiring the real
 * client at the service's composition root (`src/index.ts`) requires no
 * adapter -- just `new Redis(...)` passed in directly.
 */
import type { ChunkProvenanceLookup, ChunkProvenanceRecord } from './chunkProvenanceLookup';

/**
 * Minimal Redis command surface this lookup depends on -- a narrow
 * structural subset of `ioredis`'s `Redis` client, following the same
 * pattern as the control-plane-api-gateway service's `RedisClientLike`.
 */
export interface ChunkProvenanceRedisClientLike {
  /**
   * Sets `key` to `value` with an expiry. Mirrors `ioredis`'s
   * `set(key, value, 'EX', ttlInSeconds)` call shape (the `'EX'` flag
   * argument is fixed/implied here since this lookup always sets a TTL).
   */
  set(key: string, value: string, expiryFlag: 'EX', ttlInSeconds: number): Promise<'OK' | null>;
  /** Fetches the string value stored at `key`, or `null` if absent/expired. */
  get(key: string): Promise<string | null>;
  /** Deletes one or more keys, returning the number of keys actually removed. */
  del(...keys: string[]): Promise<number>;
}

/** Redis key prefix under which chunk-provenance records are stored, keyed by traceId. */
export const CHUNK_PROVENANCE_REDIS_KEY_PREFIX = 'provenance:';

/**
 * Default TTL applied to every stored provenance record: 4 hours. Chosen to
 * match/exceed typical CDN edge cache TTLs for video chunks (per the
 * linkage-model doc comment in `chunkProvenanceLookup.ts`, the provenance
 * record only needs to remain resolvable for as long as a chunk can still
 * be actively served from CDN edge cache and therefore still generating
 * playback beacons that reference its traceId); tune to match the actual
 * CDN configuration in production.
 */
export const DEFAULT_CHUNK_PROVENANCE_TTL_SECONDS = 4 * 60 * 60;

/**
 * Redis-backed `ChunkProvenanceLookup`: production implementation suitable
 * for a multi-replica deployment, since all replicas read/write the same
 * shared Redis keyspace instead of a private in-process `Map`.
 */
export class RedisBackedChunkProvenanceLookup implements ChunkProvenanceLookup {
  /**
   * @param redisClient - Injected Redis client (or fake, in tests) satisfying `ChunkProvenanceRedisClientLike`.
   * @param ttlSeconds - TTL, in seconds, applied to every stored record; defaults to `DEFAULT_CHUNK_PROVENANCE_TTL_SECONDS`.
   */
  public constructor(
    private readonly redisClient: ChunkProvenanceRedisClientLike,
    private readonly ttlSeconds: number = DEFAULT_CHUNK_PROVENANCE_TTL_SECONDS,
  ) {}

  /** @inheritdoc */
  public async resolveChunkProvenanceForTraceId(traceId: string): Promise<ChunkProvenanceRecord | null> {
    const rawValue = await this.redisClient.get(this.buildRedisKey(traceId));
    if (rawValue === null) {
      return null;
    }
    return JSON.parse(rawValue) as ChunkProvenanceRecord;
  }

  /** @inheritdoc */
  public async registerChunkProvenanceRecord(record: ChunkProvenanceRecord): Promise<void> {
    await this.redisClient.set(this.buildRedisKey(record.traceId), JSON.stringify(record), 'EX', this.ttlSeconds);
  }

  /**
   * Removes a stored provenance record ahead of its TTL expiry. Not part of
   * the `ChunkProvenanceLookup` interface (that interface has no delete
   * operation, mirroring `InMemoryChunkProvenanceLookup`); exposed as an
   * additional method for operational/test cleanup use only.
   *
   * @param traceId - Trace id whose provenance record should be removed.
   * @returns The number of keys actually removed (0 or 1).
   */
  public async deleteChunkProvenanceRecord(traceId: string): Promise<number> {
    return this.redisClient.del(this.buildRedisKey(traceId));
  }

  /**
   * Builds the Redis key for a given traceId.
   *
   * @param traceId - Trace id to key by.
   * @returns The prefixed Redis key.
   */
  private buildRedisKey(traceId: string): string {
    return `${CHUNK_PROVENANCE_REDIS_KEY_PREFIX}${traceId}`;
  }
}
