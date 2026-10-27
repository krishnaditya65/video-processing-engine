/**
 * @file rateLimitExceededException.ts
 * NestJS HTTP exception thrown when a caller exceeds the configured Redis-backed
 * rate limit. Mapped to HTTP 429 (Too Many Requests) so it surfaces correctly through
 * both the GraphQL error extensions and any REST-style callers.
 */

import { HttpException, HttpStatus } from '@nestjs/common';

/** Thrown by {@link RedisBackedRateLimitingGuard} when a rate-limit window is exhausted. */
export class RateLimitExceededException extends HttpException {
  /**
   * @param rateLimitIdentityKey - The key (e.g. `user:123` or `ip:1.2.3.4`) that exceeded its limit, included for observability.
   * @param retryAfterInSeconds - Seconds the caller should wait before retrying, surfaced to clients for backoff.
   */
  public constructor(rateLimitIdentityKey: string, public readonly retryAfterInSeconds: number) {
    super(
      {
        statusCode: HttpStatus.TOO_MANY_REQUESTS,
        message: `Rate limit exceeded for identity "${rateLimitIdentityKey}". Retry after ${retryAfterInSeconds} second(s).`,
        error: 'Too Many Requests',
      },
      HttpStatus.TOO_MANY_REQUESTS,
    );
  }
}
