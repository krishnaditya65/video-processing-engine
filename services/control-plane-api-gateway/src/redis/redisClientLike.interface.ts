/**
 * @file redisClientLike.interface.ts
 * A narrow interface capturing exactly the Redis commands this service depends on
 * (a subset of ioredis's `Redis` client surface). Depending on this interface instead
 * of the concrete `ioredis` `Redis` class throughout the codebase means:
 *  1. Business logic (rate limiter, cache-aside service) can be unit tested against a
 *     trivial in-memory fake, with zero network/process dependency on a real Redis.
 *  2. The concrete client implementation (ioredis today) can be swapped later without
 *     touching consumers — an application of the Dependency Inversion Principle.
 */

/** Minimal Redis command surface consumed by this service's rate limiter and cache-aside logic. */
export interface RedisClientLike {
  /**
   * Atomically increments the integer value stored at `key` by one, creating it with
   * value 1 if absent. Returns the value after the increment.
   */
  incr(key: string): Promise<number>;

  /** Sets a time-to-live, in seconds, on `key`. Returns 1 if the timeout was set, 0 if the key does not exist. */
  expire(key: string, ttlInSeconds: number): Promise<number>;

  /** Returns the remaining time-to-live, in seconds, for `key` (-1 = no TTL set, -2 = key does not exist). */
  ttl(key: string): Promise<number>;

  /** Fetches the string value stored at `key`, or `null` if it does not exist. */
  get(key: string): Promise<string | null>;

  /** Sets `key` to `value`. When `ttlInSeconds` is provided, applies an expiry atomically (`SET key value EX ttl`). */
  set(key: string, value: string, ttlInSeconds?: number): Promise<'OK'>;

  /** Deletes one or more keys, returning the number of keys actually removed. */
  del(...keys: string[]): Promise<number>;
}
