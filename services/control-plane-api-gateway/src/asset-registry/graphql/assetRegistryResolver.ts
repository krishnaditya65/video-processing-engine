/**
 * @file assetRegistryResolver.ts
 * GraphQL resolver exposing the asset registry bounded context: querying video
 * assets and registering new raw uploads. Applies both the JWT auth guard and the
 * Redis-backed rate limiting guard, demonstrating the pluggable guard composition
 * described in SERVICE_README.md.
 */

import { UseGuards } from '@nestjs/common';
import { Args, ID, Mutation, Query, Resolver } from '@nestjs/graphql';
import { JsonWebTokenAuthenticationGuard } from '../../common/auth/jsonWebTokenAuthenticationGuard';
import { RedisBackedRateLimitingGuard } from '../../common/rate-limiting/redisBackedRateLimitingGuard';
import { AssetRegistryService } from '../services/assetRegistryService';
import { RegisterRawVideoAssetInputType } from '../dto/registerRawVideoAssetInputType';
import { VideoAssetGraphQlType, mapVideoAssetEntityToGraphQlType } from '../dto/videoAssetGraphQlType';

/**
 * Resolver for `VideoAsset`-related queries and mutations. Guarded by rate limiting
 * on every operation; JWT authentication is additionally required for the mutation
 * (registering assets) while lookups are left open to any authenticated-or-not caller
 * in this reference implementation — tighten per real access-control requirements.
 */
@Resolver(() => VideoAssetGraphQlType)
@UseGuards(RedisBackedRateLimitingGuard)
export class AssetRegistryResolver {
  public constructor(private readonly assetRegistryService: AssetRegistryService) {}

  /**
   * Fetches a single video asset by id.
   * @param videoAssetIdentifier - The asset's unique id.
   * @returns The matching {@link VideoAssetGraphQlType}.
   */
  @Query(() => VideoAssetGraphQlType, {
    name: 'videoAsset',
    description: 'Fetches a single registered video asset by its identifier.',
  })
  public async videoAsset(
    @Args('videoAssetIdentifier', { type: () => ID }) videoAssetIdentifier: string,
  ): Promise<VideoAssetGraphQlType> {
    const videoAssetEntity = await this.assetRegistryService.getVideoAssetByIdentifierOrThrow(videoAssetIdentifier);
    return mapVideoAssetEntityToGraphQlType(videoAssetEntity);
  }

  /**
   * Lists every registered video asset, most recently registered first.
   * @returns All registered assets as {@link VideoAssetGraphQlType}.
   */
  @Query(() => [VideoAssetGraphQlType], {
    name: 'allVideoAssets',
    description: 'Lists every video asset currently tracked by the control plane.',
  })
  public async allVideoAssets(): Promise<VideoAssetGraphQlType[]> {
    const videoAssetEntities = await this.assetRegistryService.listAllRegisteredVideoAssets();
    return videoAssetEntities.map(mapVideoAssetEntityToGraphQlType);
  }

  /**
   * Registers a newly-uploaded raw video asset (ProRes/mezzanine master).
   * @param registerRawVideoAssetInput - Title + source storage URI for the new asset.
   * @returns The newly created {@link VideoAssetGraphQlType}.
   */
  @Mutation(() => VideoAssetGraphQlType, {
    name: 'registerRawVideoAsset',
    description: 'Registers a newly-uploaded raw mezzanine/ProRes video asset for downstream ingestion + encoding.',
  })
  @UseGuards(JsonWebTokenAuthenticationGuard)
  public async registerRawVideoAsset(
    @Args('input') registerRawVideoAssetInput: RegisterRawVideoAssetInputType,
  ): Promise<VideoAssetGraphQlType> {
    const videoAssetEntity = await this.assetRegistryService.registerNewRawVideoAsset(registerRawVideoAssetInput);
    return mapVideoAssetEntityToGraphQlType(videoAssetEntity);
  }
}
