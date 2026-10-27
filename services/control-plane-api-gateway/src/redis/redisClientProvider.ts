/**
 * @file redisClientProvider.ts
 * NestJS provider factory that constructs the single shared `ioredis` client instance
 * for the process, wired from {@link ApplicationConfigurationService}. Exposed under
 * the `REDIS_CLIENT` injection token (see {@link RedisClientLike}) so consumers depend
 * on the narrow interface rather than the concrete `ioredis` class.
 */

import { Provider } from '@nestjs/common';
import Redis from 'ioredis';
import { ApplicationConfigurationService } from '../config/applicationConfigurationService';
import { RedisClientLike } from './redisClientLike.interface';

/** DI token consumers should `@Inject(REDIS_CLIENT)` to obtain the shared Redis client. */
export const REDIS_CLIENT = Symbol('REDIS_CLIENT');

/**
 * Nest provider that lazily connects an `ioredis` client using the configured
 * `redisConnectionUrl`. `lazyConnect` is left `false` (default) so connection issues
 * surface at boot rather than on first use, which is the desired fail-fast behaviour
 * for a gateway service; `maxRetriesPerRequest` is bounded so a downed Redis degrades
 * requests quickly instead of hanging them.
 */
export const redisClientProvider: Provider = {
  provide: REDIS_CLIENT,
  inject: [ApplicationConfigurationService],
  useFactory: (applicationConfigurationService: ApplicationConfigurationService): RedisClientLike => {
    const { redisConnectionUrl } = applicationConfigurationService.getRuntimeConfiguration();
    const ioredisClientInstance = new Redis(redisConnectionUrl, {
      maxRetriesPerRequest: 3,
      enableOfflineQueue: true,
    });
    // Adapt ioredis's variadic SET signature to the simplified RedisClientLike#set contract.
    const adaptedClient: RedisClientLike = {
      incr: (key) => ioredisClientInstance.incr(key),
      expire: (key, ttlInSeconds) => ioredisClientInstance.expire(key, ttlInSeconds),
      ttl: (key) => ioredisClientInstance.ttl(key),
      get: (key) => ioredisClientInstance.get(key),
      set: (key, value, ttlInSeconds) =>
        ttlInSeconds !== undefined
          ? (ioredisClientInstance.set(key, value, 'EX', ttlInSeconds) as Promise<'OK'>)
          : (ioredisClientInstance.set(key, value) as Promise<'OK'>),
      del: (...keys) => ioredisClientInstance.del(...keys),
    };
    return adaptedClient;
  },
};
