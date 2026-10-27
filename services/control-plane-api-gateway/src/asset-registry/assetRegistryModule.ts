/**
 * @file assetRegistryModule.ts
 * NestJS module for the asset registry bounded context. Binds the
 * {@link VIDEO_ASSET_REPOSITORY} token to {@link InMemoryVideoAssetRepository} — the
 * single line that would change to swap in a real database-backed repository
 * implementation (Repository pattern).
 */

import { Module } from '@nestjs/common';
import { AssetRegistryService } from './services/assetRegistryService';
import { AssetRegistryResolver } from './graphql/assetRegistryResolver';
import { InMemoryVideoAssetRepository } from './repositories/inMemoryVideoAssetRepository';
import { VIDEO_ASSET_REPOSITORY } from './repositories/videoAssetRepository.token';

@Module({
  providers: [
    AssetRegistryService,
    AssetRegistryResolver,
    { provide: VIDEO_ASSET_REPOSITORY, useClass: InMemoryVideoAssetRepository },
  ],
  exports: [AssetRegistryService],
})
export class AssetRegistryModule {}
