/**
 * @file redisRateLimiterService.ts
 * Redis-backed fixed-window rate limiter. Implements the classic
 * `INCR` + conditional `EXPIRE` pattern: the first request in a window creates the
 * counter and stamps its TTL; subsequent requests within the same window merely
 * increment it; once the TTL lapses Redis expires the key and the window resets.
 *
 * Fixed-window is chosen over sliding-window/token-bucket for simplicity and O(1)
 * Redis round trips; the trade-off (allowing up to 2x the limit across a window
 * boundary burst) is called out in SERVICE_README.md as a known limitation.
 */

import { Injectable } from '@nestjs/common';
import { RedisClientLike } from './redisClientLike.interface';

/** Result of a single rate-limit check, returned to guards/callers to decide allow/deny. */
export interface RateLimitCheckResult {
  /** Whether the request is allowed to proceed under the current window's count. */
  isRequestPermitted: boolean;
  /** Number of requests already recorded in the current window (including this one, if permitted). */
  currentRequestCountInWindow: number;
  /** The configured maximum requests allowed per window, echoed for client visibility. */
  maximumRequestsPerWindow: number;
  /** Seconds remaining until the current window resets — used to populate `Retry-After`. */
  secondsRemainingInCurrentWindow: number;
}

/**
 * Injectable service implementing Redis-backed fixed-window rate limiting. Kept
 * independent of any specific NestJS guard/transport so it can be reused from GraphQL
 * guards, REST middleware, or plain service-to-service checks alike.
 */
@Injectable()
export class RedisRateLimiterService {
  public constructor(private readonly redisClient: RedisClientLike) {}

  /**
   * Records one request attempt for `rateLimitIdentityKey` and reports whether it is
   * within the configured limit for the current fixed window.
   *
   * Edge cases:
   * - First request for a key in a window: counter is created via `INCR` (returns 1)
   *   and a TTL is attached with `EXPIRE`; without this the key would live forever.
   * - Concurrent first requests (race on "is this the first increment"): checking
   *   `count === 1` right after `INCR` is safe because `INCR` is atomic in Redis, so
   *   exactly one caller observes `count === 1` and sets the TTL, even under concurrency.
   * - Key expires mid-check (TTL just lapsed): `ttl()` may return -2 briefly; guarded
   *   by falling back to the configured window duration when TTL is missing/negative.
   *
   * @param rateLimitIdentityKey - Stable identity for the caller being limited, e.g. `user:<id>` or `ip:<addr>`.
   * @param maximumRequestsPerWindow - Maximum permitted requests within one window.
   * @param windowDurationInSeconds - Length of the fixed window, in seconds.
   * @returns A {@link RateLimitCheckResult} describing whether the request is permitted.
   */
  public async recordRequestAttemptAndCheckRateLimit(
    rateLimitIdentityKey: string,
    maximumRequestsPerWindow: number,
    windowDurationInSeconds: number,
  ): Promise<RateLimitCheckResult> {
    const redisCounterKey = this.buildRateLimitCounterRedisKey(rateLimitIdentityKey);
    const currentRequestCountInWindow = await this.redisClient.incr(redisCounterKey);

    if (currentRequestCountInWindow === 1) {
      // This call created the counter: attach the window TTL exactly once.
      await this.redisClient.expire(redisCounterKey, windowDurationInSeconds);
    }

    const remainingTtlInSeconds = await this.redisClient.ttl(redisCounterKey);
    const secondsRemainingInCurrentWindow = remainingTtlInSeconds > 0 ? remainingTtlInSeconds : windowDurationInSeconds;

    return {
      isRequestPermitted: currentRequestCountInWindow <= maximumRequestsPerWindow,
      currentRequestCountInWindow,
      maximumRequestsPerWindow,
      secondsRemainingInCurrentWindow,
    };
  }

  /**
   * Builds the Redis key used to store a given identity's request counter, namespaced
   * to avoid collisions with other key spaces (manifest cache, session state, etc.).
   * @param rateLimitIdentityKey - Caller-supplied identity key.
   * @returns The fully-namespaced Redis key.
   */
  private buildRateLimitCounterRedisKey(rateLimitIdentityKey: string): string {
    return `rate-limit:{${rateLimitIdentityKey}}`;
  }
}
