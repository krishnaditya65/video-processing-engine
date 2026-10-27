/**
 * @file videoAssetEntity.ts
 * Domain entity describing a raw video asset (e.g. a ProRes/mezzanine master upload)
 * as tracked by the Control Plane & API Gateway's asset registry: its ingestion
 * lifecycle, per-scene encoding status, and the "structural map" produced by the
 * upstream ingestion service (scene boundaries, shot changes, complexity scoring
 * used later for convex-hull bitrate ladder selection).
 */

/** Lifecycle stages a raw upload progresses through before it is playable. */
export enum VideoAssetIngestionStatus {
  /** Upload registered but bytes not yet fully received/validated by the ingestion service. */
  AWAITING_UPLOAD = 'AWAITING_UPLOAD',
  /** Bytes received; ingestion service is probing/validating the mezzanine file. */
  VALIDATING = 'VALIDATING',
  /** Ingestion service is computing the structural map (scene/shot boundaries, complexity). */
  GENERATING_STRUCTURAL_MAP = 'GENERATING_STRUCTURAL_MAP',
  /** Structural map complete; asset is ready to be handed to the encoding orchestrator. */
  READY_FOR_ENCODING = 'READY_FOR_ENCODING',
  /** Ingestion failed validation (corrupt container, unsupported codec, checksum mismatch, etc.). */
  INGESTION_FAILED = 'INGESTION_FAILED',
}

/** Coarse encoding lifecycle status for the asset as a whole (individual scenes may vary; see {@link VideoAssetEntity.perSceneEncodingJobs}). */
export enum VideoAssetEncodingStatus {
  /** Encoding has not been requested/started yet. */
  NOT_STARTED = 'NOT_STARTED',
  /** distributedEncodingWorkflow is actively running (fan-out per-scene jobs in flight). */
  ENCODING_IN_PROGRESS = 'ENCODING_IN_PROGRESS',
  /** All per-scene jobs completed, packaging finished, and the asset has been published to the CDN/origin. */
  PUBLISHED = 'PUBLISHED',
  /** One or more per-scene jobs failed after exhausting retries. */
  ENCODING_FAILED = 'ENCODING_FAILED',
}

/** One detected scene/shot segment within the asset's structural map. */
export interface StructuralMapSceneSegment {
  /** Zero-based scene index within the asset, stable for the lifetime of the asset. */
  sceneIndex: number;
  /** Inclusive start timecode of the scene, in seconds from asset start. */
  startTimeInSeconds: number;
  /** Exclusive end timecode of the scene, in seconds from asset start. */
  endTimeInSeconds: number;
  /** Heuristic spatial/temporal complexity score (0-100) used to steer per-scene bitrate ladder selection. */
  perceptualComplexityScore: number;
}

/** Per-scene encoding job tracking, populated once `distributedEncodingWorkflow` fans out. */
export interface PerSceneEncodingJobStatus {
  /** Matches {@link StructuralMapSceneSegment.sceneIndex}. */
  sceneIndex: number;
  /** Whether this individual scene's encode has completed, failed, or is still running. */
  jobStatus: 'PENDING' | 'ENCODING' | 'COMPLETED' | 'FAILED';
  /** Bitrate ladder (kbps) selected for this scene via convex-hull optimisation, once computed. */
  selectedBitrateLadderKbps?: number[];
}

/**
 * The full domain entity persisted (in this reference implementation, in-memory) by
 * the asset registry for each raw video asset.
 */
export interface VideoAssetEntity {
  /** Globally unique identifier for this asset (UUID v4), assigned at registration time. */
  videoAssetIdentifier: string;
  /** Human-supplied title/description for editorial/CMS display purposes. */
  displayTitle: string;
  /** Storage location (e.g. S3 URI) of the original raw mezzanine/ProRes upload. */
  sourceMezzanineStorageUri: string;
  /** Current point in the ingestion lifecycle. */
  currentIngestionStatus: VideoAssetIngestionStatus;
  /** Current point in the encoding lifecycle. */
  currentEncodingStatus: VideoAssetEncodingStatus;
  /** Structural map (scene segmentation) produced by the ingestion service; absent until ingestion completes that stage. */
  structuralMapSceneSegments?: StructuralMapSceneSegment[];
  /** Per-scene encoding job statuses, populated once encoding has been kicked off. */
  perSceneEncodingJobs?: PerSceneEncodingJobStatus[];
  /** Temporal workflow id of the `distributedEncodingWorkflow` run associated with this asset, once started. */
  associatedTemporalWorkflowIdentifier?: string;
  /** ISO-8601 timestamp when this asset record was created. */
  registeredAtIsoTimestamp: string;
  /** ISO-8601 timestamp of the most recent status mutation, for observability/debugging. */
  lastUpdatedAtIsoTimestamp: string;
}
