/**
 * @file inMemoryRedisClientFake.ts
 * A minimal in-memory implementation of {@link RedisClientLike} used exclusively by
 * unit tests, so rate-limiting and cache-aside logic can be verified without a real
 * Redis server. Deliberately simple (single Map, coarse TTL handling) — it exists
 * only to exercise the contract our services depend on, not to be a faithful Redis
 * reimplementation.
 */

import { RedisClientLike } from '../redisClientLike.interface';

/** Internal record shape: the stored value plus an optional absolute expiry timestamp (ms epoch). */
interface StoredEntry {
  value: string;
  expiresAtEpochMilliseconds?: number;
}

/**
 * In-memory fake satisfying {@link RedisClientLike}, with an injectable clock so
 * TTL-dependent behaviour (expiry, `ttl()`) can be tested deterministically.
 */
export class InMemoryRedisClientFake implements RedisClientLike {
  private readonly backingStore = new Map<string, StoredEntry>();

  /** @param currentTimeProviderFunction - Injectable clock; defaults to the real `Date.now`. */
  public constructor(private readonly currentTimeProviderFunction: () => number = () => Date.now()) {}

  /** @inheritdoc */
  public async incr(key: string): Promise<number> {
    this.evictIfExpired(key);
    const existingEntry = this.backingStore.get(key);
    const nextValue = existingEntry ? Number(existingEntry.value) + 1 : 1;
    this.backingStore.set(key, { value: String(nextValue), expiresAtEpochMilliseconds: existingEntry?.expiresAtEpochMilliseconds });
    return nextValue;
  }

  /** @inheritdoc */
  public async expire(key: string, ttlInSeconds: number): Promise<number> {
    const existingEntry = this.backingStore.get(key);
    if (!existingEntry) {
      return 0;
    }
    existingEntry.expiresAtEpochMilliseconds = this.currentTimeProviderFunction() + ttlInSeconds * 1000;
    return 1;
  }

  /** @inheritdoc */
  public async ttl(key: string): Promise<number> {
    this.evictIfExpired(key);
    const existingEntry = this.backingStore.get(key);
    if (!existingEntry) {
      return -2;
    }
    if (!existingEntry.expiresAtEpochMilliseconds) {
      return -1;
    }
    return Math.ceil((existingEntry.expiresAtEpochMilliseconds - this.currentTimeProviderFunction()) / 1000);
  }

  /** @inheritdoc */
  public async get(key: string): Promise<string | null> {
    this.evictIfExpired(key);
    return this.backingStore.get(key)?.value ?? null;
  }

  /** @inheritdoc */
  public async set(key: string, value: string, ttlInSeconds?: number): Promise<'OK'> {
    this.backingStore.set(key, {
      value,
      expiresAtEpochMilliseconds: ttlInSeconds !== undefined ? this.currentTimeProviderFunction() + ttlInSeconds * 1000 : undefined,
    });
    return 'OK';
  }

  /** @inheritdoc */
  public async del(...keys: string[]): Promise<number> {
    let deletedCount = 0;
    for (const key of keys) {
      if (this.backingStore.delete(key)) {
        deletedCount += 1;
      }
    }
    return deletedCount;
  }

  /** Removes `key` from the store if its TTL (per the injected clock) has lapsed. */
  private evictIfExpired(key: string): void {
    const existingEntry = this.backingStore.get(key);
    if (existingEntry?.expiresAtEpochMilliseconds && existingEntry.expiresAtEpochMilliseconds <= this.currentTimeProviderFunction()) {
      this.backingStore.delete(key);
    }
  }
}
