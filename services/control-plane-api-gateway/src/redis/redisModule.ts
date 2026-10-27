/**
 * @file redisModule.ts
 * NestJS module wiring the shared Redis client and the two Redis-backed application
 * services (rate limiting, manifest cache-aside) that depend on it. Marked `@Global`
 * so every feature module can inject {@link RedisRateLimiterService} /
 * {@link ManifestCacheAsideService} without each one re-importing this module.
 */

import { Global, Module } from '@nestjs/common';
import { REDIS_CLIENT, redisClientProvider } from './redisClientProvider';
import { RedisRateLimiterService } from './redisRateLimiterService';
import { ManifestCacheAsideService } from './manifestCacheAsideService';
import { RedisClientLike } from './redisClientLike.interface';

/**
 * Global module exposing:
 * - `REDIS_CLIENT`: the shared low-level Redis client (see {@link RedisClientLike}).
 * - {@link RedisRateLimiterService}: fixed-window rate limiting.
 * - {@link ManifestCacheAsideService}: cache-aside manifest metadata caching.
 */
@Global()
@Module({
  providers: [
    redisClientProvider,
    {
      provide: RedisRateLimiterService,
      inject: [REDIS_CLIENT],
      useFactory: (redisClient: RedisClientLike) => new RedisRateLimiterService(redisClient),
    },
    {
      provide: ManifestCacheAsideService,
      inject: [REDIS_CLIENT],
      useFactory: (redisClient: RedisClientLike) => new ManifestCacheAsideService(redisClient),
    },
  ],
  exports: [REDIS_CLIENT, RedisRateLimiterService, ManifestCacheAsideService],
})
export class RedisModule {}
