/**
 * @file Unit tests for the pure QoE metric computation functions in
 * `src/metrics/qoeMetrics.ts`. Every expected value is hand-computed in
 * the comments alongside the fixture that produces it, so correctness can
 * be verified by inspection without re-running the code.
 */
import {
  computeAverageBitratePlayedFromPlaybackBeaconBatch,
  computeCompletionPercentageFromPlaybackSessionBeaconBatch,
  computeRebufferingRatioFromPlaybackBeaconBatch,
  computeTimeToFirstFramePercentilesFromPlaybackBeaconBatch,
} from '../src/metrics/qoeMetrics';
import type { NormalizedPlaybackBeacon } from '../src/types/beaconTypes';

/** Builds a minimally-valid `NormalizedPlaybackBeacon`, overridden per test. */
function makeBeacon(overrides: Partial<NormalizedPlaybackBeacon>): NormalizedPlaybackBeacon {
  return {
    beaconId: 'beacon-1',
    sessionId: 'session-1',
    assetId: 'asset-1',
    userId: null,
    eventType: 'heartbeat',
    eventTimestampEpochMillis: 0,
    timeToFirstFrameMillis: null,
    rebufferDurationMillis: null,
    bitrateKbps: null,
    renditionId: null,
    frameDropCount: null,
    decodeErrorCode: null,
    decodeErrorMessage: null,
    hardwareDecoderModel: null,
    deviceFirmwareVersion: null,
    deviceOperatingSystem: 'tvOS 17.5',
    networkAutonomousSystemNumber: null,
    internetServiceProviderName: null,
    playbackPositionSeconds: null,
    assetDurationSeconds: null,
    traceId: null,
    ...overrides,
  };
}

describe('computeRebufferingRatioFromPlaybackBeaconBatch', () => {
  it('computes the ratio of rebuffer time to (rebuffer + watch) time', () => {
    // Two rebuffer_end beacons: 1000ms + 2000ms = 3000ms total rebuffer.
    const rebufferBeacons = [
      makeBeacon({ eventType: 'rebuffer_end', rebufferDurationMillis: 1000, beaconId: 'r1' }),
      makeBeacon({ eventType: 'rebuffer_end', rebufferDurationMillis: 2000, beaconId: 'r2' }),
    ];
    // Four heartbeats in the same session at positions 0s, 10s, 20s, 30s
    // (timestamps increasing so they sort in this order): deltas are
    // 10s, 10s, 10s => total watch time = 30s = 30000ms.
    const heartbeats = [
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 0, eventTimestampEpochMillis: 0, beaconId: 'h1' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 10, eventTimestampEpochMillis: 1000, beaconId: 'h2' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 20, eventTimestampEpochMillis: 2000, beaconId: 'h3' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 30, eventTimestampEpochMillis: 3000, beaconId: 'h4' }),
    ];

    // ratio = 3000 / (3000 + 30000) = 3000 / 33000 = 0.0909090909...
    const expectedRatio = 3000 / 33000;
    const actualRatio = computeRebufferingRatioFromPlaybackBeaconBatch([...rebufferBeacons, ...heartbeats]);

    expect(actualRatio).toBeCloseTo(expectedRatio, 10);
    expect(actualRatio).toBeCloseTo(0.09090909, 6);
  });

  it('returns 0 when there is no rebuffering and no watch time data', () => {
    expect(computeRebufferingRatioFromPlaybackBeaconBatch([])).toBe(0);
  });

  it('ignores out-of-order heartbeat delivery by sorting on timestamp before differencing', () => {
    // Same as above but delivered out of order in the array; result must be identical.
    const heartbeats = [
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 30, eventTimestampEpochMillis: 3000, beaconId: 'h4' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 0, eventTimestampEpochMillis: 0, beaconId: 'h1' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 20, eventTimestampEpochMillis: 2000, beaconId: 'h3' }),
      makeBeacon({ eventType: 'heartbeat', playbackPositionSeconds: 10, eventTimestampEpochMillis: 1000, beaconId: 'h2' }),
    ];
    const rebufferBeacons = [
      makeBeacon({ eventType: 'rebuffer_end', rebufferDurationMillis: 1000, beaconId: 'r1' }),
      makeBeacon({ eventType: 'rebuffer_end', rebufferDurationMillis: 2000, beaconId: 'r2' }),
    ];
    const actualRatio = computeRebufferingRatioFromPlaybackBeaconBatch([...rebufferBeacons, ...heartbeats]);
    expect(actualRatio).toBeCloseTo(3000 / 33000, 10);
  });
});

describe('computeTimeToFirstFramePercentilesFromPlaybackBeaconBatch', () => {
  it('computes p50/p95/p99 via nearest-rank method over 10 sorted samples', () => {
    // Samples (already sorted ascending): 100,200,300,400,500,600,700,800,900,1000 (N=10)
    // p50: rank = ceil(0.50 * 10) = ceil(5)   = 5  -> 1-indexed 5th value = 500
    // p95: rank = ceil(0.95 * 10) = ceil(9.5) = 10 -> 1-indexed 10th value = 1000
    // p99: rank = ceil(0.99 * 10) = ceil(9.9) = 10 -> 1-indexed 10th value = 1000
    const values = [100, 200, 300, 400, 500, 600, 700, 800, 900, 1000];
    const beacons = values.map((v, i) =>
      makeBeacon({ eventType: 'first_frame', timeToFirstFrameMillis: v, beaconId: `f${i}` }),
    );

    const result = computeTimeToFirstFramePercentilesFromPlaybackBeaconBatch(beacons);

    expect(result).toEqual({ p50: 500, p95: 1000, p99: 1000 });
  });

  it('returns all zeros when there are no first_frame beacons', () => {
    expect(computeTimeToFirstFramePercentilesFromPlaybackBeaconBatch([])).toEqual({ p50: 0, p95: 0, p99: 0 });
  });
});

describe('computeAverageBitratePlayedFromPlaybackBeaconBatch', () => {
  it('computes the arithmetic mean of all reported bitrate samples', () => {
    // (1000 + 2000 + 3000) / 3 = 2000
    const beacons = [
      makeBeacon({ bitrateKbps: 1000, beaconId: 'b1' }),
      makeBeacon({ bitrateKbps: 2000, beaconId: 'b2' }),
      makeBeacon({ bitrateKbps: 3000, beaconId: 'b3' }),
    ];
    expect(computeAverageBitratePlayedFromPlaybackBeaconBatch(beacons)).toBe(2000);
  });

  it('returns 0 when no beacons report a bitrate', () => {
    expect(computeAverageBitratePlayedFromPlaybackBeaconBatch([makeBeacon({})])).toBe(0);
  });
});

describe('computeCompletionPercentageFromPlaybackSessionBeaconBatch', () => {
  it('computes furthest position over duration as a percentage', () => {
    // max(10, 50, 90) / 100 * 100 = 90%
    const beacons = [
      makeBeacon({ playbackPositionSeconds: 10, assetDurationSeconds: 100, beaconId: 'p1' }),
      makeBeacon({ playbackPositionSeconds: 50, assetDurationSeconds: 100, beaconId: 'p2' }),
      makeBeacon({ playbackPositionSeconds: 90, assetDurationSeconds: 100, beaconId: 'p3' }),
    ];
    expect(computeCompletionPercentageFromPlaybackSessionBeaconBatch(beacons)).toBe(90);
  });

  it('clamps at 100 when a reported position slightly exceeds duration', () => {
    // 105 / 100 * 100 = 105 -> clamped to 100
    const beacons = [makeBeacon({ playbackPositionSeconds: 105, assetDurationSeconds: 100 })];
    expect(computeCompletionPercentageFromPlaybackSessionBeaconBatch(beacons)).toBe(100);
  });

  it('returns 0 when no position/duration data is present', () => {
    expect(computeCompletionPercentageFromPlaybackSessionBeaconBatch([makeBeacon({})])).toBe(0);
  });
});
