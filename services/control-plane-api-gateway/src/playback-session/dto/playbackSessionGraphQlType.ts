/**
 * @file playbackSessionGraphQlType.ts
 * Code-first GraphQL object types for playback sessions and DRM negotiation results,
 * plus the factory function mapping internal entities to these GraphQL-facing types.
 */

import { Field, GraphQLISODateTime, ID, Int, ObjectType } from '@nestjs/graphql';
import { PlaybackSessionEntity } from '../entities/playbackSessionEntity';

/** GraphQL representation of a negotiated DRM configuration. */
@ObjectType('DrmConfiguration', { description: 'DRM system negotiated for a playback session via the DRM selection strategy factory.' })
export class DrmConfigurationGraphQlType {
  @Field(() => String)
  selectedDrmSystemIdentifier!: string;

  @Field(() => String)
  licenseAcquisitionEndpointUrl!: string;

  @Field(() => String)
  negotiatedSecurityLevel!: string;
}

/** GraphQL representation of a playback session. */
@ObjectType('PlaybackSession', { description: 'A playback session created for a device/user pair against a specific video asset.' })
export class PlaybackSessionGraphQlType {
  @Field(() => ID)
  playbackSessionIdentifier!: string;

  @Field(() => ID)
  videoAssetIdentifier!: string;

  @Field(() => String)
  devicePlatformFamily!: string;

  @Field(() => String)
  networkConditionHint!: string;

  @Field(() => DrmConfigurationGraphQlType)
  negotiatedDrmConfiguration!: DrmConfigurationGraphQlType;

  @Field(() => GraphQLISODateTime)
  createdAtIsoTimestamp!: string;

  @Field(() => Int)
  sessionTimeToLiveInSeconds!: number;
}

/** GraphQL representation of a resolved, personalised manifest. */
@ObjectType('PersonalizedManifestResolution', { description: 'Resolved manifest URL and bitrate ladder for a playback session, cache-aside resolved.' })
export class PersonalizedManifestResolutionGraphQlType {
  @Field(() => String)
  manifestUrl!: string;

  @Field(() => [Int])
  bitrateLadderKbps!: number[];

  @Field(() => Boolean, { description: 'Whether this result was served from the Redis manifest cache (cache-aside hit) or freshly computed.' })
  wasServedFromCache!: boolean;
}

/**
 * Factory function mapping an internal {@link PlaybackSessionEntity} to its
 * GraphQL-facing {@link PlaybackSessionGraphQlType} representation.
 * @param playbackSessionEntity - The internal domain entity.
 * @returns The equivalent GraphQL object type instance.
 */
export function mapPlaybackSessionEntityToGraphQlType(playbackSessionEntity: PlaybackSessionEntity): PlaybackSessionGraphQlType {
  const graphQlType = new PlaybackSessionGraphQlType();
  Object.assign(graphQlType, playbackSessionEntity);
  return graphQlType;
}
