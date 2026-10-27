/**
 * @file playbackSessionModule.ts
 * NestJS module for the playback session orchestration bounded context: wires the DRM
 * strategy factory + its concrete strategies, the orchestration service, and the
 * GraphQL resolver. Imports {@link AssetRegistryModule} to validate assets exist at
 * session-creation time.
 */

import { Module } from '@nestjs/common';
import { AssetRegistryModule } from '../asset-registry/assetRegistryModule';
import { ClearKeyDrmSystemSelectionStrategy } from './drm/clearKeyDrmSystemSelectionStrategy';
import { DrmSystemSelectionStrategyFactory } from './drm/drmSystemSelectionStrategyFactory';
import { FairPlayDrmSystemSelectionStrategy } from './drm/fairPlayDrmSystemSelectionStrategy';
import { PlayReadyDrmSystemSelectionStrategy } from './drm/playReadyDrmSystemSelectionStrategy';
import { WidevineDrmSystemSelectionStrategy } from './drm/widevineDrmSystemSelectionStrategy';
import { PlaybackSessionResolver } from './graphql/playbackSessionResolver';
import { PlaybackSessionOrchestrationService } from './services/playbackSessionOrchestrationService';

@Module({
  imports: [AssetRegistryModule],
  providers: [
    PlaybackSessionOrchestrationService,
    PlaybackSessionResolver,
    DrmSystemSelectionStrategyFactory,
    WidevineDrmSystemSelectionStrategy,
    FairPlayDrmSystemSelectionStrategy,
    PlayReadyDrmSystemSelectionStrategy,
    ClearKeyDrmSystemSelectionStrategy,
  ],
  exports: [PlaybackSessionOrchestrationService],
})
export class PlaybackSessionModule {}
