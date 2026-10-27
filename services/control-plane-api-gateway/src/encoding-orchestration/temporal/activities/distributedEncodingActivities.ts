/**
 * @file distributedEncodingActivities.ts
 * Temporal Activity implementations backing `distributedEncodingWorkflow`. Activities
 * are where all actual I/O / non-deterministic work happens in Temporal's programming
 * model (workflow code itself must stay deterministic); here they are SIMULATED
 * (timed `sleep` + synthetic return values) since this sandbox has no real ffmpeg
 * cluster, packaging service, or CDN to call. Every function's doc comment states
 * plainly what a production implementation would actually do.
 *
 * These are plain async functions (the `@temporalio/activity` package is used only
 * for `Context.current()` heartbeating) registered with the Worker in
 * `runDistributedEncodingWorker.ts` — Temporal does not require activities to be
 * classes or use decorators.
 */

import { Context as TemporalActivityContext } from '@temporalio/activity';

/** Result of the `ingestRawVideoAsset` activity. */
export interface IngestRawVideoAssetActivityResult {
  /** Whether the mezzanine file passed container/codec validation. */
  isSourceFileValid: boolean;
  /** Total duration of the source asset, in seconds, as probed from the container. */
  probedDurationInSeconds: number;
}

/**
 * SIMULATED: validates and probes the raw mezzanine upload. A production
 * implementation would invoke a media-info/ffprobe-style tool against the object in
 * blob storage referenced by `sourceMezzanineStorageUri` and validate container/codec
 * conformance against the platform's ingestion contract.
 * @param sourceMezzanineStorageUri - Storage URI of the raw upload.
 * @returns A simulated {@link IngestRawVideoAssetActivityResult}.
 */
export async function ingestRawVideoAsset(sourceMezzanineStorageUri: string): Promise<IngestRawVideoAssetActivityResult> {
  TemporalActivityContext.current().heartbeat({ stage: 'ingest', sourceMezzanineStorageUri });
  await simulateWorkDelay(50);
  return { isSourceFileValid: true, probedDurationInSeconds: 1800 };
}

/** A single scene segment as produced by structural map generation. */
export interface StructuralMapSceneSegmentActivityResult {
  sceneIndex: number;
  startTimeInSeconds: number;
  endTimeInSeconds: number;
  perceptualComplexityScore: number;
}

/**
 * SIMULATED: computes scene/shot boundaries and a perceptual complexity score per
 * scene. Production implementation: the upstream ingestion service's scene-detection
 * model (e.g. histogram-diff/ML shot-boundary detector) — this control plane merely
 * *consumes* that structural map, it does not compute it; this activity stands in for
 * the RPC call to that service.
 * @param probedDurationInSeconds - Total asset duration, used to synthesize a plausible number of scenes.
 * @returns A deterministic, synthetic list of {@link StructuralMapSceneSegmentActivityResult}.
 */
export async function generateStructuralMap(probedDurationInSeconds: number): Promise<StructuralMapSceneSegmentActivityResult[]> {
  await simulateWorkDelay(75);
  const sceneCount = Math.max(1, Math.round(probedDurationInSeconds / 300)); // ~one scene per 5 minutes, synthetic.
  const sceneDurationInSeconds = probedDurationInSeconds / sceneCount;
  return Array.from({ length: sceneCount }, (_unused, sceneIndex) => ({
    sceneIndex,
    startTimeInSeconds: Math.round(sceneIndex * sceneDurationInSeconds),
    endTimeInSeconds: Math.round((sceneIndex + 1) * sceneDurationInSeconds),
    // Deterministic pseudo-complexity in [20, 90] so tests/demos are stable across runs.
    perceptualComplexityScore: 20 + ((sceneIndex * 37) % 71),
  }));
}

/** Result of encoding a single scene at a single candidate rendition. */
export interface SceneEncodeCandidateRendition {
  bitrateKbps: number;
  simulatedVmafQualityScore: number;
}

/**
 * SIMULATED: encodes one scene across a fixed ladder of candidate bitrates and
 * reports a simulated perceptual quality (VMAF-style) score per rendition, which
 * `selectConvexHullBitrateLadder` then reduces to the Pareto-optimal ladder.
 * Production implementation: dispatches to the actual distributed transcode farm
 * (e.g. per-scene ffmpeg/hardware-encoder jobs) and collects real VMAF scores.
 * @param sceneIndex - Index of the scene being encoded.
 * @param perceptualComplexityScore - Complexity score from the structural map, used to bias simulated quality.
 * @returns A synthetic set of {@link SceneEncodeCandidateRendition} candidates.
 */
export async function encodeSceneAcrossCandidateRenditions(
  sceneIndex: number,
  perceptualComplexityScore: number,
): Promise<SceneEncodeCandidateRendition[]> {
  TemporalActivityContext.current().heartbeat({ stage: 'encode-scene', sceneIndex });
  await simulateWorkDelay(100);
  const candidateBitratesKbps = [400, 800, 1600, 3200, 6000];
  return candidateBitratesKbps.map((bitrateKbps) => ({
    bitrateKbps,
    // Higher complexity scenes need more bits for the same quality: quality rises
    // with bitrate but is penalised by complexity, clipped to a plausible VMAF range.
    simulatedVmafQualityScore: Math.min(99, Math.max(30, Math.log2(bitrateKbps) * 12 - perceptualComplexityScore * 0.3)),
  }));
}

/**
 * Selects the Pareto-optimal ("convex hull") subset of candidate renditions — the
 * bitrate ladder points that are never dominated by a cheaper-and-at-least-as-good
 * alternative — mirroring the real-world convex-hull bitrate ladder optimisation
 * technique (cf. Netflix's per-title/per-shot encoding approach). This runs as a
 * genuine (non-simulated) algorithm, since it's pure computation with no I/O.
 * @param candidateRenditions - All candidate (bitrate, quality) pairs for a scene.
 * @returns The subset of `candidateRenditions`, sorted ascending by bitrate, forming the upper convex hull of the quality/bitrate curve.
 */
export async function selectConvexHullBitrateLadder(
  candidateRenditions: SceneEncodeCandidateRendition[],
): Promise<SceneEncodeCandidateRendition[]> {
  const sortedByBitrateAscending = [...candidateRenditions].sort((first, second) => first.bitrateKbps - second.bitrateKbps);
  const hullStack: SceneEncodeCandidateRendition[] = [];

  for (const candidate of sortedByBitrateAscending) {
    // Pop points that would make the hull non-convex (i.e. the middle point lies
    // below or on the line formed by its neighbours in bitrate/quality space).
    while (hullStack.length >= 2 && !formsLeftTurnMaximizingQuality(hullStack[hullStack.length - 2], hullStack[hullStack.length - 1], candidate)) {
      hullStack.pop();
    }
    hullStack.push(candidate);
  }

  return hullStack;
}

/**
 * Determines whether three consecutive (bitrate, quality) points turn "upward"
 * (convex from above) — the standard cross-product test used by the monotone-chain
 * convex hull algorithm, specialised to the upper hull (maximising quality per bit).
 * @param first - Earlier point on the hull.
 * @param middle - Candidate point being tested for hull membership.
 * @param last - The new point being added.
 * @returns `true` if `middle` should remain on the hull given `last`.
 */
function formsLeftTurnMaximizingQuality(
  first: SceneEncodeCandidateRendition,
  middle: SceneEncodeCandidateRendition,
  last: SceneEncodeCandidateRendition,
): boolean {
  const crossProduct =
    (middle.bitrateKbps - first.bitrateKbps) * (last.simulatedVmafQualityScore - first.simulatedVmafQualityScore) -
    (middle.simulatedVmafQualityScore - first.simulatedVmafQualityScore) * (last.bitrateKbps - first.bitrateKbps);
  return crossProduct > 0;
}

/**
 * SIMULATED: packages all per-scene encoded renditions into a playable
 * segmented/CMAF output (HLS/DASH) and stitches scenes back into a single asset
 * timeline. Production implementation: invokes the packaging service (e.g. Shaka
 * Packager / MP4Box) against the encoded scene segments in blob storage.
 * @param videoAssetIdentifier - The asset being packaged.
 * @param sceneCount - Number of scenes stitched into the package.
 * @returns A simulated package storage URI.
 */
export async function packageEncodedAsset(videoAssetIdentifier: string, sceneCount: number): Promise<{ packagedStorageUri: string }> {
  await simulateWorkDelay(60);
  return { packagedStorageUri: `s3://packaged-assets/${videoAssetIdentifier}/package-${sceneCount}-scenes/` };
}

/**
 * SIMULATED: publishes the packaged asset to the CDN/origin so it becomes
 * resolvable via manifest URLs. Production implementation: triggers CDN cache
 * pre-warming / origin registration and flips the asset's encoding status to
 * PUBLISHED once the origin confirms availability.
 * @param packagedStorageUri - Storage location produced by `packageEncodedAsset`.
 * @returns A simulated public manifest base URL.
 */
export async function publishPackagedAssetToCdn(packagedStorageUri: string): Promise<{ publishedManifestBaseUrl: string }> {
  await simulateWorkDelay(40);
  return { publishedManifestBaseUrl: `https://cdn.example.com/${packagedStorageUri.replace('s3://packaged-assets/', '')}` };
}

/**
 * Small helper simulating variable-latency downstream work without blocking the
 * Node.js event loop unnecessarily long in tests/demos — real activities would await
 * genuine I/O instead of a timer.
 * @param maximumDelayInMilliseconds - Upper bound (inclusive) of the simulated random delay.
 */
async function simulateWorkDelay(maximumDelayInMilliseconds: number): Promise<void> {
  const delayInMilliseconds = Math.floor(Math.random() * maximumDelayInMilliseconds);
  await new Promise<void>((resolve) => setTimeout(resolve, delayInMilliseconds));
}
