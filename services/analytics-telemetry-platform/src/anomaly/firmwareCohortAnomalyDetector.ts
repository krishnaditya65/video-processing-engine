/**
 * @file Hardware Decoder Anomaly Detection engine.
 *
 * Given a stream of `decode_error` beacons tagged with device firmware
 * version, computes a per-firmware-cohort decode-error rate, compares it
 * against a rolling baseline via z-score (see `anomalyStatistics.ts`), and
 * emits an `AnomalyDetectedEvent` (via `AnomalyEventEmitter`, Observer
 * pattern) whenever a cohort's z-score crosses the documented threshold.
 *
 * The detection algorithm itself is pluggable via the
 * `AnomalyDetectionStrategy` interface (Strategy pattern referenced in
 * README "Design Patterns") -- `ZScoreAnomalyDetectionStrategy` is the
 * concrete implementation provided here, but a caller could substitute an
 * alternative (e.g. an EWMA-based or IQR-based strategy) without changing
 * `FirmwareCohortAnomalyDetector`.
 */
import {
  computeArithmeticMean,
  computeSampleStandardDeviation,
  computeZScore,
} from './anomalyStatistics';
import type { AnomalyDetectedEvent, AnomalyEventEmitter } from './anomalyEventEmitter';
import type { NormalizedPlaybackBeacon } from '../types/beaconTypes';

/**
 * Default z-score threshold beyond which a firmware cohort's decode-error
 * rate is considered anomalous.
 *
 * Rationale: under a normal (Gaussian) distribution, |z| > 3 corresponds
 * to roughly the 99.7th percentile (three-sigma rule) -- i.e. under normal
 * operating variance, an observation this far from the baseline mean
 * would be expected by chance well under 0.3% of the time. Three sigma is
 * the conventional "this is not noise" threshold used broadly in
 * statistical process control (e.g. manufacturing SPC charts), and is
 * conservative enough to avoid paging on ordinary day-to-day fluctuation
 * while still catching meaningful firmware regressions.
 */
export const DEFAULT_FIRMWARE_ANOMALY_Z_SCORE_THRESHOLD = 3.0;

/** One rolling-baseline observation of a firmware cohort's decode-error rate. */
export interface FirmwareCohortErrorRateObservation {
  /** Device firmware version string identifying the cohort. */
  firmwareCohortId: string;
  /** Decode errors observed per session in this observation window. */
  errorRatePerSession: number;
}

/**
 * Strategy interface for pluggable anomaly-detection algorithms, per the
 * Strategy pattern documented in README "Design Patterns". Implementations
 * decide, given a current observation and a historical baseline window,
 * whether the observation is anomalous and by what score.
 */
export interface AnomalyDetectionStrategy {
  /**
   * Evaluates whether a current observation is anomalous relative to a
   * historical baseline window of prior observations.
   *
   * @param currentErrorRate - The error rate observed in the current
   * window for a single cohort.
   * @param baselineErrorRates - Historical error-rate observations for the
   * same cohort (the "rolling baseline").
   * @returns The computed anomaly score and whether it crosses this
   * strategy's threshold.
   */
  evaluateAnomaly(
    currentErrorRate: number,
    baselineErrorRates: number[],
  ): { score: number; isAnomalous: boolean; threshold: number };
}

/**
 * Z-score-based `AnomalyDetectionStrategy` implementation: computes the
 * baseline's mean and sample standard deviation, then flags an observation
 * anomalous when `|zScore| >= thresholdZScore`.
 */
export class ZScoreAnomalyDetectionStrategy implements AnomalyDetectionStrategy {
  /**
   * @param thresholdZScore - Absolute z-score threshold; defaults to
   * `DEFAULT_FIRMWARE_ANOMALY_Z_SCORE_THRESHOLD`.
   */
  public constructor(
    private readonly thresholdZScore: number = DEFAULT_FIRMWARE_ANOMALY_Z_SCORE_THRESHOLD,
  ) {}

  /** @inheritdoc */
  public evaluateAnomaly(
    currentErrorRate: number,
    baselineErrorRates: number[],
  ): { score: number; isAnomalous: boolean; threshold: number } {
    if (baselineErrorRates.length < 2) {
      // Not enough baseline history to compute a meaningful stddev; treat
      // as not-yet-evaluable rather than raising a false anomaly.
      return { score: 0, isAnomalous: false, threshold: this.thresholdZScore };
    }
    const baselineMean = computeArithmeticMean(baselineErrorRates);
    const baselineStdDev = computeSampleStandardDeviation(baselineErrorRates);
    const zScore = computeZScore(currentErrorRate, baselineMean, baselineStdDev);
    return {
      score: zScore,
      isAnomalous: Math.abs(zScore) >= this.thresholdZScore,
      threshold: this.thresholdZScore,
    };
  }
}

/**
 * Detects, for a single firmware cohort, whether its current-window
 * decode-error rate is anomalous relative to its rolling baseline, using
 * the supplied `AnomalyDetectionStrategy`, and -- if anomalous -- emits an
 * `AnomalyDetectedEvent` through the supplied `AnomalyEventEmitter`.
 *
 * This is the pure decision function (no I/O); the emitter call is a
 * synchronous, in-process notification, keeping this function easy to
 * unit test end-to-end including the emitted event payload.
 *
 * @param firmwareCohortId - Device firmware version identifying the cohort.
 * @param currentErrorRatePerSession - Decode errors observed per session in the current window.
 * @param baselineErrorRatesPerSession - Historical per-window error rates for this cohort.
 * @param strategy - Pluggable anomaly-detection algorithm.
 * @param eventEmitter - Observer-pattern emitter to publish `AnomalyDetectedEvent` on.
 * @param clock - Injectable clock function (defaults to `Date.now`).
 * @returns `true` if an anomaly was detected (and thus an event emitted), else `false`.
 */
export function detectFirmwareCohortDecodeErrorAnomaly(
  firmwareCohortId: string,
  currentErrorRatePerSession: number,
  baselineErrorRatesPerSession: number[],
  strategy: AnomalyDetectionStrategy,
  eventEmitter: AnomalyEventEmitter,
  clock: () => number = Date.now,
): boolean {
  const evaluation = strategy.evaluateAnomaly(currentErrorRatePerSession, baselineErrorRatesPerSession);

  if (!evaluation.isAnomalous) {
    return false;
  }

  const baselineMean =
    baselineErrorRatesPerSession.length > 0 ? computeArithmeticMean(baselineErrorRatesPerSession) : 0;
  const baselineStdDev =
    baselineErrorRatesPerSession.length >= 2
      ? computeSampleStandardDeviation(baselineErrorRatesPerSession)
      : 0;

  const event: AnomalyDetectedEvent = {
    firmwareCohortId,
    observedErrorRate: currentErrorRatePerSession,
    baselineMeanErrorRate: baselineMean,
    baselineStandardDeviationErrorRate: baselineStdDev,
    zScore: evaluation.score,
    thresholdCrossed: evaluation.threshold,
    detectedAtEpochMillis: clock(),
  };

  eventEmitter.emitAnomalyDetected(event);
  return true;
}

/**
 * Computes the decode-error rate per session for a single firmware cohort
 * from a raw beacon batch: count of `decode_error` beacons for that
 * firmware version divided by count of distinct sessions observed for
 * that firmware version in the batch.
 *
 * Formula: errorRatePerSession = decodeErrorBeaconCount / distinctSessionCount
 *
 * @param beacons - Batch of normalized beacons, potentially spanning
 * multiple firmware cohorts.
 * @param firmwareCohortId - The firmware version to compute the rate for.
 * @returns The decode-error rate per session for the given cohort, or 0 if
 * no sessions for that cohort are present in the batch.
 */
export function computeFirmwareCohortDecodeErrorRatePerSession(
  beacons: NormalizedPlaybackBeacon[],
  firmwareCohortId: string,
): number {
  const cohortBeacons = beacons.filter((beacon) => beacon.deviceFirmwareVersion === firmwareCohortId);

  const distinctSessionIds = new Set(cohortBeacons.map((beacon) => beacon.sessionId));
  if (distinctSessionIds.size === 0) {
    return 0;
  }

  const decodeErrorCount = cohortBeacons.filter((beacon) => beacon.eventType === 'decode_error').length;
  return decodeErrorCount / distinctSessionIds.size;
}
