/**
 * @file Zod validation schema for raw QoE playback beacons received over
 * Kafka/Redpanda. This is the single source of truth for "what shape must a
 * beacon have to be admitted into the pipeline" -- the validation stage of
 * the ingestion pipeline (`beaconValidationStage.ts`) parses every incoming
 * wire message through `playbackBeaconWireSchema` and rejects/dead-letters
 * anything that fails.
 */
import { z } from 'zod';

/** Zod enum mirroring `PlaybackBeaconEventType`. */
export const playbackBeaconEventTypeSchema = z.enum([
  'session_start',
  'first_frame',
  'rebuffer_start',
  'rebuffer_end',
  'bitrate_switch',
  'frame_drop',
  'decode_error',
  'heartbeat',
  'session_end',
]);

/**
 * Validation + coercion schema for a single raw playback beacon wire
 * message. Fields are intentionally lenient about numeric string coercion
 * (client SDKs on some legacy set-top-box firmwares serialize numbers as
 * strings) but strict about required identifiers and enumerations.
 *
 * On successful parse, `.parse()`/`.safeParse()` yields an object whose
 * shape is close to, but not identical to, `NormalizedPlaybackBeacon` --
 * the normalization pipeline stage (`beaconNormalizationStage.ts`) performs
 * the remaining unit/shape conversion (e.g. default-filling nullable
 * fields, epoch-millis coercion).
 */
export const playbackBeaconWireSchema = z.object({
  beaconId: z.string().min(1, 'beaconId must be a non-empty string'),
  sessionId: z.string().min(1, 'sessionId must be a non-empty string'),
  assetId: z.string().min(1, 'assetId must be a non-empty string'),
  userId: z.string().min(1).nullable().optional(),
  eventType: playbackBeaconEventTypeSchema,
  // Accept either an epoch-millis number or an ISO-8601 string; coerced
  // downstream in the normalization stage.
  eventTimestamp: z.union([z.number().positive(), z.string().min(1)]),
  timeToFirstFrameMillis: z.coerce.number().nonnegative().nullable().optional(),
  rebufferDurationMillis: z.coerce.number().nonnegative().nullable().optional(),
  bitrateKbps: z.coerce.number().nonnegative().nullable().optional(),
  renditionId: z.string().min(1).nullable().optional(),
  frameDropCount: z.coerce.number().int().nonnegative().nullable().optional(),
  decodeErrorCode: z.string().min(1).nullable().optional(),
  decodeErrorMessage: z.string().nullable().optional(),
  hardwareDecoderModel: z.string().min(1).nullable().optional(),
  deviceFirmwareVersion: z.string().min(1).nullable().optional(),
  deviceOperatingSystem: z.string().min(1),
  networkAutonomousSystemNumber: z.coerce.number().int().positive().nullable().optional(),
  internetServiceProviderName: z.string().min(1).nullable().optional(),
  playbackPositionSeconds: z.coerce.number().nonnegative().nullable().optional(),
  assetDurationSeconds: z.coerce.number().positive().nullable().optional(),
  traceId: z.string().min(1).nullable().optional(),
});

/** Static TypeScript type inferred from `playbackBeaconWireSchema`. */
export type PlaybackBeaconWireSchemaOutput = z.infer<typeof playbackBeaconWireSchema>;
