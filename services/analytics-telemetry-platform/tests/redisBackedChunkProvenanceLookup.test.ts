/**
 * @file Unit tests for `RedisBackedChunkProvenanceLookup`
 * (`src/tracing/redisBackedChunkProvenanceLookup.ts`), exercised against a
 * fake in-memory `ChunkProvenanceRedisClientLike` implementation (mirroring
 * the control-plane-api-gateway service's `RedisClientLike` fake-testing
 * pattern) -- no real Redis connection is used.
 */
import {
  RedisBackedChunkProvenanceLookup,
  CHUNK_PROVENANCE_REDIS_KEY_PREFIX,
  DEFAULT_CHUNK_PROVENANCE_TTL_SECONDS,
  type ChunkProvenanceRedisClientLike,
} from '../src/tracing/redisBackedChunkProvenanceLookup';
import type { ChunkProvenanceRecord } from '../src/tracing/chunkProvenanceLookup';

/** A trivial in-memory fake satisfying `ChunkProvenanceRedisClientLike`, for tests only. */
class FakeChunkProvenanceRedisClient implements ChunkProvenanceRedisClientLike {
  public readonly store = new Map<string, { value: string; expiresAtEpochSeconds: number }>();
  public readonly setCalls: Array<{ key: string; value: string; ttlSeconds: number }> = [];

  public constructor(private readonly nowEpochSeconds: () => number = () => Date.now() / 1000) {}

  public async set(key: string, value: string, expiryFlag: 'EX', ttlInSeconds: number): Promise<'OK' | null> {
    expect(expiryFlag).toBe('EX');
    this.store.set(key, { value, expiresAtEpochSeconds: this.nowEpochSeconds() + ttlInSeconds });
    this.setCalls.push({ key, value, ttlSeconds: ttlInSeconds });
    return 'OK';
  }

  public async get(key: string): Promise<string | null> {
    const entry = this.store.get(key);
    if (entry === undefined) {
      return null;
    }
    if (entry.expiresAtEpochSeconds <= this.nowEpochSeconds()) {
      this.store.delete(key);
      return null;
    }
    return entry.value;
  }

  public async del(...keys: string[]): Promise<number> {
    let removed = 0;
    for (const key of keys) {
      if (this.store.delete(key)) {
        removed += 1;
      }
    }
    return removed;
  }
}

const sampleRecord: ChunkProvenanceRecord = {
  traceId: 'trace-abc-123',
  encodingWorkerId: 'encoding-worker-7',
  chunkId: 'chunk-000042',
  encodedAtEpochMillis: 1_700_000_000_000,
};

describe('RedisBackedChunkProvenanceLookup', () => {
  it('resolves null for a traceId with no registered record', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis);

    expect(await lookup.resolveChunkProvenanceForTraceId('unknown-trace')).toBeNull();
  });

  it('registers a record and resolves it back, round-tripping through JSON serialization', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis);

    await lookup.registerChunkProvenanceRecord(sampleRecord);
    const resolved = await lookup.resolveChunkProvenanceForTraceId('trace-abc-123');

    expect(resolved).toEqual(sampleRecord);
  });

  it('stores the record under the documented key prefix with the default TTL', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis);

    await lookup.registerChunkProvenanceRecord(sampleRecord);

    expect(fakeRedis.setCalls).toHaveLength(1);
    expect(fakeRedis.setCalls[0]?.key).toBe(`${CHUNK_PROVENANCE_REDIS_KEY_PREFIX}trace-abc-123`);
    expect(fakeRedis.setCalls[0]?.ttlSeconds).toBe(DEFAULT_CHUNK_PROVENANCE_TTL_SECONDS);
    expect(JSON.parse(fakeRedis.setCalls[0]?.value ?? '{}')).toEqual(sampleRecord);
  });

  it('honors a custom TTL when provided', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis, 60);

    await lookup.registerChunkProvenanceRecord(sampleRecord);

    expect(fakeRedis.setCalls[0]?.ttlSeconds).toBe(60);
  });

  it('resolves null once the record has expired past its TTL (simulated via a fake clock)', async () => {
    let currentEpochSeconds = 1_000_000;
    const fakeRedis = new FakeChunkProvenanceRedisClient(() => currentEpochSeconds);
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis, 60);

    await lookup.registerChunkProvenanceRecord(sampleRecord);
    expect(await lookup.resolveChunkProvenanceForTraceId('trace-abc-123')).toEqual(sampleRecord);

    currentEpochSeconds += 61; // past the 60s TTL
    expect(await lookup.resolveChunkProvenanceForTraceId('trace-abc-123')).toBeNull();
  });

  it('deleteChunkProvenanceRecord removes a stored record ahead of TTL expiry', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis);

    await lookup.registerChunkProvenanceRecord(sampleRecord);
    const removedCount = await lookup.deleteChunkProvenanceRecord('trace-abc-123');

    expect(removedCount).toBe(1);
    expect(await lookup.resolveChunkProvenanceForTraceId('trace-abc-123')).toBeNull();
  });

  it('supports multiple independent traceIds without collision', async () => {
    const fakeRedis = new FakeChunkProvenanceRedisClient();
    const lookup = new RedisBackedChunkProvenanceLookup(fakeRedis);

    const secondRecord: ChunkProvenanceRecord = {
      traceId: 'trace-xyz-789',
      encodingWorkerId: 'encoding-worker-9',
      chunkId: 'chunk-000099',
      encodedAtEpochMillis: 1_700_000_005_000,
    };

    await lookup.registerChunkProvenanceRecord(sampleRecord);
    await lookup.registerChunkProvenanceRecord(secondRecord);

    expect(await lookup.resolveChunkProvenanceForTraceId('trace-abc-123')).toEqual(sampleRecord);
    expect(await lookup.resolveChunkProvenanceForTraceId('trace-xyz-789')).toEqual(secondRecord);
  });
});
