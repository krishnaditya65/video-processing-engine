/**
 * @file Validation stage of the beacon ingestion Pipeline pattern.
 *
 * This is the first stage of the four-stage pipeline
 * (validate -> normalize -> enrich -> batch-insert) applied to every raw
 * beacon consumed off Kafka/Redpanda. It is a pure function: given a raw,
 * untrusted wire message, it either returns a validated
 * `PlaybackBeaconWireSchemaOutput` or a structured validation failure --
 * it performs no I/O, which is precisely what makes it unit-testable
 * without a live broker.
 */
import { ZodError } from 'zod';
import {
  playbackBeaconWireSchema,
  type PlaybackBeaconWireSchemaOutput,
} from '../../schemas/playbackBeaconSchema';
import type { RawPlaybackBeaconWireMessage } from '../../types/beaconTypes';

/** Discriminated-union result of validating a single raw beacon wire message. */
export type BeaconValidationResult =
  | { readonly isValid: true; readonly beacon: PlaybackBeaconWireSchemaOutput }
  | { readonly isValid: false; readonly rawMessage: RawPlaybackBeaconWireMessage; readonly validationErrors: string[] };

/**
 * Validates a single raw playback beacon wire message against
 * `playbackBeaconWireSchema`.
 *
 * @param rawMessage - The untrusted, parsed-JSON Kafka/Redpanda message
 * value for one beacon.
 * @returns A `BeaconValidationResult` discriminated union: `isValid: true`
 * with the parsed/coerced beacon, or `isValid: false` with a flat list of
 * human-readable validation error strings (one per failing field path),
 * suitable for dead-lettering or structured logging.
 */
export function validateRawPlaybackBeaconWireMessage(
  rawMessage: RawPlaybackBeaconWireMessage,
): BeaconValidationResult {
  const result = playbackBeaconWireSchema.safeParse(rawMessage);
  if (result.success) {
    return { isValid: true, beacon: result.data };
  }
  return {
    isValid: false,
    rawMessage,
    validationErrors: flattenZodErrorIntoReadableStrings(result.error),
  };
}

/**
 * Validates a batch of raw beacon wire messages, partitioning them into
 * valid and invalid groups. Used by the Kafka consumer to process an
 * entire fetched batch in one pass before handing valid beacons to the
 * normalization stage.
 *
 * @param rawMessages - Array of untrusted raw beacon wire messages.
 * @returns An object with `validBeacons` (successfully parsed) and
 * `invalidBeacons` (with their validation errors attached), preserving
 * relative order within each group.
 */
export function validateRawPlaybackBeaconWireMessageBatch(
  rawMessages: RawPlaybackBeaconWireMessage[],
): {
  validBeacons: PlaybackBeaconWireSchemaOutput[];
  invalidBeacons: Array<{ rawMessage: RawPlaybackBeaconWireMessage; validationErrors: string[] }>;
} {
  const validBeacons: PlaybackBeaconWireSchemaOutput[] = [];
  const invalidBeacons: Array<{ rawMessage: RawPlaybackBeaconWireMessage; validationErrors: string[] }> = [];

  for (const rawMessage of rawMessages) {
    const validationResult = validateRawPlaybackBeaconWireMessage(rawMessage);
    if (validationResult.isValid) {
      validBeacons.push(validationResult.beacon);
    } else {
      invalidBeacons.push({
        rawMessage: validationResult.rawMessage,
        validationErrors: validationResult.validationErrors,
      });
    }
  }

  return { validBeacons, invalidBeacons };
}

/**
 * Flattens a `ZodError` into an array of `"<dot.path>: <message>"` strings
 * for structured logging / dead-letter payloads, avoiding leaking the full
 * ZodError object (with its internal issue tree) into downstream systems.
 *
 * @param error - The `ZodError` thrown/returned by a failed schema parse.
 * @returns Array of readable `"path: message"` strings, one per issue.
 */
function flattenZodErrorIntoReadableStrings(error: ZodError): string[] {
  return error.issues.map((issue) => {
    const path = issue.path.length > 0 ? issue.path.join('.') : '(root)';
    return `${path}: ${issue.message}`;
  });
}
