/**
 * @file encodingOrchestrationResolver.ts
 * GraphQL resolver exposing encoding orchestration: kicking off the
 * `distributedEncodingWorkflow` Temporal workflow for a registered asset, and polling
 * its live progress. All calls to the encoding orchestrator go through the
 * circuit-breaker-protected {@link EncodingOrchestratorClientService}.
 */

import { UseGuards } from '@nestjs/common';
import { Args, ID, Mutation, Query, Resolver } from '@nestjs/graphql';
import { JsonWebTokenAuthenticationGuard } from '../../common/auth/jsonWebTokenAuthenticationGuard';
import { RedisBackedRateLimitingGuard } from '../../common/rate-limiting/redisBackedRateLimitingGuard';
import { AssetRegistryService } from '../../asset-registry/services/assetRegistryService';
import { VideoAssetGraphQlType, mapVideoAssetEntityToGraphQlType } from '../../asset-registry/dto/videoAssetGraphQlType';
import { EncodingOrchestratorClientService } from '../services/encodingOrchestratorClientService';
import { EncodingWorkflowProgressGraphQlType } from '../dto/encodingWorkflowProgressGraphQlType';

@Resolver()
@UseGuards(RedisBackedRateLimitingGuard)
export class EncodingOrchestrationResolver {
  public constructor(
    private readonly encodingOrchestratorClientService: EncodingOrchestratorClientService,
    private readonly assetRegistryService: AssetRegistryService,
  ) {}

  /**
   * Starts `distributedEncodingWorkflow` for a registered asset and records the
   * resulting Temporal workflow id against the asset entity.
   * @param videoAssetIdentifier - The asset to encode; must already be registered.
   * @returns The updated {@link VideoAssetGraphQlType}, now referencing the started workflow.
   */
  @Mutation(() => VideoAssetGraphQlType, {
    name: 'startEncodingForVideoAsset',
    description: 'Starts the distributedEncodingWorkflow Temporal workflow for a registered video asset (circuit-breaker protected).',
  })
  @UseGuards(JsonWebTokenAuthenticationGuard)
  public async startEncodingForVideoAsset(
    @Args('videoAssetIdentifier', { type: () => ID }) videoAssetIdentifier: string,
  ): Promise<VideoAssetGraphQlType> {
    const videoAssetEntity = await this.assetRegistryService.getVideoAssetByIdentifierOrThrow(videoAssetIdentifier);
    const startedWorkflowIdentifier = await this.encodingOrchestratorClientService.startDistributedEncodingWorkflowForAsset(
      videoAssetEntity.videoAssetIdentifier,
      videoAssetEntity.sourceMezzanineStorageUri,
    );
    const updatedVideoAssetEntity = await this.assetRegistryService.recordEncodingWorkflowStarted(
      videoAssetIdentifier,
      startedWorkflowIdentifier,
    );
    return mapVideoAssetEntityToGraphQlType(updatedVideoAssetEntity);
  }

  /**
   * Polls live progress for an asset's associated encoding workflow.
   * @param videoAssetIdentifier - The asset whose encoding progress should be queried.
   * @returns An {@link EncodingWorkflowProgressGraphQlType}, or `null` if no workflow is associated or the orchestrator is unavailable.
   */
  @Query(() => EncodingWorkflowProgressGraphQlType, {
    name: 'encodingWorkflowProgressForVideoAsset',
    nullable: true,
    description: 'Fetches live distributedEncodingWorkflow progress for an asset; null if unavailable (no workflow yet, or orchestrator circuit breaker is open).',
  })
  public async encodingWorkflowProgressForVideoAsset(
    @Args('videoAssetIdentifier', { type: () => ID }) videoAssetIdentifier: string,
  ): Promise<EncodingWorkflowProgressGraphQlType | null> {
    const videoAssetEntity = await this.assetRegistryService.getVideoAssetByIdentifierOrThrow(videoAssetIdentifier);
    if (!videoAssetEntity.associatedTemporalWorkflowIdentifier) {
      return null;
    }
    const progressSnapshot = await this.encodingOrchestratorClientService.queryEncodingWorkflowProgress(
      videoAssetEntity.associatedTemporalWorkflowIdentifier,
    );
    return progressSnapshot ? { ...progressSnapshot } : null;
  }
}
