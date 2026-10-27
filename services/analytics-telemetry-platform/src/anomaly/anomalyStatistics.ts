/**
 * @file Pure statistics primitives (mean, sample standard deviation,
 * z-score) underpinning the hardware decoder anomaly detection engine.
 *
 * Kept separate from `firmwareCohortAnomalyDetector.ts` so the raw math is
 * independently unit-testable with hand-computed fixtures, and so it can
 * be reused by other Strategy implementations (see `AnomalyDetectionStrategy`
 * in `firmwareCohortAnomalyDetector.ts`) beyond firmware-cohort decode
 * errors.
 */

/**
 * Computes the arithmetic mean of a numeric array.
 *
 * Formula: mean = (sum of all values) / (count of values).
 *
 * @param values - Non-empty array of numbers.
 * @returns The arithmetic mean.
 * @throws {Error} If `values` is empty.
 */
export function computeArithmeticMean(values: number[]): number {
  if (values.length === 0) {
    throw new Error('computeArithmeticMean requires a non-empty array');
  }
  return values.reduce((sum, value) => sum + value, 0) / values.length;
}

/**
 * Computes the sample standard deviation of a numeric array, using
 * Bessel's correction (dividing by N-1 rather than N), which is the
 * standard unbiased estimator of population variance when the input is a
 * *sample* drawn from a larger population (here: a rolling baseline
 * window of historical error-rate observations, not the full population
 * of all possible observations).
 *
 * Formula:
 *   variance = sum((x_i - mean)^2) / (N - 1)
 *   stddev = sqrt(variance)
 *
 * @param values - Array of numbers with length >= 2 (sample stddev is
 * undefined for N < 2).
 * @returns The sample standard deviation.
 * @throws {Error} If `values` has fewer than 2 elements.
 */
export function computeSampleStandardDeviation(values: number[]): number {
  if (values.length < 2) {
    throw new Error('computeSampleStandardDeviation requires at least 2 values');
  }
  const mean = computeArithmeticMean(values);
  const sumOfSquaredDeviations = values.reduce((sum, value) => sum + (value - mean) ** 2, 0);
  const variance = sumOfSquaredDeviations / (values.length - 1);
  return Math.sqrt(variance);
}

/**
 * Computes the z-score of an observed value against a baseline
 * distribution's mean and standard deviation: how many standard
 * deviations the observation is above/below the baseline mean.
 *
 * Formula: z = (observedValue - baselineMean) / baselineStandardDeviation
 *
 * @param observedValue - The value being tested for anomalousness.
 * @param baselineMean - Mean of the rolling baseline window.
 * @param baselineStandardDeviation - Sample standard deviation of the
 * rolling baseline window.
 * @returns The z-score. Returns 0 if `baselineStandardDeviation` is 0 and
 * `observedValue === baselineMean` (no deviation, no variance); returns
 * `Infinity`/`-Infinity` with the sign of the deviation if
 * `baselineStandardDeviation` is 0 but the observation differs from the
 * mean (a genuinely unbounded anomaly against a zero-variance baseline).
 */
export function computeZScore(
  observedValue: number,
  baselineMean: number,
  baselineStandardDeviation: number,
): number {
  const deviation = observedValue - baselineMean;
  if (baselineStandardDeviation === 0) {
    if (deviation === 0) {
      return 0;
    }
    return deviation > 0 ? Infinity : -Infinity;
  }
  return deviation / baselineStandardDeviation;
}
