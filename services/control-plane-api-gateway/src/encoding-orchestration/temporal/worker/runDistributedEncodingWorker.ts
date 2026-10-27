/**
 * @file runDistributedEncodingWorker.ts
 * Standalone entry point (NOT part of the NestJS dependency graph) that boots a real
 * `@temporalio/worker` `Worker`, polling the configured task queue and executing
 * `distributedEncodingWorkflow` + its activities. Temporal's architecture requires
 * workers to run as separate long-lived processes from any client that merely starts
 * workflows — this file is that separate process.
 *
 * Run with: `npm run start:temporal-worker` (see SERVICE_README.md for prerequisites
 * — a reachable Temporal server is required; this process will retry connecting and
 * log clearly if one is not available, rather than crash-looping silently).
 */

import { Worker, NativeConnection } from '@temporalio/worker';
import * as distributedEncodingActivities from '../activities/distributedEncodingActivities';

/**
 * Resolves task-queue/server configuration from environment variables directly
 * (rather than {@link ApplicationConfigurationService}) since this process is
 * intentionally outside the Nest DI container — it is a plain Node.js script per
 * Temporal's worker-process model.
 */
function resolveWorkerRuntimeConfigurationFromEnvironment(): {
  temporalServerFrontendAddress: string;
  temporalNamespace: string;
  temporalEncodingTaskQueueName: string;
} {
  return {
    temporalServerFrontendAddress: process.env.TEMPORAL_SERVER_FRONTEND_ADDRESS ?? 'localhost:7233',
    temporalNamespace: process.env.TEMPORAL_NAMESPACE ?? 'default',
    temporalEncodingTaskQueueName: process.env.TEMPORAL_ENCODING_TASK_QUEUE_NAME ?? 'distributed-encoding-task-queue',
  };
}

/**
 * Boots the Temporal Worker and begins polling for workflow/activity tasks. Resolves
 * once the worker is shut down (normally never, until the process receives a
 * termination signal).
 */
async function runDistributedEncodingWorkerProcess(): Promise<void> {
  const { temporalServerFrontendAddress, temporalNamespace, temporalEncodingTaskQueueName } =
    resolveWorkerRuntimeConfigurationFromEnvironment();

  // eslint-disable-next-line no-console
  console.log(
    `[distributed-encoding-worker] Connecting to Temporal server at ${temporalServerFrontendAddress} (namespace=${temporalNamespace}), task queue "${temporalEncodingTaskQueueName}"...`,
  );

  const nativeConnection = await NativeConnection.connect({ address: temporalServerFrontendAddress });

  const worker = await Worker.create({
    connection: nativeConnection,
    namespace: temporalNamespace,
    taskQueue: temporalEncodingTaskQueueName,
    // Workflow bundling: point Temporal at the compiled workflow module so it can be
    // loaded into an isolated V8 context per Temporal's deterministic-replay model.
    workflowsPath: require.resolve('../workflows/distributedEncodingWorkflow'),
    activities: distributedEncodingActivities,
  });

  // eslint-disable-next-line no-console
  console.log('[distributed-encoding-worker] Worker created; polling for tasks. Press Ctrl+C to exit.');
  await worker.run();
}

runDistributedEncodingWorkerProcess().catch((workerStartupError) => {
  // eslint-disable-next-line no-console
  console.error(
    '[distributed-encoding-worker] Fatal error starting the Temporal worker. Is a Temporal server reachable at ' +
      'TEMPORAL_SERVER_FRONTEND_ADDRESS? See SERVICE_README.md "Running against real Temporal".',
    workerStartupError,
  );
  process.exitCode = 1;
});
