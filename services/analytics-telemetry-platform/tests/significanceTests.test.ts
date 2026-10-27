/**
 * @file Unit tests for Welch's t-test (`src/abtest/significanceTests.ts`)
 * and the encoding A/B test evaluator built on top of it
 * (`src/abtest/encodingExperimentEvaluator.ts`).
 *
 * The t-statistic and degrees-of-freedom arithmetic is hand-verified in
 * comments. The p-value itself is produced by a numerical CDF
 * approximation (no closed form exists), so instead of hand-computing it
 * digit-by-digit we cross-check it against well-known published critical
 * values from standard Student's t reference tables (e.g. t=2.086 at
 * df=20 is the textbook two-tailed alpha=0.05 critical value), which
 * gives an independently-sourced ground truth to assert against.
 */
import { computeWelchTTest, type ExperimentArmAggregate } from '../src/abtest/significanceTests';
import {
  evaluateEncodingExperimentStatisticalSignificance,
  WelchTTestSignificanceTestStrategy,
} from '../src/abtest/encodingExperimentEvaluator';

describe('computeWelchTTest', () => {
  it('returns t=0 and p=1 for two identical-mean arms', () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.001, sampleSize: 50 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.001, sampleSize: 50 };

    // t = (mean2 - mean1) / sqrt(v1/n1 + v2/n2) = (0.05 - 0.05) / sqrt(...) = 0 / sqrt(...) = 0
    const result = computeWelchTTest(controlArm, treatmentArm);

    expect(result.tStatistic).toBe(0);
    expect(result.pValue).toBeCloseTo(1, 6);
  });

  it('matches the textbook two-tailed alpha=0.05 critical t-value of 2.086 at df=20', () => {
    // Construct two arms with EQUAL variance (v=1) and EQUAL sample size (n=11).
    // With v1=v2=v and n1=n2=n, the Welch-Satterthwaite degrees of freedom
    // simplifies (the v/n terms cancel) to exactly 2*(n-1):
    //
    //   SE^2 = v/n + v/n = 2v/n
    //   df = (2v/n)^2 / [2 * (v/n)^2 / (n-1)] = 4(v/n)^2 / [2(v/n)^2/(n-1)] = 2(n-1)
    //
    // For n=11: df = 2*(11-1) = 20.
    //
    // We then solve for the mean difference that produces t = 2.086 exactly:
    //   t = meanDiff / sqrt(2v/n) = meanDiff / sqrt(2*1/11) = meanDiff / sqrt(0.181818...)
    //   sqrt(0.181818...) = 0.4264014...
    //   meanDiff = 2.086 * 0.4264014... = 0.889465...
    const n = 11;
    const v = 1;
    const targetT = 2.086;
    const standardError = Math.sqrt((2 * v) / n);
    const meanDiff = targetT * standardError;

    const controlArm: ExperimentArmAggregate = { meanValue: 0, variance: v, sampleSize: n };
    const treatmentArm: ExperimentArmAggregate = { meanValue: meanDiff, variance: v, sampleSize: n };

    const result = computeWelchTTest(controlArm, treatmentArm);

    expect(result.degreesOfFreedom).toBeCloseTo(20, 6);
    expect(result.tStatistic).toBeCloseTo(2.086, 3);
    // The textbook two-tailed p-value at t=2.086, df=20 is 0.05 (that IS
    // the definition of the critical value); allow a small tolerance for
    // numerical approximation error in our incomplete-beta implementation.
    expect(result.pValue).toBeCloseTo(0.05, 2);
  });

  it('produces a large |t| and near-zero p-value for a strongly significant difference', () => {
    // control: mean=0.05, variance=0.0001, n=100
    // treatment: mean=0.01, variance=0.0001, n=100
    // SE^2 = 0.0001/100 + 0.0001/100 = 0.000001 + 0.000001 = 0.000002
    // SE = sqrt(0.000002) = 0.0014142135...
    // t = (0.01 - 0.05) / 0.0014142135... = -0.04 / 0.0014142135... = -28.284271...
    const controlArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };

    const result = computeWelchTTest(controlArm, treatmentArm);

    expect(result.tStatistic).toBeCloseTo(-28.2842712, 3);
    expect(result.pValue).toBeLessThan(0.0001);
  });

  it('throws when a sample size is less than 2', () => {
    const tinyArm: ExperimentArmAggregate = { meanValue: 0, variance: 1, sampleSize: 1 };
    const normalArm: ExperimentArmAggregate = { meanValue: 0, variance: 1, sampleSize: 10 };
    expect(() => computeWelchTTest(tinyArm, normalArm)).toThrow();
  });
});

describe('evaluateEncodingExperimentStatisticalSignificance', () => {
  it('decides PROMOTE when treatment rebuffering ratio is significantly lower than control', () => {
    // Same strongly-significant fixture as above: treatment mean (0.01) < control mean (0.05), p << 0.05.
    const controlArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };

    const result = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);

    expect(result.decision).toBe('PROMOTE');
    expect(result.pValue).toBeLessThan(0.05);
  });

  it('decides ROLLBACK when treatment rebuffering ratio is significantly higher than control', () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };

    const result = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);

    expect(result.decision).toBe('ROLLBACK');
    expect(result.pValue).toBeLessThan(0.05);
  });

  it('decides INCONCLUSIVE when the arms are statistically indistinguishable', () => {
    // Identical means -> p=1, well above alpha=0.05.
    const controlArm: ExperimentArmAggregate = { meanValue: 0.03, variance: 0.0005, sampleSize: 40 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.03, variance: 0.0006, sampleSize: 40 };

    const result = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);

    expect(result.decision).toBe('INCONCLUSIVE');
    expect(result.pValue).toBeGreaterThanOrEqual(0.05);
  });

  it('accepts an injected SignificanceTestStrategy (Strategy pattern)', () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };

    const strategy = new WelchTTestSignificanceTestStrategy();
    const result = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm, 0.05, strategy);

    expect(result.decision).toBe('PROMOTE');
  });
});
