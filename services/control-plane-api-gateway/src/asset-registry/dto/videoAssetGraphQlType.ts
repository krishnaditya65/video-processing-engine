/**
 * @file videoAssetGraphQlType.ts
 * Code-first GraphQL object types describing a video asset and its nested structural
 * map / per-scene encoding status. These are intentionally separate from
 * {@link VideoAssetEntity} (the persistence-layer domain entity) so the GraphQL
 * schema surface can evolve independently of internal storage shape — a lightweight
 * application of the Factory pattern for DTO construction: {@link mapVideoAssetEntityToGraphQlType}
 * is the single factory function responsible for producing the GraphQL-facing shape
 * from the internal domain entity, so resolvers never hand-roll that mapping ad hoc.
 */

import { Field, Float, GraphQLISODateTime, ID, Int, ObjectType, registerEnumType } from '@nestjs/graphql';
import {
  PerSceneEncodingJobStatus,
  StructuralMapSceneSegment,
  VideoAssetEncodingStatus,
  VideoAssetEntity,
  VideoAssetIngestionStatus,
} from '../entities/videoAssetEntity';

registerEnumType(VideoAssetIngestionStatus, {
  name: 'VideoAssetIngestionStatus',
  description: 'Lifecycle stage of raw asset ingestion, from upload through structural-map generation.',
});
registerEnumType(VideoAssetEncodingStatus, {
  name: 'VideoAssetEncodingStatus',
  description: 'Lifecycle stage of the distributed encoding workflow for an asset.',
});

/** GraphQL object type for a single detected scene segment. */
@ObjectType('StructuralMapSceneSegment', { description: 'A single detected scene/shot segment within an asset\'s structural map.' })
export class StructuralMapSceneSegmentGraphQlType implements StructuralMapSceneSegment {
  @Field(() => Int, { description: 'Zero-based, stable scene index within the asset.' })
  sceneIndex!: number;

  @Field(() => Float, { description: 'Inclusive scene start time, in seconds from asset start.' })
  startTimeInSeconds!: number;

  @Field(() => Float, { description: 'Exclusive scene end time, in seconds from asset start.' })
  endTimeInSeconds!: number;

  @Field(() => Float, { description: 'Heuristic perceptual complexity score (0-100) driving bitrate ladder selection.' })
  perceptualComplexityScore!: number;
}

/** GraphQL object type for a single scene's encoding job status. */
@ObjectType('PerSceneEncodingJobStatus', { description: 'Encoding job status for a single scene within a distributed encoding workflow run.' })
export class PerSceneEncodingJobStatusGraphQlType implements PerSceneEncodingJobStatus {
  @Field(() => Int)
  sceneIndex!: number;

  @Field(() => String, { description: 'One of PENDING | ENCODING | COMPLETED | FAILED.' })
  jobStatus!: 'PENDING' | 'ENCODING' | 'COMPLETED' | 'FAILED';

  @Field(() => [Int], { nullable: true, description: 'Selected bitrate ladder (kbps) for this scene, once convex-hull optimisation has run.' })
  selectedBitrateLadderKbps?: number[];
}

/** Top-level GraphQL object type representing a registered video asset. */
@ObjectType('VideoAsset', { description: 'A raw video asset tracked by the control plane, from upload through published, playable output.' })
export class VideoAssetGraphQlType {
  @Field(() => ID)
  videoAssetIdentifier!: string;

  @Field(() => String)
  displayTitle!: string;

  @Field(() => String, { description: 'Storage URI of the original raw mezzanine/ProRes upload.' })
  sourceMezzanineStorageUri!: string;

  @Field(() => VideoAssetIngestionStatus)
  currentIngestionStatus!: VideoAssetIngestionStatus;

  @Field(() => VideoAssetEncodingStatus)
  currentEncodingStatus!: VideoAssetEncodingStatus;

  @Field(() => [StructuralMapSceneSegmentGraphQlType], { nullable: true })
  structuralMapSceneSegments?: StructuralMapSceneSegmentGraphQlType[];

  @Field(() => [PerSceneEncodingJobStatusGraphQlType], { nullable: true })
  perSceneEncodingJobs?: PerSceneEncodingJobStatusGraphQlType[];

  @Field(() => String, { nullable: true, description: 'Temporal workflow id of the associated distributedEncodingWorkflow run, if encoding has started.' })
  associatedTemporalWorkflowIdentifier?: string;

  @Field(() => GraphQLISODateTime)
  registeredAtIsoTimestamp!: string;

  @Field(() => GraphQLISODateTime)
  lastUpdatedAtIsoTimestamp!: string;
}

/**
 * Factory function mapping an internal {@link VideoAssetEntity} to its GraphQL-facing
 * {@link VideoAssetGraphQlType} representation. Centralising this mapping avoids
 * resolvers each re-implementing (and potentially diverging on) the entity-to-DTO
 * translation.
 * @param videoAssetEntity - The internal domain entity.
 * @returns The equivalent GraphQL object type instance.
 */
export function mapVideoAssetEntityToGraphQlType(videoAssetEntity: VideoAssetEntity): VideoAssetGraphQlType {
  const graphQlType = new VideoAssetGraphQlType();
  Object.assign(graphQlType, videoAssetEntity);
  return graphQlType;
}
