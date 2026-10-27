/**
 * @file encodingOrchestrationModule.ts
 * NestJS module for the encoding orchestration bounded context: Temporal client
 * wiring, the circuit-breaker-protected façade, and the GraphQL resolver. Imports
 * {@link AssetRegistryModule} to look up/update asset records.
 */

import { Module } from '@nestjs/common';
import { AssetRegistryModule } from '../asset-registry/assetRegistryModule';
import { EncodingOrchestrationResolver } from './graphql/encodingOrchestrationResolver';
import { EncodingOrchestratorClientService } from './services/encodingOrchestratorClientService';
import { TemporalWorkflowClientService } from './temporal/temporalWorkflowClientService';

@Module({
  imports: [AssetRegistryModule],
  providers: [TemporalWorkflowClientService, EncodingOrchestratorClientService, EncodingOrchestrationResolver],
  exports: [EncodingOrchestratorClientService],
})
export class EncodingOrchestrationModule {}
