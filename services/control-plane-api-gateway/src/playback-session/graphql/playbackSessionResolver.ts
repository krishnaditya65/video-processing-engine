/**
 * @file playbackSessionResolver.ts
 * GraphQL resolver for playback session orchestration: session creation, lookup, and
 * personalised manifest URL resolution — the three GraphQL BFF responsibilities
 * called out in the service's charter.
 */

import { UseGuards } from '@nestjs/common';
import { Args, ID, Mutation, Query, Resolver } from '@nestjs/graphql';
import { CurrentAuthenticatedUser } from '../../common/auth/currentAuthenticatedUserParamDecorator';
import { AuthenticatedUserPayload } from '../../common/auth/authenticatedUserPayload.interface';
import { JsonWebTokenAuthenticationGuard } from '../../common/auth/jsonWebTokenAuthenticationGuard';
import { RedisBackedRateLimitingGuard } from '../../common/rate-limiting/redisBackedRateLimitingGuard';
import { PlaybackSessionOrchestrationService } from '../services/playbackSessionOrchestrationService';
import { CreatePlaybackSessionInputType } from '../dto/createPlaybackSessionInputType';
import {
  PersonalizedManifestResolutionGraphQlType,
  PlaybackSessionGraphQlType,
  mapPlaybackSessionEntityToGraphQlType,
} from '../dto/playbackSessionGraphQlType';

@Resolver(() => PlaybackSessionGraphQlType)
@UseGuards(RedisBackedRateLimitingGuard)
export class PlaybackSessionResolver {
  public constructor(private readonly playbackSessionOrchestrationService: PlaybackSessionOrchestrationService) {}

  /**
   * Creates a new playback session for the authenticated caller.
   * @param createPlaybackSessionInput - Asset id, device platform, network hint, DRM preferences.
   * @param currentAuthenticatedUser - The verified caller, attached to the session as its owner.
   * @returns The newly created {@link PlaybackSessionGraphQlType}.
   */
  @Mutation(() => PlaybackSessionGraphQlType, {
    name: 'createPlaybackSession',
    description: 'Creates a playback session, negotiating DRM configuration for the requesting device and persisting session state to Redis.',
  })
  @UseGuards(JsonWebTokenAuthenticationGuard)
  public async createPlaybackSession(
    @Args('input') createPlaybackSessionInput: CreatePlaybackSessionInputType,
    @CurrentAuthenticatedUser() currentAuthenticatedUser: AuthenticatedUserPayload,
  ): Promise<PlaybackSessionGraphQlType> {
    const playbackSessionEntity = await this.playbackSessionOrchestrationService.createPlaybackSessionForAuthenticatedUser({
      ...createPlaybackSessionInput,
      requestingUserIdentifier: currentAuthenticatedUser.subjectUserIdentifier,
    });
    return mapPlaybackSessionEntityToGraphQlType(playbackSessionEntity);
  }

  /**
   * Fetches an existing playback session by id.
   * @param playbackSessionIdentifier - The session's unique id.
   * @returns The matching {@link PlaybackSessionGraphQlType}.
   */
  @Query(() => PlaybackSessionGraphQlType, {
    name: 'playbackSession',
    description: 'Fetches an active playback session by its identifier.',
  })
  public async playbackSession(
    @Args('playbackSessionIdentifier', { type: () => ID }) playbackSessionIdentifier: string,
  ): Promise<PlaybackSessionGraphQlType> {
    const playbackSessionEntity =
      await this.playbackSessionOrchestrationService.getPlaybackSessionByIdentifierOrThrow(playbackSessionIdentifier);
    return mapPlaybackSessionEntityToGraphQlType(playbackSessionEntity);
  }

  /**
   * Resolves the personalised manifest URL + bitrate ladder for an active playback
   * session, via the Redis cache-aside pattern.
   * @param playbackSessionIdentifier - The session requesting manifest resolution.
   * @returns A {@link PersonalizedManifestResolutionGraphQlType}.
   */
  @Query(() => PersonalizedManifestResolutionGraphQlType, {
    name: 'resolvePersonalizedManifestUrlForPlaybackSession',
    description: 'Resolves (cache-aside, via Redis) the personalised manifest URL and bitrate ladder for a playback session.',
  })
  public async resolvePersonalizedManifestUrlForPlaybackSession(
    @Args('playbackSessionIdentifier', { type: () => ID }) playbackSessionIdentifier: string,
  ): Promise<PersonalizedManifestResolutionGraphQlType> {
    return this.playbackSessionOrchestrationService.resolvePersonalizedManifestUrlForPlaybackSession(playbackSessionIdentifier);
  }
}
