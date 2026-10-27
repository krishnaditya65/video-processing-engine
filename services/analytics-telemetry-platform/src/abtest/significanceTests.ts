/**
 * @file Statistical significance test primitives for the Automated
 * Encoding A/B Test evaluator.
 *
 * We use Welch's t-test (unequal-variance two-sample t-test) rather than
 * Student's pooled t-test or a two-proportion z-test, because per-session
 * rebuffering ratio is a continuous, bounded-but-not-binary metric whose
 * variance is not assumed equal between control and experiment groups
 * (experiment groups running a new encoding ladder plausibly have
 * different variance, not just different mean, in their rebuffering
 * behavior). Welch's t-test is the standard, more robust choice
 * whenever equal variance cannot be assumed (Welch 1947, "The
 * generalization of Student's problem when several different population
 * variances are involved", Biometrika 34(1-2):28-35).
 */

/** Summary statistics for one experiment arm (control or treatment). */
export interface ExperimentArmAggregate {
  /** Arithmetic mean of the metric (e.g. rebuffering ratio) across sessions in this arm. */
  meanValue: number;
  /** Sample variance of the metric across sessions in this arm. */
  variance: number;
  /** Number of sessions/observations in this arm. */
  sampleSize: number;
}

/** Result of running Welch's t-test between two experiment arms. */
export interface WelchTTestResult {
  /** Welch's t-statistic. */
  tStatistic: number;
  /** Welch-Satterthwaite approximated degrees of freedom. */
  degreesOfFreedom: number;
  /**
   * Two-tailed p-value approximation for the given t-statistic and degrees
   * of freedom, computed via a numerical approximation to the Student's t
   * CDF (see `approximateTwoTailedPValueFromTDistribution`).
   */
  pValue: number;
}

/**
 * Runs Welch's t-test comparing the means of two independent samples given
 * only their summary aggregates (mean, variance, sample size) -- the
 * inputs a ClickHouse `GROUP BY experimentArm` query would naturally
 * produce (`avg()`, `varSamp()`, `count()`), avoiding the need to pull raw
 * per-session rows into this service for the comparison.
 *
 * Formulas (Welch's t-test):
 *
 *   t = (mean1 - mean2) / sqrt(variance1/n1 + variance2/n2)
 *
 *   degreesOfFreedom (Welch-Satterthwaite equation) =
 *       (variance1/n1 + variance2/n2)^2
 *       ----------------------------------------------------
 *       (variance1/n1)^2 / (n1 - 1) + (variance2/n2)^2 / (n2 - 1)
 *
 * @param controlArm - Summary aggregate for the control group.
 * @param treatmentArm - Summary aggregate for the treatment/experiment group.
 * @returns The computed t-statistic, degrees of freedom, and two-tailed p-value.
 * @throws {Error} If either arm has a sample size less than 2.
 */
export function computeWelchTTest(
  controlArm: ExperimentArmAggregate,
  treatmentArm: ExperimentArmAggregate,
): WelchTTestResult {
  if (controlArm.sampleSize < 2 || treatmentArm.sampleSize < 2) {
    throw new Error('computeWelchTTest requires both arms to have a sample size of at least 2');
  }

  const controlVarianceOverN = controlArm.variance / controlArm.sampleSize;
  const treatmentVarianceOverN = treatmentArm.variance / treatmentArm.sampleSize;
  const standardErrorSquared = controlVarianceOverN + treatmentVarianceOverN;
  const standardError = Math.sqrt(standardErrorSquared);

  const tStatistic =
    standardError === 0 ? 0 : (treatmentArm.meanValue - controlArm.meanValue) / standardError;

  const degreesOfFreedomNumerator = standardErrorSquared ** 2;
  const degreesOfFreedomDenominator =
    controlVarianceOverN ** 2 / (controlArm.sampleSize - 1) +
    treatmentVarianceOverN ** 2 / (treatmentArm.sampleSize - 1);
  const degreesOfFreedom =
    degreesOfFreedomDenominator === 0 ? 0 : degreesOfFreedomNumerator / degreesOfFreedomDenominator;

  const pValue = approximateTwoTailedPValueFromTDistribution(tStatistic, degreesOfFreedom);

  return { tStatistic, degreesOfFreedom, pValue };
}

/**
 * Approximates the two-tailed p-value for a given t-statistic and degrees
 * of freedom using the relationship between the Student's t distribution
 * and the regularized incomplete beta function:
 *
 *   P(|T| > |t|) = I_{x}(df/2, 1/2)
 *   where x = df / (df + t^2)
 *
 * and `I_x` is the regularized incomplete beta function, evaluated here
 * via a continued-fraction expansion (Numerical Recipes' `betacf`), which
 * is a standard, well-tested numerical approximation technique for this
 * exact purpose (there is no closed-form elementary expression for the t
 * CDF).
 *
 * @param tStatistic - The computed t-statistic.
 * @param degreesOfFreedom - The computed (possibly fractional, per
 * Welch-Satterthwaite) degrees of freedom.
 * @returns Two-tailed p-value in [0, 1].
 */
function approximateTwoTailedPValueFromTDistribution(
  tStatistic: number,
  degreesOfFreedom: number,
): number {
  if (degreesOfFreedom <= 0) {
    return 1;
  }
  const x = degreesOfFreedom / (degreesOfFreedom + tStatistic * tStatistic);
  return regularizedIncompleteBeta(x, degreesOfFreedom / 2, 0.5);
}

/**
 * Regularized incomplete beta function I_x(a, b), evaluated via the
 * standard continued-fraction method (Lentz's algorithm variant as
 * presented in Numerical Recipes, ch. 6.4). Used to compute Student's t
 * distribution's CDF/p-values without requiring an external statistics
 * library.
 *
 * @param x - Upper integration bound, in [0, 1].
 * @param a - First shape parameter.
 * @param b - Second shape parameter.
 * @returns The regularized incomplete beta function value, in [0, 1].
 */
function regularizedIncompleteBeta(x: number, a: number, b: number): number {
  if (x <= 0) return 0;
  if (x >= 1) return 1;

  const logBeta = logGamma(a + b) - logGamma(a) - logGamma(b) + a * Math.log(x) + b * Math.log(1 - x);
  const front = Math.exp(logBeta);

  // Use the symmetry relation to ensure the continued fraction converges
  // quickly (standard numerical-recipes technique).
  if (x < (a + 1) / (a + b + 2)) {
    return (front * betaContinuedFraction(x, a, b)) / a;
  }
  return 1 - (front * betaContinuedFraction(1 - x, b, a)) / b;
}

/** Continued-fraction evaluation used internally by `regularizedIncompleteBeta`. */
function betaContinuedFraction(x: number, a: number, b: number): number {
  const maxIterations = 200;
  const epsilon = 1e-12;
  const fpMin = 1e-300;

  const qab = a + b;
  const qap = a + 1;
  const qam = a - 1;
  let c = 1;
  let d = 1 - (qab * x) / qap;
  if (Math.abs(d) < fpMin) d = fpMin;
  d = 1 / d;
  let h = d;

  for (let m = 1; m <= maxIterations; m += 1) {
    const m2 = 2 * m;
    let aa = (m * (b - m) * x) / ((qam + m2) * (a + m2));
    d = 1 + aa * d;
    if (Math.abs(d) < fpMin) d = fpMin;
    c = 1 + aa / c;
    if (Math.abs(c) < fpMin) c = fpMin;
    d = 1 / d;
    h *= d * c;

    aa = (-(a + m) * (qab + m) * x) / ((a + m2) * (qap + m2));
    d = 1 + aa * d;
    if (Math.abs(d) < fpMin) d = fpMin;
    c = 1 + aa / c;
    if (Math.abs(c) < fpMin) c = fpMin;
    d = 1 / d;
    const del = d * c;
    h *= del;

    if (Math.abs(del - 1) < epsilon) {
      break;
    }
  }
  return h;
}

/**
 * Log-gamma function (Lanczos approximation), used internally by
 * `regularizedIncompleteBeta` to compute the log-beta normalization
 * constant without overflowing for larger shape parameters.
 *
 * @param z - Input value (must be > 0 for the domains used in this file).
 * @returns The natural logarithm of the gamma function evaluated at `z`.
 */
function logGamma(z: number): number {
  const g = 7;
  const coefficients = [
    0.99999999999980993, 676.5203681218851, -1259.1392167224028, 771.32342877765313, -176.61502916214059,
    12.507343278686905, -0.13857109526572012, 9.9843695780195716e-6, 1.5056327351493116e-7,
  ];

  if (z < 0.5) {
    return Math.log(Math.PI / Math.sin(Math.PI * z)) - logGamma(1 - z);
  }

  const zAdjusted = z - 1;
  let x = coefficients[0] as number;
  for (let i = 1; i < g + 2; i += 1) {
    x += (coefficients[i] as number) / (zAdjusted + i);
  }
  const t = zAdjusted + g + 0.5;
  return 0.5 * Math.log(2 * Math.PI) + (zAdjusted + 0.5) * Math.log(t) - t + Math.log(x);
}
