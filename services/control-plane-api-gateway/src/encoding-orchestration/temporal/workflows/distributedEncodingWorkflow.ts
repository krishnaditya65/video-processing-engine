/**
 * @file distributedEncodingWorkflow.ts
 * The `distributedEncodingWorkflow` Temporal workflow: models the DAG
 *
 *   ingest -> generate structural map -> fan out per-scene encode jobs
 *          -> convex-hull bitrate selection (per scene) -> package -> publish
 *
 * as a durable, retryable Temporal workflow. Workflow code (this file) must remain
 * deterministic per Temporal's programming model — all actual I/O and
 * non-deterministic work happens in Activities (`distributedEncodingActivities.ts`),
 * invoked here via `proxyActivities`. This file is written against real
 * `@temporalio/workflow` APIs; running it end-to-end requires a live Temporal server
 * + worker process (see SERVICE_README.md "Running against real Temporal").
 */

import { ActivityFailure, defineQuery, defineSignal, proxyActivities, setHandler } from '@temporalio/workflow';
import type {
  IngestRawVideoAssetActivityResult,
  SceneEncodeCandidateRendition,
  StructuralMapSceneSegmentActivityResult,
} from '../activities/distributedEncodingActivities';

/**
 * Strongly-typed handle to the activities this workflow calls, proxied through
 * Temporal so each call becomes a durable, independently-retried remote invocation
 * executed by a Worker polling the configured task queue.
 *
 * Retry policy rationale:
 * - `startToCloseTimeout` bounds a single activity attempt's wall-clock time.
 * - `retry.maximumAttempts` bounds total attempts so a permanently-broken downstream
 *   (e.g. a corrupt source file) does not retry forever.
 * - `retry.backoffCoefficient` applies exponential backoff between attempts to avoid
 *   hammering a struggling downstream (complementary to this gateway's own
 *   {@link CircuitBreakerUtility} on the *synchronous* call path into Temporal).
 */
const distributedEncodingActivities = proxyActivities<typeof import('../activities/distributedEncodingActivities')>({
  startToCloseTimeout: '5 minutes',
  retry: {
    initialInterval: '1 second',
    backoffCoefficient: 2,
    maximumInterval: '30 seconds',
    maximumAttempts: 5,
  },
});

/**
 * Per-scene encoding gets its own, more aggressive retry policy since scene encode
 * jobs are the highest-fan-out, most failure-prone step (transient transcode farm
 * capacity issues) and are cheap to retry individually without re-running the whole
 * workflow.
 */
const perSceneEncodingActivities = proxyActivities<typeof import('../activities/distributedEncodingActivities')>({
  startToCloseTimeout: '10 minutes',
  retry: {
    initialInterval: '2 seconds',
    backoffCoefficient: 2,
    maximumInterval: '1 minute',
    maximumAttempts: 8,
  },
});

/** Input parameters accepted by `distributedEncodingWorkflow`. */
export interface DistributedEncodingWorkflowInput {
  /** The video asset to encode; correlates back to {@link VideoAssetEntity.videoAssetIdentifier}. */
  videoAssetIdentifier: string;
  /** Storage URI of the raw mezzanine upload to ingest. */
  sourceMezzanineStorageUri: string;
}

/** Final result of a completed `distributedEncodingWorkflow` run. */
export interface DistributedEncodingWorkflowResult {
  videoAssetIdentifier: string;
  sceneCount: number;
  publishedManifestBaseUrl: string;
  perSceneSelectedBitrateLadderKbps: Record<number, number[]>;
}

/** Live progress snapshot exposed via the `distributedEncodingWorkflowProgress` query. */
export interface DistributedEncodingWorkflowProgressSnapshot {
  currentStage: 'INGESTING' | 'GENERATING_STRUCTURAL_MAP' | 'ENCODING_SCENES' | 'PACKAGING' | 'PUBLISHING' | 'COMPLETED';
  totalSceneCount: number;
  completedSceneCount: number;
}

/**
 * Query definition allowing external callers (via {@link TemporalWorkflowClientService})
 * to poll this workflow's live progress without waiting for completion — used to
 * back a "job status" GraphQL field without needing a separate side-channel status
 * store.
 */
export const distributedEncodingWorkflowProgressQuery = defineQuery<DistributedEncodingWorkflowProgressSnapshot>(
  'distributedEncodingWorkflowProgress',
);

/**
 * Signal allowing an operator/control-plane caller to request cooperative
 * cancellation of remaining, not-yet-started scene encode work (already-dispatched
 * scene activities are allowed to finish rather than being forcibly killed, to avoid
 * wasting completed transcode work).
 */
export const cancelRemainingSceneEncodingSignal = defineSignal('cancelRemainingSceneEncoding');

/**
 * `distributedEncodingWorkflow`: the top-level Temporal workflow function
 * implementing the ingest → structural-map → fan-out-encode → convex-hull-select →
 * package → publish DAG described in this service's charter.
 *
 * Determinism note: this function must not perform direct I/O, use `Math.random()`,
 * read wall-clock time directly, etc. — all of that is delegated to Activities. The
 * `Promise.all` fan-out below is deterministic because Temporal's workflow runtime
 * replays the *recorded results* of each activity call, not re-executes them.
 *
 * @param workflowInput - {@link DistributedEncodingWorkflowInput} identifying the asset + source file.
 * @returns A {@link DistributedEncodingWorkflowResult} once ingest → publish completes.
 */
export async function distributedEncodingWorkflow(
  workflowInput: DistributedEncodingWorkflowInput,
): Promise<DistributedEncodingWorkflowResult> {
  let cancellationHasBeenRequested = false;
  setHandler(cancelRemainingSceneEncodingSignal, () => {
    cancellationHasBeenRequested = true;
  });

  let progressSnapshot: DistributedEncodingWorkflowProgressSnapshot = {
    currentStage: 'INGESTING',
    totalSceneCount: 0,
    completedSceneCount: 0,
  };
  setHandler(distributedEncodingWorkflowProgressQuery, () => progressSnapshot);

  // Step 1: ingest.
  const ingestResult: IngestRawVideoAssetActivityResult = await distributedEncodingActivities.ingestRawVideoAsset(
    workflowInput.sourceMezzanineStorageUri,
  );
  if (!ingestResult.isSourceFileValid) {
    throw new Error(`Source mezzanine file for asset "${workflowInput.videoAssetIdentifier}" failed ingestion validation.`);
  }

  // Step 2: generate structural map.
  progressSnapshot = { ...progressSnapshot, currentStage: 'GENERATING_STRUCTURAL_MAP' };
  const structuralMapSceneSegments: StructuralMapSceneSegmentActivityResult[] =
    await distributedEncodingActivities.generateStructuralMap(ingestResult.probedDurationInSeconds);

  progressSnapshot = { ...progressSnapshot, currentStage: 'ENCODING_SCENES', totalSceneCount: structuralMapSceneSegments.length };

  // Step 3 + 4: fan out per-scene encode jobs, then convex-hull bitrate selection per
  // scene. Each scene is encoded and hull-selected independently and concurrently —
  // this is the DAG's fan-out point. Cooperative cancellation is checked before each
  // scene is *started* (not mid-flight) to avoid discarding partially-completed,
  // billable transcode work.
  const perSceneSelectedBitrateLadderKbps: Record<number, number[]> = {};
  let completedSceneCount = 0;

  await Promise.all(
    structuralMapSceneSegments.map(async (sceneSegment) => {
      if (cancellationHasBeenRequested) {
        return;
      }
      const candidateRenditions: SceneEncodeCandidateRendition[] =
        await perSceneEncodingActivities.encodeSceneAcrossCandidateRenditions(
          sceneSegment.sceneIndex,
          sceneSegment.perceptualComplexityScore,
        );
      const convexHullLadder = await distributedEncodingActivities.selectConvexHullBitrateLadder(candidateRenditions);
      perSceneSelectedBitrateLadderKbps[sceneSegment.sceneIndex] = convexHullLadder.map((rendition) => rendition.bitrateKbps);

      completedSceneCount += 1;
      progressSnapshot = { ...progressSnapshot, completedSceneCount };
    }),
  );

  // Step 5: package.
  progressSnapshot = { ...progressSnapshot, currentStage: 'PACKAGING' };
  const packageResult = await distributedEncodingActivities.packageEncodedAsset(
    workflowInput.videoAssetIdentifier,
    structuralMapSceneSegments.length,
  );

  // Step 6: publish.
  progressSnapshot = { ...progressSnapshot, currentStage: 'PUBLISHING' };
  const publishResult = await distributedEncodingActivities.publishPackagedAssetToCdn(packageResult.packagedStorageUri);

  progressSnapshot = { ...progressSnapshot, currentStage: 'COMPLETED' };

  return {
    videoAssetIdentifier: workflowInput.videoAssetIdentifier,
    sceneCount: structuralMapSceneSegments.length,
    publishedManifestBaseUrl: publishResult.publishedManifestBaseUrl,
    perSceneSelectedBitrateLadderKbps,
  };
}

/**
 * Type guard distinguishing a Temporal `ActivityFailure` (an activity exhausted its
 * retries or was cancelled) from other error shapes, useful for callers/tests
 * inspecting workflow failure causes without importing Temporal's full error
 * hierarchy everywhere.
 * @param error - The thrown value to inspect.
 * @returns `true` if `error` is a Temporal {@link ActivityFailure}.
 */
export function isTemporalActivityFailure(error: unknown): error is ActivityFailure {
  return error instanceof ActivityFailure;
}
