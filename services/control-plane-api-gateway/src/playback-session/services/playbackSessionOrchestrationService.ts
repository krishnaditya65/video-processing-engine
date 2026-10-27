/**
 * @file playbackSessionOrchestrationService.ts
 * Orchestrates playback session creation and manifest URL resolution: the heart of
 * this gateway's "session orchestration" responsibility. Combines:
 *  - {@link DrmSystemSelectionStrategyFactory} (Strategy pattern) for DRM negotiation.
 *  - {@link AssetRegistryService} to validate the target asset is playable.
 *  - Redis (`REDIS_CLIENT`) for durable-ish session state storage, keyed with a TTL so
 *    abandoned sessions self-expire without an explicit cleanup job.
 *  - {@link ManifestCacheAsideService} for cache-aside manifest metadata resolution,
 *    personalised per device/network hint.
 */

import { Inject, Injectable, NotFoundException } from '@nestjs/common';
import { randomUUID } from 'crypto';
import { REDIS_CLIENT } from '../../redis/redisClientProvider';
import { RedisClientLike } from '../../redis/redisClientLike.interface';
import { ManifestCacheAsideService } from '../../redis/manifestCacheAsideService';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { AssetRegistryService } from '../../asset-registry/services/assetRegistryService';
import { DrmSystemSelectionStrategyFactory } from '../drm/drmSystemSelectionStrategyFactory';
import { PlaybackSessionEntity, PlaybackSessionNetworkHint } from '../entities/playbackSessionEntity';

/** Command payload accepted by {@link PlaybackSessionOrchestrationService.createPlaybackSessionForAuthenticatedUser}. */
export interface CreatePlaybackSessionCommand {
  videoAssetIdentifier: string;
  requestingUserIdentifier: string;
  devicePlatformFamily: string;
  networkConditionHint: PlaybackSessionNetworkHint;
  clientSupportedDrmSystemIdentifiers: string[];
}

/** Personalised manifest resolution result returned to GraphQL callers. */
export interface PersonalizedManifestResolutionResult {
  manifestUrl: string;
  bitrateLadderKbps: number[];
  wasServedFromCache: boolean;
}

@Injectable()
export class PlaybackSessionOrchestrationService {
  public constructor(
    @Inject(REDIS_CLIENT) private readonly redisClient: RedisClientLike,
    private readonly manifestCacheAsideService: ManifestCacheAsideService,
    private readonly assetRegistryService: AssetRegistryService,
    private readonly drmSystemSelectionStrategyFactory: DrmSystemSelectionStrategyFactory,
    private readonly applicationConfigurationService: ApplicationConfigurationService,
  ) {}

  /**
   * Creates a new playback session: validates the target asset exists, negotiates a
   * DRM configuration for the requesting device, and persists session state to Redis
   * with a TTL so idle sessions expire automatically.
   *
   * Edge cases:
   * - Unknown `videoAssetIdentifier`: propagates {@link NotFoundException} from
   *   {@link AssetRegistryService}, since a session cannot meaningfully exist for a
   *   non-existent asset.
   * - Redis write failure: propagates to the caller; session creation is considered
   *   failed rather than silently degrading, since playback cannot proceed without
   *   session state being discoverable by edge/CDN-facing services.
   *
   * @param createPlaybackSessionCommand - Session creation parameters (asset, user, device, network, DRM preferences).
   * @returns The newly created {@link PlaybackSessionEntity}.
   */
  public async createPlaybackSessionForAuthenticatedUser(
    createPlaybackSessionCommand: CreatePlaybackSessionCommand,
  ): Promise<PlaybackSessionEntity> {
    // Validate the asset exists (throws NotFoundException otherwise) before doing any
    // further work, so we never create a session pointing at a nonexistent asset.
    await this.assetRegistryService.getVideoAssetByIdentifierOrThrow(createPlaybackSessionCommand.videoAssetIdentifier);

    const negotiatedDrmConfiguration = this.drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: createPlaybackSessionCommand.devicePlatformFamily,
      clientSupportedDrmSystemIdentifiers: createPlaybackSessionCommand.clientSupportedDrmSystemIdentifiers,
    });

    const sessionTimeToLiveInSeconds = 4 * 60 * 60; // 4 hours: generous ceiling for a single viewing session.
    const playbackSessionEntity: PlaybackSessionEntity = {
      playbackSessionIdentifier: randomUUID(),
      videoAssetIdentifier: createPlaybackSessionCommand.videoAssetIdentifier,
      requestingUserIdentifier: createPlaybackSessionCommand.requestingUserIdentifier,
      devicePlatformFamily: createPlaybackSessionCommand.devicePlatformFamily,
      networkConditionHint: createPlaybackSessionCommand.networkConditionHint,
      negotiatedDrmConfiguration,
      createdAtIsoTimestamp: new Date().toISOString(),
      sessionTimeToLiveInSeconds,
    };

    await this.redisClient.set(
      this.buildPlaybackSessionRedisKey(playbackSessionEntity.playbackSessionIdentifier),
      JSON.stringify(playbackSessionEntity),
      sessionTimeToLiveInSeconds,
    );

    return playbackSessionEntity;
  }

  /**
   * Fetches session state from Redis by id.
   * @param playbackSessionIdentifier - The session's unique id.
   * @returns The {@link PlaybackSessionEntity}.
   * @throws {NotFoundException} If the session does not exist or has expired.
   */
  public async getPlaybackSessionByIdentifierOrThrow(playbackSessionIdentifier: string): Promise<PlaybackSessionEntity> {
    const rawSessionValue = await this.redisClient.get(this.buildPlaybackSessionRedisKey(playbackSessionIdentifier));
    if (!rawSessionValue) {
      throw new NotFoundException(
        `No active playback session found for identifier "${playbackSessionIdentifier}" (it may have expired).`,
      );
    }
    return JSON.parse(rawSessionValue) as PlaybackSessionEntity;
  }

  /**
   * Resolves a personalised manifest URL + bitrate ladder for a playback session,
   * using the cache-aside pattern via {@link ManifestCacheAsideService}. The cache key
   * incorporates both the asset id and the device platform family, since manifest
   * personalisation (e.g. codec selection) can legitimately differ by device even for
   * the same underlying asset.
   *
   * @param playbackSessionIdentifier - The session requesting manifest resolution.
   * @returns A {@link PersonalizedManifestResolutionResult}.
   * @throws {NotFoundException} If the session or its underlying asset cannot be found.
   */
  public async resolvePersonalizedManifestUrlForPlaybackSession(
    playbackSessionIdentifier: string,
  ): Promise<PersonalizedManifestResolutionResult> {
    const playbackSessionEntity = await this.getPlaybackSessionByIdentifierOrThrow(playbackSessionIdentifier);
    const videoAssetEntity = await this.assetRegistryService.getVideoAssetByIdentifierOrThrow(
      playbackSessionEntity.videoAssetIdentifier,
    );

    const manifestCacheKey = `manifest:{${videoAssetEntity.videoAssetIdentifier}}:${playbackSessionEntity.devicePlatformFamily}`;
    const { manifestCacheEntryTimeToLiveInSeconds } = this.applicationConfigurationService.getRuntimeConfiguration();

    const cacheAsideResult = await this.manifestCacheAsideService.resolveWithCacheAsideStrategy(
      manifestCacheKey,
      async () => this.computeAuthoritativeManifestMetadata(videoAssetEntity.videoAssetIdentifier, playbackSessionEntity.devicePlatformFamily),
      manifestCacheEntryTimeToLiveInSeconds,
    );

    return { ...cacheAsideResult.resolvedValue, wasServedFromCache: cacheAsideResult.wasServedFromCache };
  }

  /**
   * Simulates the authoritative (uncached) manifest metadata computation that, in
   * production, would call the packaging/origin service to resolve the actual
   * per-device manifest URL and bitrate ladder. Deterministic per asset+platform for
   * this reference implementation so repeated calls (absent caching) are stable.
   * @param videoAssetIdentifier - The asset the manifest belongs to.
   * @param devicePlatformFamily - The requesting device's platform family, used for codec/container personalisation.
   * @returns The simulated manifest URL + bitrate ladder.
   */
  private async computeAuthoritativeManifestMetadata(
    videoAssetIdentifier: string,
    devicePlatformFamily: string,
  ): Promise<{ manifestUrl: string; bitrateLadderKbps: number[] }> {
    const manifestContainerExtension = devicePlatformFamily.toLowerCase() === 'ios' ? 'm3u8' : 'mpd';
    return {
      manifestUrl: `https://origin.example.com/manifests/${videoAssetIdentifier}/${devicePlatformFamily.toLowerCase()}/master.${manifestContainerExtension}`,
      bitrateLadderKbps: [400, 800, 1600, 3200, 6000],
    };
  }

  /**
   * Builds the namespaced Redis key used to store/retrieve a session's state.
   * @param playbackSessionIdentifier - The session's unique id.
   * @returns The fully-namespaced Redis key.
   */
  private buildPlaybackSessionRedisKey(playbackSessionIdentifier: string): string {
    return `playback-session:{${playbackSessionIdentifier}}`;
  }
}
