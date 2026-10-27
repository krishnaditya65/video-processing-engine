/**
 * @file createPlaybackSessionInputType.ts
 * Code-first GraphQL input type + validation for the `createPlaybackSession`
 * mutation, capturing device type and network hints per the service's playback
 * session orchestration responsibility.
 */

import { Field, ID, InputType } from '@nestjs/graphql';
import { IsArray, IsIn, IsNotEmpty, IsString } from 'class-validator';

/** Allowed values for `networkConditionHint`, mirrored from {@link PlaybackSessionNetworkHint}. */
const SUPPORTED_NETWORK_CONDITION_HINTS = ['CELLULAR_LOW', 'CELLULAR_HIGH', 'WIFI', 'WIRED_ETHERNET', 'UNKNOWN'] as const;

/** Input payload for creating a new playback session. */
@InputType('CreatePlaybackSessionInput')
export class CreatePlaybackSessionInputType {
  @Field(() => ID, { description: 'The video asset the caller wants to begin playing.' })
  @IsString()
  @IsNotEmpty()
  videoAssetIdentifier!: string;

  @Field(() => String, { description: "Requesting device's platform family, e.g. 'ios', 'android', 'web', 'smarttv', 'xbox'." })
  @IsString()
  @IsNotEmpty()
  devicePlatformFamily!: string;

  @Field(() => String, { description: 'Coarse network condition hint supplied by the client.' })
  @IsIn(SUPPORTED_NETWORK_CONDITION_HINTS)
  networkConditionHint!: (typeof SUPPORTED_NETWORK_CONDITION_HINTS)[number];

  @Field(() => [String], { description: "DRM systems the client declares support for, in the client's own preference order." })
  @IsArray()
  clientSupportedDrmSystemIdentifiers!: string[];
}
