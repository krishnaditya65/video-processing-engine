/**
 * @file Unit tests for the validation and normalization pipeline stages
 * (`src/ingestion/pipeline/beaconValidationStage.ts` and
 * `beaconNormalizationStage.ts`), which are pure and I/O-free and
 * therefore fully testable without a live Kafka/Redpanda broker.
 */
import {
  validateRawPlaybackBeaconWireMessage,
  validateRawPlaybackBeaconWireMessageBatch,
} from '../src/ingestion/pipeline/beaconValidationStage';
import {
  normalizeValidatedPlaybackBeacon,
} from '../src/ingestion/pipeline/beaconNormalizationStage';
import type { RawPlaybackBeaconWireMessage } from '../src/types/beaconTypes';

function validRawHeartbeatBeacon(): RawPlaybackBeaconWireMessage {
  return {
    beaconId: 'beacon-abc-123',
    sessionId: 'session-xyz-789',
    assetId: 'asset-42',
    userId: 'user-1',
    eventType: 'heartbeat',
    eventTimestamp: 1_700_000_000_000,
    bitrateKbps: 4500,
    renditionId: '1080p_4500k',
    playbackPositionSeconds: 120.5,
    assetDurationSeconds: 3600,
    deviceOperatingSystem: 'Android 14',
    networkAutonomousSystemNumber: 7018,
    internetServiceProviderName: 'AT&T',
    traceId: 'trace-001',
  };
}

describe('validateRawPlaybackBeaconWireMessage', () => {
  it('accepts a well-formed heartbeat beacon', () => {
    const result = validateRawPlaybackBeaconWireMessage(validRawHeartbeatBeacon());
    expect(result.isValid).toBe(true);
    if (result.isValid) {
      expect(result.beacon.beaconId).toBe('beacon-abc-123');
      expect(result.beacon.eventType).toBe('heartbeat');
      expect(result.beacon.bitrateKbps).toBe(4500);
    }
  });

  it('accepts an ISO-8601 string eventTimestamp', () => {
    const raw = { ...validRawHeartbeatBeacon(), eventTimestamp: '2023-11-14T22:13:20.000Z' };
    const result = validateRawPlaybackBeaconWireMessage(raw);
    expect(result.isValid).toBe(true);
  });

  it('rejects a beacon missing a required field (assetId)', () => {
    const raw = validRawHeartbeatBeacon();
    delete (raw as Record<string, unknown>)['assetId'];

    const result = validateRawPlaybackBeaconWireMessage(raw);

    expect(result.isValid).toBe(false);
    if (!result.isValid) {
      expect(result.validationErrors.some((e) => e.startsWith('assetId:'))).toBe(true);
    }
  });

  it('rejects a beacon with an invalid eventType enum value', () => {
    const raw = { ...validRawHeartbeatBeacon(), eventType: 'not_a_real_event_type' };

    const result = validateRawPlaybackBeaconWireMessage(raw);

    expect(result.isValid).toBe(false);
    if (!result.isValid) {
      expect(result.validationErrors.some((e) => e.startsWith('eventType:'))).toBe(true);
    }
  });

  it('rejects a beacon with a negative bitrateKbps', () => {
    const raw = { ...validRawHeartbeatBeacon(), bitrateKbps: -100 };

    const result = validateRawPlaybackBeaconWireMessage(raw);

    expect(result.isValid).toBe(false);
  });

  it('coerces numeric-looking string fields (legacy set-top-box firmware compatibility)', () => {
    const raw = { ...validRawHeartbeatBeacon(), bitrateKbps: '4500', frameDropCount: '3' };

    const result = validateRawPlaybackBeaconWireMessage(raw);

    expect(result.isValid).toBe(true);
    if (result.isValid) {
      expect(result.beacon.bitrateKbps).toBe(4500);
      expect(result.beacon.frameDropCount).toBe(3);
    }
  });
});

describe('validateRawPlaybackBeaconWireMessageBatch', () => {
  it('partitions a batch into valid and invalid groups, preserving order', () => {
    const valid1 = validRawHeartbeatBeacon();
    const valid2 = { ...validRawHeartbeatBeacon(), beaconId: 'beacon-2' };
    const invalid1: RawPlaybackBeaconWireMessage = { beaconId: 'bad-1' }; // missing everything else

    const { validBeacons, invalidBeacons } = validateRawPlaybackBeaconWireMessageBatch([
      valid1,
      invalid1,
      valid2,
    ]);

    expect(validBeacons).toHaveLength(2);
    expect(validBeacons[0]?.beaconId).toBe('beacon-abc-123');
    expect(validBeacons[1]?.beaconId).toBe('beacon-2');
    expect(invalidBeacons).toHaveLength(1);
    expect(invalidBeacons[0]?.rawMessage).toBe(invalid1);
    expect(invalidBeacons[0]?.validationErrors.length).toBeGreaterThan(0);
  });
});

describe('normalizeValidatedPlaybackBeacon', () => {
  it('coerces a numeric eventTimestamp through unchanged', () => {
    const validated = validateRawPlaybackBeaconWireMessage(validRawHeartbeatBeacon());
    expect(validated.isValid).toBe(true);
    if (!validated.isValid) return;

    const normalized = normalizeValidatedPlaybackBeacon(validated.beacon);

    expect(normalized.eventTimestampEpochMillis).toBe(1_700_000_000_000);
    expect(normalized.userId).toBe('user-1');
    expect(normalized.traceId).toBe('trace-001');
  });

  it('parses an ISO-8601 eventTimestamp string into the correct epoch-millis value', () => {
    const raw = { ...validRawHeartbeatBeacon(), eventTimestamp: '2023-11-14T22:13:20.000Z' };
    const validated = validateRawPlaybackBeaconWireMessage(raw);
    expect(validated.isValid).toBe(true);
    if (!validated.isValid) return;

    const normalized = normalizeValidatedPlaybackBeacon(validated.beacon);

    // 2023-11-14T22:13:20.000Z is exactly epoch millis 1700000000000
    // (this is a well-known round epoch second, verifiable via `date -d @1700000000`).
    expect(normalized.eventTimestampEpochMillis).toBe(1_700_000_000_000);
  });

  it('defaults absent optional fields to null', () => {
    const minimalRaw: RawPlaybackBeaconWireMessage = {
      beaconId: 'b1',
      sessionId: 's1',
      assetId: 'a1',
      eventType: 'session_start',
      eventTimestamp: 1_700_000_000_000,
      deviceOperatingSystem: 'iOS 17',
    };
    const validated = validateRawPlaybackBeaconWireMessage(minimalRaw);
    expect(validated.isValid).toBe(true);
    if (!validated.isValid) return;

    const normalized = normalizeValidatedPlaybackBeacon(validated.beacon);

    expect(normalized.userId).toBeNull();
    expect(normalized.bitrateKbps).toBeNull();
    expect(normalized.decodeErrorCode).toBeNull();
    expect(normalized.traceId).toBeNull();
  });
});
