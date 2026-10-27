/**
 * @file temporalWorkflowClientService.ts
 * NestJS-injectable wrapper around `@temporalio/client`, responsible for starting and
 * querying `distributedEncodingWorkflow` runs against a Temporal server. This is the
 * thin "client" half of the Temporal integration — the "worker" half (which actually
 * executes workflow/activity code) lives in `worker/runDistributedEncodingWorker.ts`
 * and must run as a separate process (this is how Temporal is designed to be used:
 * clients start/signal/query workflows, workers execute them).
 *
 * Connecting to a real Temporal server requires `TEMPORAL_SERVER_FRONTEND_ADDRESS`
 * to point at a reachable Temporal frontend (see SERVICE_README.md). In this sandbox
 * there is no live Temporal server, so the connection is established lazily on first
 * use and any failure to connect/start is surfaced as a normal rejected promise —
 * which is exactly what {@link EncodingOrchestratorClientService}'s circuit breaker
 * wraps and protects the rest of the gateway from.
 */

import { Injectable, Logger, OnModuleDestroy } from '@nestjs/common';
import { Client, Connection, WorkflowNotFoundError } from '@temporalio/client';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import {
  DistributedEncodingWorkflowInput,
  DistributedEncodingWorkflowProgressSnapshot,
  DistributedEncodingWorkflowResult,
  distributedEncodingWorkflow,
  distributedEncodingWorkflowProgressQuery,
} from './workflows/distributedEncodingWorkflow';

@Injectable()
export class TemporalWorkflowClientService implements OnModuleDestroy {
  private readonly logger = new Logger(TemporalWorkflowClientService.name);
  /** Lazily-established Temporal client + connection, cached for the process lifetime. */
  private cachedTemporalClient: Client | undefined;
  private cachedTemporalConnection: Connection | undefined;

  public constructor(private readonly applicationConfigurationService: ApplicationConfigurationService) {}

  /**
   * Starts a new `distributedEncodingWorkflow` execution for the given asset.
   * Workflow id is deterministically derived from the asset id so re-invoking this
   * for the same asset while a run is already in flight surfaces Temporal's
   * "workflow already started" semantics rather than silently double-encoding.
   * @param distributedEncodingWorkflowInput - Asset id + source storage URI to encode.
   * @returns The started workflow's Temporal workflow id, for correlation/status polling.
   */
  public async startDistributedEncodingWorkflow(
    distributedEncodingWorkflowInput: DistributedEncodingWorkflowInput,
  ): Promise<string> {
    const temporalClient = await this.getOrCreateTemporalClient();
    const { temporalEncodingTaskQueueName } = this.applicationConfigurationService.getRuntimeConfiguration();
    const workflowIdentifier = this.buildDeterministicWorkflowIdentifier(distributedEncodingWorkflowInput.videoAssetIdentifier);

    const workflowHandle = await temporalClient.workflow.start(distributedEncodingWorkflow, {
      workflowId: workflowIdentifier,
      taskQueue: temporalEncodingTaskQueueName,
      args: [distributedEncodingWorkflowInput],
    });

    this.logger.log(`Started distributedEncodingWorkflow "${workflowHandle.workflowId}" (runId=${workflowHandle.firstExecutionRunId}).`);
    return workflowHandle.workflowId;
  }

  /**
   * Queries the live progress snapshot of an in-flight (or recently completed)
   * workflow run without blocking on its completion.
   * @param workflowIdentifier - The Temporal workflow id (as returned by {@link startDistributedEncodingWorkflow}).
   * @returns The current {@link DistributedEncodingWorkflowProgressSnapshot}, or `undefined` if the workflow cannot be found.
   */
  public async queryWorkflowProgress(workflowIdentifier: string): Promise<DistributedEncodingWorkflowProgressSnapshot | undefined> {
    const temporalClient = await this.getOrCreateTemporalClient();
    try {
      const workflowHandle = temporalClient.workflow.getHandle(workflowIdentifier);
      return await workflowHandle.query(distributedEncodingWorkflowProgressQuery);
    } catch (thrownError) {
      if (thrownError instanceof WorkflowNotFoundError) {
        return undefined;
      }
      throw thrownError;
    }
  }

  /**
   * Awaits the final result of a `distributedEncodingWorkflow` run. Intended for
   * batch/administrative callers willing to block; the primary GraphQL surface
   * should prefer {@link queryWorkflowProgress} for responsiveness.
   * @param workflowIdentifier - The Temporal workflow id to await.
   * @returns The workflow's {@link DistributedEncodingWorkflowResult} once it completes.
   */
  public async awaitWorkflowResult(workflowIdentifier: string): Promise<DistributedEncodingWorkflowResult> {
    const temporalClient = await this.getOrCreateTemporalClient();
    const workflowHandle = temporalClient.workflow.getHandle<typeof distributedEncodingWorkflow>(workflowIdentifier);
    return workflowHandle.result();
  }

  /**
   * Lazily establishes (and memoises) the Temporal `Connection` + `Client`. Kept
   * lazy rather than connecting eagerly in the constructor so this NestJS service
   * (and therefore the whole gateway) can boot successfully even when no Temporal
   * server is reachable in the current environment — connection is only attempted
   * once encoding orchestration is actually invoked.
   * @returns A connected Temporal {@link Client}.
   */
  private async getOrCreateTemporalClient(): Promise<Client> {
    if (this.cachedTemporalClient) {
      return this.cachedTemporalClient;
    }
    const { temporalServerFrontendAddress, temporalNamespace } = this.applicationConfigurationService.getRuntimeConfiguration();
    this.cachedTemporalConnection = await Connection.connect({ address: temporalServerFrontendAddress });
    this.cachedTemporalClient = new Client({ connection: this.cachedTemporalConnection, namespace: temporalNamespace });
    return this.cachedTemporalClient;
  }

  /**
   * Derives a stable Temporal workflow id for an asset, so at most one
   * `distributedEncodingWorkflow` run is ever "the current run" for a given asset at
   * a time (Temporal enforces workflow id uniqueness among running workflows).
   * @param videoAssetIdentifier - The asset being encoded.
   * @returns A deterministic workflow id string.
   */
  private buildDeterministicWorkflowIdentifier(videoAssetIdentifier: string): string {
    return `distributed-encoding-workflow-${videoAssetIdentifier}`;
  }

  /**
   * Closes the underlying Temporal connection on module teardown so the process can
   * exit cleanly (important for test runs / short-lived CLI invocations).
   */
  public async onModuleDestroy(): Promise<void> {
    await this.cachedTemporalConnection?.close();
  }
}
