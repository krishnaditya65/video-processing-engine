/**
 * @file encodingOrchestratorClientService.ts
 * Circuit-breaker-protected façade over {@link TemporalWorkflowClientService}. All
 * outbound calls from this gateway to the "encoding orchestrator" (here: the
 * Temporal cluster running `distributedEncodingWorkflow`) are routed through this
 * service so a struggling/unreachable Temporal deployment cannot cascade into
 * exhausting this gateway's own resources — the explicit Circuit Breaker pattern
 * requirement for this service.
 */

import { Injectable, Logger } from '@nestjs/common';
import { CircuitBreakerUtility } from '../../common/circuit-breaker/circuitBreakerUtility';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { TemporalWorkflowClientService } from '../temporal/temporalWorkflowClientService';
import { DistributedEncodingWorkflowProgressSnapshot } from '../temporal/workflows/distributedEncodingWorkflow';

@Injectable()
export class EncodingOrchestratorClientService {
  private readonly logger = new Logger(EncodingOrchestratorClientService.name);
  /** Circuit breaker guarding all calls into the Temporal-backed encoding orchestrator. */
  private readonly encodingOrchestratorCircuitBreaker: CircuitBreakerUtility<unknown>;

  public constructor(
    private readonly temporalWorkflowClientService: TemporalWorkflowClientService,
    applicationConfigurationService: ApplicationConfigurationService,
  ) {
    const {
      encodingOrchestratorCircuitBreakerFailureThreshold,
      encodingOrchestratorCircuitBreakerOpenDurationInMilliseconds,
    } = applicationConfigurationService.getRuntimeConfiguration();

    this.encodingOrchestratorCircuitBreaker = new CircuitBreakerUtility({
      circuitBreakerIdentifierName: 'encoding-orchestrator-temporal-client',
      consecutiveFailureThresholdBeforeOpening: encodingOrchestratorCircuitBreakerFailureThreshold,
      openStateDurationInMilliseconds: encodingOrchestratorCircuitBreakerOpenDurationInMilliseconds,
    });
  }

  /**
   * Starts a `distributedEncodingWorkflow` run for an asset, protected by the circuit
   * breaker. When the breaker is OPEN, this throws {@link CircuitBreakerOpenError}
   * (surfaced up through the GraphQL layer as a clear "encoding orchestrator
   * temporarily unavailable" error) instead of attempting — and likely hanging on —
   * a connection to a known-unhealthy Temporal deployment.
   * @param videoAssetIdentifier - The asset to encode.
   * @param sourceMezzanineStorageUri - Storage URI of the raw upload to encode.
   * @returns The started workflow's Temporal workflow id.
   */
  public async startDistributedEncodingWorkflowForAsset(
    videoAssetIdentifier: string,
    sourceMezzanineStorageUri: string,
  ): Promise<string> {
    return this.encodingOrchestratorCircuitBreaker.executeWithCircuitBreakerProtection(async () => {
      return this.temporalWorkflowClientService.startDistributedEncodingWorkflow({
        videoAssetIdentifier,
        sourceMezzanineStorageUri,
      });
    }) as Promise<string>;
  }

  /**
   * Fetches live workflow progress, protected by the same circuit breaker. Unlike
   * {@link startDistributedEncodingWorkflowForAsset}, callers of this method may
   * reasonably want a degraded-but-successful response rather than a hard failure
   * when the breaker is open, so this method catches {@link CircuitBreakerOpenError}
   * internally and returns `undefined` (treated by resolvers as "status unknown")
   * instead of propagating the error — an explicit fallback/degradation choice.
   * @param workflowIdentifier - The Temporal workflow id to query.
   * @returns The current progress snapshot, or `undefined` if unavailable (breaker open, workflow not found, etc.).
   */
  public async queryEncodingWorkflowProgress(
    workflowIdentifier: string,
  ): Promise<DistributedEncodingWorkflowProgressSnapshot | undefined> {
    try {
      return (await this.encodingOrchestratorCircuitBreaker.executeWithCircuitBreakerProtection(async () =>
        this.temporalWorkflowClientService.queryWorkflowProgress(workflowIdentifier),
      )) as DistributedEncodingWorkflowProgressSnapshot | undefined;
    } catch (thrownError) {
      this.logger.warn(
        `Encoding orchestrator progress query for workflow "${workflowIdentifier}" failed/short-circuited: ${(thrownError as Error).message}`,
      );
      return undefined;
    }
  }
}
