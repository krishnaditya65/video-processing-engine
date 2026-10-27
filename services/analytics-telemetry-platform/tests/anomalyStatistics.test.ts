/**
 * @file Unit tests for `src/anomaly/anomalyStatistics.ts` and the
 * firmware-cohort anomaly detector built on top of it. Expected values are
 * hand-computed in comments alongside each fixture.
 */
import {
  computeArithmeticMean,
  computeSampleStandardDeviation,
  computeZScore,
} from '../src/anomaly/anomalyStatistics';
import {
  ZScoreAnomalyDetectionStrategy,
  detectFirmwareCohortDecodeErrorAnomaly,
  computeFirmwareCohortDecodeErrorRatePerSession,
  DEFAULT_FIRMWARE_ANOMALY_Z_SCORE_THRESHOLD,
} from '../src/anomaly/firmwareCohortAnomalyDetector';
import { AnomalyEventEmitter, type AnomalyDetectedEvent } from '../src/anomaly/anomalyEventEmitter';
import type { NormalizedPlaybackBeacon } from '../src/types/beaconTypes';

describe('computeArithmeticMean', () => {
  it('computes the mean of [1,2,3,4,5] as 3', () => {
    // (1+2+3+4+5)/5 = 15/5 = 3
    expect(computeArithmeticMean([1, 2, 3, 4, 5])).toBe(3);
  });

  it('throws on empty input', () => {
    expect(() => computeArithmeticMean([])).toThrow();
  });
});

describe('computeSampleStandardDeviation', () => {
  it('computes the sample stddev of [10,12,14] as 2', () => {
    // mean = (10+12+14)/3 = 36/3 = 12
    // deviations: -2, 0, 2 -> squares: 4, 0, 4 -> sum = 8
    // variance = 8 / (3-1) = 4
    // stddev = sqrt(4) = 2
    expect(computeSampleStandardDeviation([10, 12, 14])).toBeCloseTo(2, 10);
  });

  it('throws when fewer than 2 values are given', () => {
    expect(() => computeSampleStandardDeviation([5])).toThrow();
  });
});

describe('computeZScore', () => {
  it('computes z = (18 - 12) / 2 = 3', () => {
    expect(computeZScore(18, 12, 2)).toBe(3);
  });

  it('computes a negative z-score when the observation is below the mean', () => {
    // (6 - 12) / 2 = -3
    expect(computeZScore(6, 12, 2)).toBe(-3);
  });

  it('returns 0 when stddev is 0 and observation equals the mean', () => {
    expect(computeZScore(5, 5, 0)).toBe(0);
  });

  it('returns +/-Infinity when stddev is 0 and observation differs from the mean', () => {
    expect(computeZScore(6, 5, 0)).toBe(Infinity);
    expect(computeZScore(4, 5, 0)).toBe(-Infinity);
  });
});

describe('ZScoreAnomalyDetectionStrategy + detectFirmwareCohortDecodeErrorAnomaly', () => {
  it('flags an anomaly when z-score >= default threshold (3.0)', () => {
    // baseline = [10, 12, 14] -> mean=12, stddev=2 (as verified above)
    // current = 18 -> z = (18-12)/2 = 3.0, which is >= 3.0 threshold -> anomalous
    const strategy = new ZScoreAnomalyDetectionStrategy();
    const evaluation = strategy.evaluateAnomaly(18, [10, 12, 14]);

    expect(evaluation.score).toBeCloseTo(3, 10);
    expect(evaluation.isAnomalous).toBe(true);
    expect(evaluation.threshold).toBe(DEFAULT_FIRMWARE_ANOMALY_Z_SCORE_THRESHOLD);
  });

  it('does not flag an anomaly for a mild deviation (z=0.5)', () => {
    // current = 13 -> z = (13-12)/2 = 0.5, well below 3.0 -> not anomalous
    const strategy = new ZScoreAnomalyDetectionStrategy();
    const evaluation = strategy.evaluateAnomaly(13, [10, 12, 14]);

    expect(evaluation.score).toBeCloseTo(0.5, 10);
    expect(evaluation.isAnomalous).toBe(false);
  });

  it('does not evaluate (treats as non-anomalous) when baseline has fewer than 2 points', () => {
    const strategy = new ZScoreAnomalyDetectionStrategy();
    const evaluation = strategy.evaluateAnomaly(100, [12]);
    expect(evaluation.isAnomalous).toBe(false);
    expect(evaluation.score).toBe(0);
  });

  it('emits an AnomalyDetectedEvent with the correct payload when anomalous', () => {
    const emitter = new AnomalyEventEmitter();
    const strategy = new ZScoreAnomalyDetectionStrategy();
    const receivedEvents: AnomalyDetectedEvent[] = [];
    emitter.onAnomalyDetected((event) => receivedEvents.push(event));

    const fixedClock = () => 1_700_000_000_000;
    const wasAnomalous = detectFirmwareCohortDecodeErrorAnomaly(
      'firmware-v9.2.1',
      18,
      [10, 12, 14],
      strategy,
      emitter,
      fixedClock,
    );

    expect(wasAnomalous).toBe(true);
    expect(receivedEvents).toHaveLength(1);
    const event = receivedEvents[0] as AnomalyDetectedEvent;
    expect(event.firmwareCohortId).toBe('firmware-v9.2.1');
    expect(event.observedErrorRate).toBe(18);
    expect(event.baselineMeanErrorRate).toBeCloseTo(12, 10);
    expect(event.baselineStandardDeviationErrorRate).toBeCloseTo(2, 10);
    expect(event.zScore).toBeCloseTo(3, 10);
    expect(event.thresholdCrossed).toBe(3.0);
    expect(event.detectedAtEpochMillis).toBe(1_700_000_000_000);
  });

  it('does not emit an event when the deviation is not anomalous', () => {
    const emitter = new AnomalyEventEmitter();
    const strategy = new ZScoreAnomalyDetectionStrategy();
    const receivedEvents: AnomalyDetectedEvent[] = [];
    emitter.onAnomalyDetected((event) => receivedEvents.push(event));

    const wasAnomalous = detectFirmwareCohortDecodeErrorAnomaly('firmware-v9.2.1', 13, [10, 12, 14], strategy, emitter);

    expect(wasAnomalous).toBe(false);
    expect(receivedEvents).toHaveLength(0);
  });
});

describe('computeFirmwareCohortDecodeErrorRatePerSession', () => {
  function makeBeacon(overrides: Partial<NormalizedPlaybackBeacon>): NormalizedPlaybackBeacon {
    return {
      beaconId: 'b',
      sessionId: 's1',
      assetId: 'a1',
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
      deviceFirmwareVersion: 'fw-1.0.0',
      deviceOperatingSystem: 'tvOS 17.5',
      networkAutonomousSystemNumber: null,
      internetServiceProviderName: null,
      playbackPositionSeconds: null,
      assetDurationSeconds: null,
      traceId: null,
      ...overrides,
    };
  }

  it('computes decodeErrorCount / distinctSessionCount for the given cohort', () => {
    // Cohort "fw-1.0.0": 3 distinct sessions (s1, s2, s3), 2 decode_error beacons -> rate = 2/3
    const beacons = [
      makeBeacon({ sessionId: 's1', eventType: 'decode_error', beaconId: 'e1' }),
      makeBeacon({ sessionId: 's2', eventType: 'decode_error', beaconId: 'e2' }),
      makeBeacon({ sessionId: 's3', eventType: 'heartbeat', beaconId: 'h1' }),
      // A beacon from a different firmware cohort must not count.
      makeBeacon({ sessionId: 's4', eventType: 'decode_error', deviceFirmwareVersion: 'fw-2.0.0', beaconId: 'e3' }),
    ];

    expect(computeFirmwareCohortDecodeErrorRatePerSession(beacons, 'fw-1.0.0')).toBeCloseTo(2 / 3, 10);
  });

  it('returns 0 when the cohort has no sessions in the batch', () => {
    expect(computeFirmwareCohortDecodeErrorRatePerSession([], 'fw-does-not-exist')).toBe(0);
  });
});
