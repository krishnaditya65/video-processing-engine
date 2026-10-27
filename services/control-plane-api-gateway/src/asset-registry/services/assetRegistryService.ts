/**
 * @file assetRegistryService.ts
 * Core domain service for the asset registry bounded context: registering newly
 * uploaded raw video assets, tracking their ingestion/encoding lifecycle, and
 * exposing lookups consumed by the GraphQL BFF layer. Persistence is delegated
 * entirely to a {@link VideoAssetRepositoryInterface} implementation (Repository
 * pattern), injected via the {@link VIDEO_ASSET_REPOSITORY} token.
 */

import { Inject, Injectable, NotFoundException } from '@nestjs/common';
import { randomUUID } from 'crypto';
import {
  VideoAssetEncodingStatus,
  VideoAssetEntity,
  VideoAssetIngestionStatus,
} from '../entities/videoAssetEntity';
import { VideoAssetRepositoryInterface } from '../repositories/videoAssetRepositoryInterface';
import { VIDEO_ASSET_REPOSITORY } from '../repositories/videoAssetRepository.token';

/** Input required to register a newly-uploaded raw video asset. */
export interface RegisterRawVideoAssetCommand {
  /** Human-supplied display title for the asset. */
  displayTitle: string;
  /** Storage URI (e.g. `s3://bucket/key.mov`) of the raw mezzanine upload. */
  sourceMezzanineStorageUri: string;
}

@Injectable()
export class AssetRegistryService {
  public constructor(
    @Inject(VIDEO_ASSET_REPOSITORY) private readonly videoAssetRepository: VideoAssetRepositoryInterface,
  ) {}

  /**
   * Registers a new raw video asset in the `AWAITING_UPLOAD` ingestion state. In the
   * real system this would typically be called once an upload session is initiated
   * (e.g. after a pre-signed S3 multipart upload URL has been issued), not after
   * bytes have finished transferring.
   * @param registerRawVideoAssetCommand - Title + source storage location for the new asset.
   * @returns The newly created {@link VideoAssetEntity}.
   */
  public async registerNewRawVideoAsset(
    registerRawVideoAssetCommand: RegisterRawVideoAssetCommand,
  ): Promise<VideoAssetEntity> {
    const nowIsoTimestamp = new Date().toISOString();
    const newVideoAssetEntity: VideoAssetEntity = {
      videoAssetIdentifier: randomUUID(),
      displayTitle: registerRawVideoAssetCommand.displayTitle,
      sourceMezzanineStorageUri: registerRawVideoAssetCommand.sourceMezzanineStorageUri,
      currentIngestionStatus: VideoAssetIngestionStatus.AWAITING_UPLOAD,
      currentEncodingStatus: VideoAssetEncodingStatus.NOT_STARTED,
      registeredAtIsoTimestamp: nowIsoTimestamp,
      lastUpdatedAtIsoTimestamp: nowIsoTimestamp,
    };
    return this.videoAssetRepository.saveNewVideoAsset(newVideoAssetEntity);
  }

  /**
   * Fetches a single asset by id.
   * @param videoAssetIdentifier - The asset's unique id.
   * @returns The matching {@link VideoAssetEntity}.
   * @throws {NotFoundException} If no asset with that id is registered.
   */
  public async getVideoAssetByIdentifierOrThrow(videoAssetIdentifier: string): Promise<VideoAssetEntity> {
    const videoAssetEntity = await this.videoAssetRepository.findVideoAssetByIdentifier(videoAssetIdentifier);
    if (!videoAssetEntity) {
      throw new NotFoundException(`No video asset registered with identifier "${videoAssetIdentifier}".`);
    }
    return videoAssetEntity;
  }

  /**
   * Lists every registered asset, most-recently-registered first.
   * @returns All registered {@link VideoAssetEntity} records.
   */
  public async listAllRegisteredVideoAssets(): Promise<VideoAssetEntity[]> {
    return this.videoAssetRepository.listAllRegisteredVideoAssets();
  }

  /**
   * Records that the ingestion service has produced a structural map for an asset,
   * advancing its ingestion status accordingly.
   * @param videoAssetIdentifier - The asset the structural map belongs to.
   * @param structuralMapSceneSegments - The computed scene segmentation.
   * @returns The updated {@link VideoAssetEntity}.
   * @throws {NotFoundException} If no asset with that id is registered.
   */
  public async recordStructuralMapGenerated(
    videoAssetIdentifier: string,
    structuralMapSceneSegments: VideoAssetEntity['structuralMapSceneSegments'],
  ): Promise<VideoAssetEntity> {
    const updatedEntity = await this.videoAssetRepository.updateExistingVideoAsset(videoAssetIdentifier, {
      structuralMapSceneSegments,
      currentIngestionStatus: VideoAssetIngestionStatus.READY_FOR_ENCODING,
    });
    if (!updatedEntity) {
      throw new NotFoundException(`No video asset registered with identifier "${videoAssetIdentifier}".`);
    }
    return updatedEntity;
  }

  /**
   * Associates a started `distributedEncodingWorkflow` Temporal run with an asset and
   * marks encoding as in progress.
   * @param videoAssetIdentifier - The asset being encoded.
   * @param temporalWorkflowIdentifier - The Temporal workflow id returned by the orchestration client.
   * @returns The updated {@link VideoAssetEntity}.
   * @throws {NotFoundException} If no asset with that id is registered.
   */
  public async recordEncodingWorkflowStarted(
    videoAssetIdentifier: string,
    temporalWorkflowIdentifier: string,
  ): Promise<VideoAssetEntity> {
    const updatedEntity = await this.videoAssetRepository.updateExistingVideoAsset(videoAssetIdentifier, {
      associatedTemporalWorkflowIdentifier: temporalWorkflowIdentifier,
      currentEncodingStatus: VideoAssetEncodingStatus.ENCODING_IN_PROGRESS,
    });
    if (!updatedEntity) {
      throw new NotFoundException(`No video asset registered with identifier "${videoAssetIdentifier}".`);
    }
    return updatedEntity;
  }
}
