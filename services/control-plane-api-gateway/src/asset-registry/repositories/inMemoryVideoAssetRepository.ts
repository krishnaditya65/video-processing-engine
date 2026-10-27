/**
 * @file inMemoryVideoAssetRepository.ts
 * In-memory implementation of {@link VideoAssetRepositoryInterface}, backed by a
 * `Map`. Suitable for local development, testing, and demonstrating the Repository
 * pattern's swappability — NOT durable and NOT safe across multiple gateway
 * instances (each process has its own independent map). See SERVICE_README.md
 * "Known limitations" for the production swap-out path (e.g. Postgres/DynamoDB).
 */

import { Injectable } from '@nestjs/common';
import { VideoAssetEntity } from '../entities/videoAssetEntity';
import { VideoAssetRepositoryInterface } from './videoAssetRepositoryInterface';

@Injectable()
export class InMemoryVideoAssetRepository implements VideoAssetRepositoryInterface {
  /** Backing store keyed by `videoAssetIdentifier`. */
  private readonly videoAssetsByIdentifier = new Map<string, VideoAssetEntity>();

  /** @inheritdoc */
  public async saveNewVideoAsset(videoAssetEntity: VideoAssetEntity): Promise<VideoAssetEntity> {
    this.videoAssetsByIdentifier.set(videoAssetEntity.videoAssetIdentifier, videoAssetEntity);
    return videoAssetEntity;
  }

  /** @inheritdoc */
  public async findVideoAssetByIdentifier(videoAssetIdentifier: string): Promise<VideoAssetEntity | undefined> {
    return this.videoAssetsByIdentifier.get(videoAssetIdentifier);
  }

  /** @inheritdoc */
  public async listAllRegisteredVideoAssets(): Promise<VideoAssetEntity[]> {
    return Array.from(this.videoAssetsByIdentifier.values()).sort((firstAsset, secondAsset) =>
      secondAsset.registeredAtIsoTimestamp.localeCompare(firstAsset.registeredAtIsoTimestamp),
    );
  }

  /** @inheritdoc */
  public async updateExistingVideoAsset(
    videoAssetIdentifier: string,
    partialUpdate: Partial<VideoAssetEntity>,
  ): Promise<VideoAssetEntity | undefined> {
    const existingEntity = this.videoAssetsByIdentifier.get(videoAssetIdentifier);
    if (!existingEntity) {
      return undefined;
    }
    const mergedEntity: VideoAssetEntity = {
      ...existingEntity,
      ...partialUpdate,
      lastUpdatedAtIsoTimestamp: new Date().toISOString(),
    };
    this.videoAssetsByIdentifier.set(videoAssetIdentifier, mergedEntity);
    return mergedEntity;
  }
}
