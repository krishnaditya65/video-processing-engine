/**
 * @file manifestCacheAsideService.spec.ts
 * Unit tests for {@link ManifestCacheAsideService} verifying: loader is invoked and
 * result cached on a miss, loader is skipped on a hit, corrupted cache entries are
 * treated as a miss, and explicit invalidation forces recomputation.
 */

import { ManifestCacheAsideService } from './manifestCacheAsideService';
import { InMemoryRedisClientFake } from './testFakes/inMemoryRedisClientFake';

interface FakeManifestMetadata {
  manifestUrl: string;
  bitrateLadderKbps: number[];
}

describe('ManifestCacheAsideService', () => {
  let inMemoryRedisClientFake: InMemoryRedisClientFake;
  let manifestCacheAsideService: ManifestCacheAsideService;

  beforeEach(() => {
    inMemoryRedisClientFake = new InMemoryRedisClientFake();
    manifestCacheAsideService = new ManifestCacheAsideService(inMemoryRedisClientFake);
  });

  it('invokes the loader and caches the result on a cache miss', async () => {
    const loaderFunction = jest.fn(async (): Promise<FakeManifestMetadata> => ({
      manifestUrl: 'https://cdn.example.com/asset-1/master.m3u8',
      bitrateLadderKbps: [800, 1600, 3200],
    }));

    const result = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-1', loaderFunction, 30);

    expect(loaderFunction).toHaveBeenCalledTimes(1);
    expect(result.wasServedFromCache).toBe(false);
    expect(result.resolvedValue.manifestUrl).toBe('https://cdn.example.com/asset-1/master.m3u8');
  });

  it('serves subsequent lookups from cache without invoking the loader again', async () => {
    const loaderFunction = jest.fn(async (): Promise<FakeManifestMetadata> => ({
      manifestUrl: 'https://cdn.example.com/asset-2/master.m3u8',
      bitrateLadderKbps: [800, 1600],
    }));

    await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-2', loaderFunction, 30);
    const secondResult = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-2', loaderFunction, 30);

    expect(loaderFunction).toHaveBeenCalledTimes(1);
    expect(secondResult.wasServedFromCache).toBe(true);
    expect(secondResult.resolvedValue.manifestUrl).toBe('https://cdn.example.com/asset-2/master.m3u8');
  });

  it('treats a corrupted cache entry as a miss and recomputes', async () => {
    await inMemoryRedisClientFake.set('manifest:asset-3', 'not-valid-json{{{', 30);
    const loaderFunction = jest.fn(async (): Promise<FakeManifestMetadata> => ({
      manifestUrl: 'https://cdn.example.com/asset-3/master.m3u8',
      bitrateLadderKbps: [1200],
    }));

    const result = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-3', loaderFunction, 30);

    expect(loaderFunction).toHaveBeenCalledTimes(1);
    expect(result.wasServedFromCache).toBe(false);
  });

  it('does not persist to cache when timeToLiveInSeconds is non-positive', async () => {
    const loaderFunction = jest.fn(async (): Promise<FakeManifestMetadata> => ({
      manifestUrl: 'https://cdn.example.com/asset-4/master.m3u8',
      bitrateLadderKbps: [500],
    }));

    await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-4', loaderFunction, 0);
    const secondResult = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-4', loaderFunction, 0);

    expect(loaderFunction).toHaveBeenCalledTimes(2);
    expect(secondResult.wasServedFromCache).toBe(false);
  });

  it('forces recomputation after explicit invalidation', async () => {
    const loaderFunction = jest
      .fn<Promise<FakeManifestMetadata>, []>()
      .mockResolvedValueOnce({ manifestUrl: 'v1-url', bitrateLadderKbps: [1000] })
      .mockResolvedValueOnce({ manifestUrl: 'v2-url', bitrateLadderKbps: [2000] });

    const firstResult = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-5', loaderFunction, 30);
    expect(firstResult.resolvedValue.manifestUrl).toBe('v1-url');

    await manifestCacheAsideService.invalidateCachedManifestEntry('manifest:asset-5');

    const secondResult = await manifestCacheAsideService.resolveWithCacheAsideStrategy('manifest:asset-5', loaderFunction, 30);
    expect(secondResult.resolvedValue.manifestUrl).toBe('v2-url');
    expect(secondResult.wasServedFromCache).toBe(false);
    expect(loaderFunction).toHaveBeenCalledTimes(2);
  });
});
