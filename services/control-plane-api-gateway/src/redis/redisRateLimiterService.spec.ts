/**
 * @file redisRateLimiterService.spec.ts
 * Unit tests for {@link RedisRateLimiterService} against {@link InMemoryRedisClientFake},
 * covering: permitting requests under the limit, denying once the limit is exceeded,
 * and window reset behaviour after TTL expiry.
 */

import { RedisRateLimiterService } from './redisRateLimiterService';
import { InMemoryRedisClientFake } from './testFakes/inMemoryRedisClientFake';

describe('RedisRateLimiterService', () => {
  let fakeCurrentTimeInMilliseconds: number;
  let redisRateLimiterService: RedisRateLimiterService;

  beforeEach(() => {
    fakeCurrentTimeInMilliseconds = 0;
    const inMemoryRedisClientFake = new InMemoryRedisClientFake(() => fakeCurrentTimeInMilliseconds);
    redisRateLimiterService = new RedisRateLimiterService(inMemoryRedisClientFake);
  });

  it('permits requests while under the configured maximum', async () => {
    for (let requestIndex = 0; requestIndex < 5; requestIndex += 1) {
      const result = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:alpha', 5, 60);
      expect(result.isRequestPermitted).toBe(true);
      expect(result.currentRequestCountInWindow).toBe(requestIndex + 1);
    }
  });

  it('denies the request once the count exceeds the configured maximum', async () => {
    for (let requestIndex = 0; requestIndex < 5; requestIndex += 1) {
      await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:beta', 5, 60);
    }
    const sixthResult = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:beta', 5, 60);
    expect(sixthResult.isRequestPermitted).toBe(false);
    expect(sixthResult.currentRequestCountInWindow).toBe(6);
  });

  it('tracks separate identity keys independently', async () => {
    await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:gamma', 1, 60);
    const secondUserResult = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:delta', 1, 60);
    expect(secondUserResult.isRequestPermitted).toBe(true);
    expect(secondUserResult.currentRequestCountInWindow).toBe(1);
  });

  it('resets the window (and permits again) once the TTL has elapsed', async () => {
    for (let requestIndex = 0; requestIndex < 2; requestIndex += 1) {
      await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:epsilon', 2, 10);
    }
    const deniedResult = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:epsilon', 2, 10);
    expect(deniedResult.isRequestPermitted).toBe(false);

    // Advance the fake clock past the 10s window.
    fakeCurrentTimeInMilliseconds += 10_001;

    const afterResetResult = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:epsilon', 2, 10);
    expect(afterResetResult.isRequestPermitted).toBe(true);
    expect(afterResetResult.currentRequestCountInWindow).toBe(1);
  });

  it('reports the remaining seconds in the current window', async () => {
    const result = await redisRateLimiterService.recordRequestAttemptAndCheckRateLimit('user:zeta', 10, 30);
    expect(result.secondsRemainingInCurrentWindow).toBe(30);
  });
});
