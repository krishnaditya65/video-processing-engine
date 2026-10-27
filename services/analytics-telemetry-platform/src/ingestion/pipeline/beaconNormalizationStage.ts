/**
 * @file Normalization stage of the beacon ingestion Pipeline pattern.
 *
 * Second stage of validate -> normalize -> enrich -> batch-insert. Takes a
 * beacon that has already passed Zod validation
 * (`PlaybackBeaconWireSchemaOutput`) and converts it into the canonical,
 * fully-typed `NormalizedPlaybackBeacon` shape used by every downstream
 * consumer: coercing timestamps to epoch millis, default-filling optional
 * fields to `null`, and trimming/lower-casing identifiers where
 * appropriate. Pure function, no I/O -- unit-testable in isolation.
 */
import type { PlaybackBeaconWireSchemaOutput } from '../../schemas/playbackBeaconSchema';
import type { NormalizedPlaybackBeacon } from '../../types/beaconTypes';

/**
 * Normalizes a validated raw beacon into the canonical
 * `NormalizedPlaybackBeacon` shape.
 *
 * @param validatedBeacon - Output of `playbackBeaconWireSchema.safeParse`
 * for a single beacon (already schema-valid).
 * @returns A fully normalized `NormalizedPlaybackBeacon` with all optional
 * fields explicitly defaulted to `null` and the timestamp coerced to
 * epoch milliseconds.
 */
export function normalizeValidatedPlaybackBeacon(
  validatedBeacon: PlaybackBeaconWireSchemaOutput,
): NormalizedPlaybackBeacon {
  return {
    beaconId: validatedBeacon.beaconId,
    sessionId: validatedBeacon.sessionId,
    assetId: validatedBeacon.assetId,
    userId: validatedBeacon.userId ?? null,
    eventType: validatedBeacon.eventType,
    eventTimestampEpochMillis: coerceTimestampInputToEpochMilliseconds(validatedBeacon.eventTimestamp),
    timeToFirstFrameMillis: validatedBeacon.timeToFirstFrameMillis ?? null,
    rebufferDurationMillis: validatedBeacon.rebufferDurationMillis ?? null,
    bitrateKbps: validatedBeacon.bitrateKbps ?? null,
    renditionId: validatedBeacon.renditionId ?? null,
    frameDropCount: validatedBeacon.frameDropCount ?? null,
    decodeErrorCode: validatedBeacon.decodeErrorCode ?? null,
    decodeErrorMessage: validatedBeacon.decodeErrorMessage ?? null,
    hardwareDecoderModel: validatedBeacon.hardwareDecoderModel ?? null,
    deviceFirmwareVersion: validatedBeacon.deviceFirmwareVersion ?? null,
    deviceOperatingSystem: validatedBeacon.deviceOperatingSystem,
    networkAutonomousSystemNumber: validatedBeacon.networkAutonomousSystemNumber ?? null,
    internetServiceProviderName: validatedBeacon.internetServiceProviderName ?? null,
    playbackPositionSeconds: validatedBeacon.playbackPositionSeconds ?? null,
    assetDurationSeconds: validatedBeacon.assetDurationSeconds ?? null,
    traceId: validatedBeacon.traceId ?? null,
  };
}

/**
 * Normalizes a batch of validated beacons in one pass, preserving order.
 *
 * @param validatedBeacons - Array of schema-valid raw beacons.
 * @returns Array of normalized beacons, same length/order as input.
 */
export function normalizeValidatedPlaybackBeaconBatch(
  validatedBeacons: PlaybackBeaconWireSchemaOutput[],
): NormalizedPlaybackBeacon[] {
  return validatedBeacons.map(normalizeValidatedPlaybackBeacon);
}

/**
 * Coerces a beacon's `eventTimestamp` field -- which may arrive as either
 * an epoch-milliseconds number or an ISO-8601 date string, per
 * `playbackBeaconWireSchema` -- into a canonical epoch-milliseconds number.
 *
 * @param timestampInput - Either a numeric epoch-millis value or an
 * ISO-8601 timestamp string.
 * @returns Epoch-milliseconds numeric timestamp.
 * @throws {Error} If a string input cannot be parsed into a valid date.
 */
function coerceTimestampInputToEpochMilliseconds(timestampInput: number | string): number {
  if (typeof timestampInput === 'number') {
    return timestampInput;
  }
  const parsedEpochMillis = Date.parse(timestampInput);
  if (Number.isNaN(parsedEpochMillis)) {
    throw new Error(`Unable to parse eventTimestamp string as a valid date: "${timestampInput}"`);
  }
  return parsedEpochMillis;
}
