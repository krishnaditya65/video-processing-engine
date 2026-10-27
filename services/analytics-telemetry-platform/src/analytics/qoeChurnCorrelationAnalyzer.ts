/**
 * @file Cohort LTV/churn correlation module.
 *
 * `computeRebufferingChurnPearsonCorrelation` computes the Pearson
 * product-moment correlation coefficient between per-user average
 * rebuffering ratio and a boolean 30-day-churn outcome, over a synthetic
 * joined dataset of `{ userId, averageRebufferingRatio, churnedWithin30Days }`
 * records (the join itself -- QoE beacons on one side, a subscription/churn
 * fact table on the other -- is assumed to have already happened upstream,
 * e.g. in a scheduled ClickHouse/warehouse job; this module is the pure
 * statistics layer on top of that joined result set, mirroring how
 * `anomalyStatistics.ts` is the pure-math layer under
 * `firmwareCohortAnomalyDetector.ts`).
 *
 * Business motivation: this quantifies, with a real statistic rather than
 * intuition, whether "poor streaming quality (rebuffering) causes/correlates
 * with subscriber churn" -- the foundational analysis that justifies
 * QoE-driven engineering investment (encoding quality, CDN capacity, ABR
 * tuning) in terms of retained-revenue impact.
 */

/** One row of the synthetic joined QoE/churn dataset consumed by `computeRebufferingChurnPearsonCorrelation`. */
export interface UserQoeChurnRecord {
  /** Unique subscriber identifier. */
  userId: string;
  /** This user's average rebuffering ratio (see `qoeMetrics.ts`) across their playback sessions in the observation period, in `[0, 1]`. */
  averageRebufferingRatio: number;
  /** Whether this user churned (cancelled/lapsed) within 30 days of the observation period, encoded as a boolean; treated as `1`/`0` for the correlation computation (point-biserial correlation is mathematically identical to Pearson's r applied to a 0/1-coded variable). */
  churnedWithin30Days: boolean;
}

/** Result of `computeRebufferingChurnPearsonCorrelation`. */
export interface QoeChurnCorrelationResult {
  /** The Pearson correlation coefficient, in `[-1, 1]`. */
  pearsonCorrelationCoefficient: number;
  /** Number of records the correlation was computed over. */
  sampleSize: number;
}

/**
 * Computes the Pearson product-moment correlation coefficient between
 * `averageRebufferingRatio` (x) and `churnedWithin30Days` (y, coded as
 * 1 = churned, 0 = retained) across `records`.
 *
 * Formula (standard Pearson's r):
 *
 *   r = Σ((x_i - x̄)(y_i - ȳ)) / sqrt(Σ(x_i - x̄)² * Σ(y_i - ȳ)²)
 *
 * where x̄ and ȳ are the arithmetic means of x and y respectively. Applying
 * this formula to a variable that is binary-coded (0/1) rather than
 * continuous is mathematically well-defined and is exactly the
 * **point-biserial correlation coefficient** -- Pearson's r is not
 * restricted to continuous-continuous pairs; it is simply undefined
 * terminology-wise when one side is binary, hence the more specific name,
 * but the arithmetic (and this implementation) is identical.
 *
 * @param records - The joined per-user QoE/churn dataset; must contain at least 2 records with non-zero variance in both `averageRebufferingRatio` and `churnedWithin30Days` (otherwise the denominator is 0 and correlation is undefined).
 * @returns The Pearson correlation coefficient and the sample size it was computed over.
 * @throws {Error} If `records` has fewer than 2 entries, or if either variable has zero variance across the dataset (correlation undefined -- e.g. every user has an identical rebuffering ratio, or nobody/everybody churned).
 */
export function computeRebufferingChurnPearsonCorrelation(
  records: UserQoeChurnRecord[],
): QoeChurnCorrelationResult {
  if (records.length < 2) {
    throw new Error('computeRebufferingChurnPearsonCorrelation requires at least 2 records');
  }

  const xValues: number[] = records.map((record) => record.averageRebufferingRatio);
  const yValues: number[] = records.map((record) => (record.churnedWithin30Days ? 1 : 0));

  const n = records.length;
  const xMean = xValues.reduce((sum, x) => sum + x, 0) / n;
  const yMean = yValues.reduce((sum, y) => sum + y, 0) / n;

  let sumOfProductsOfDeviations = 0;
  let sumOfSquaredXDeviations = 0;
  let sumOfSquaredYDeviations = 0;

  for (let i = 0; i < n; i += 1) {
    const xDeviation = (xValues[i] as number) - xMean;
    const yDeviation = (yValues[i] as number) - yMean;
    sumOfProductsOfDeviations += xDeviation * yDeviation;
    sumOfSquaredXDeviations += xDeviation * xDeviation;
    sumOfSquaredYDeviations += yDeviation * yDeviation;
  }

  const denominator = Math.sqrt(sumOfSquaredXDeviations * sumOfSquaredYDeviations);
  if (denominator === 0) {
    throw new Error(
      'computeRebufferingChurnPearsonCorrelation: correlation is undefined because at least one variable has zero variance across the dataset',
    );
  }

  return {
    pearsonCorrelationCoefficient: sumOfProductsOfDeviations / denominator,
    sampleSize: n,
  };
}
