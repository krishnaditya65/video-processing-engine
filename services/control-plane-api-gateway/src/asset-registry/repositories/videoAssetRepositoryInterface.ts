/**
 * @file videoAssetRepositoryInterface.ts
 * Repository-pattern abstraction over video asset persistence. All application/domain
 * code (AssetRegistryService, GraphQL resolvers) depends only on this interface, not
 * on any concrete storage technology — the in-memory implementation
 * ({@link InMemoryVideoAssetRepository}) used in this reference build is designed to
 * be swapped for a real database-backed implementation (e.g. Postgres via Prisma/TypeORM,
 * or DynamoDB) without touching a single caller. This is the Repository design pattern
 * applied explicitly, as required by the service's design constraints.
 */

import { VideoAssetEntity } from '../entities/videoAssetEntity';

/** Repository contract for persisting and querying {@link VideoAssetEntity} records. */
export interface VideoAssetRepositoryInterface {
  /**
   * Persists a brand-new asset record.
   * @param videoAssetEntity - The fully-constructed entity to store.
   * @returns The stored entity (returned for symmetry/chaining; storage-generated fields, if any, would be populated here).
   */
  saveNewVideoAsset(videoAssetEntity: VideoAssetEntity): Promise<VideoAssetEntity>;

  /**
   * Looks up a single asset by its identifier.
   * @param videoAssetIdentifier - The asset's unique id.
   * @returns The matching entity, or `undefined` if no asset with that id exists.
   */
  findVideoAssetByIdentifier(videoAssetIdentifier: string): Promise<VideoAssetEntity | undefined>;

  /**
   * Lists all currently registered assets. In a real database-backed implementation
   * this would take pagination cursors; kept simple here since the in-memory store
   * is not expected to hold production-scale volumes.
   * @returns All currently stored assets, newest first.
   */
  listAllRegisteredVideoAssets(): Promise<VideoAssetEntity[]>;

  /**
   * Applies a partial update to an existing asset (e.g. status transitions,
   * structural map population) and persists it.
   * @param videoAssetIdentifier - The asset to update.
   * @param partialUpdate - Fields to merge into the existing entity.
   * @returns The updated entity, or `undefined` if no asset with that id exists.
   */
  updateExistingVideoAsset(
    videoAssetIdentifier: string,
    partialUpdate: Partial<VideoAssetEntity>,
  ): Promise<VideoAssetEntity | undefined>;
}
