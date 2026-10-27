/**
 * @file Automated Encoding A/B Test evaluator.
 *
 * Given control-group and experiment-group rebuffering-ratio aggregates,
 * runs Welch's t-test (see `significanceTests.ts`) and produces a
 * promote/rollback/inconclusive decision per a documented confidence
 * threshold. This is the Strategy-pattern consumer side referenced in
 * README "Design Patterns": the significance-test *algorithm* itself is
 * injected as a `SignificanceTestStrategy`, so a two-proportion z-test or
 * any other test could be substituted without changing the decision logic
 * below.
 */
import { computeWelchTTest, type ExperimentArmAggregate } from './significanceTests';

/** Possible outcomes of an encoding A/B test evaluation. */
export type EncodingExperimentDecision = 'PROMOTE' | 'ROLLBACK' | 'INCONCLUSIVE';

/** Result of evaluating an encoding experiment's statistical significance. */
export interface EncodingExperimentEvaluationResult {
  /** The computed t-statistic from the significance test. */
  tStatistic: number;
  /** The computed (Welch-Satterthwaite) degrees of freedom. */
  degreesOfFreedom: number;
  /** The computed two-tailed p-value. */
  pValue: number;
  /** The confidence threshold (alpha) the decision was made against. */
  significanceLevelAlpha: number;
  /** The final decision. */
  decision: EncodingExperimentDecision;
  /** Human-readable rationale for the decision, for audit logs/dashboards. */
  rationale: string;
}

/**
 * Strategy interface for pluggable significance tests, allowing
 * `evaluateEncodingExperimentStatisticalSignificance` to be parameterized
 * over different statistical tests (Welch's t-test by default; a
 * two-proportion z-test could be substituted for a binary/conversion-style
 * metric).
 */
export interface SignificanceTestStrategy {
  /**
   * Runs the underlying significance test.
   *
   * @param controlArm - Control group aggregate.
   * @param treatmentArm - Treatment/experiment group aggregate.
   * @returns The p-value and any test-specific statistics.
   */
  runTest(
    controlArm: ExperimentArmAggregate,
    treatmentArm: ExperimentArmAggregate,
  ): { pValue: number; tStatistic: number; degreesOfFreedom: number };
}

/** Default `SignificanceTestStrategy` backed by Welch's t-test. */
export class WelchTTestSignificanceTestStrategy implements SignificanceTestStrategy {
  /** @inheritdoc */
  public runTest(
    controlArm: ExperimentArmAggregate,
    treatmentArm: ExperimentArmAggregate,
  ): { pValue: number; tStatistic: number; degreesOfFreedom: number } {
    return computeWelchTTest(controlArm, treatmentArm);
  }
}

/**
 * Default significance level (alpha) used to decide statistical
 * significance: 0.05, the conventional 95% confidence threshold used
 * broadly across A/B testing practice. A p-value below this threshold is
 * treated as "the observed difference is unlikely to be due to chance
 * alone."
 */
export const DEFAULT_SIGNIFICANCE_LEVEL_ALPHA = 0.05;

/**
 * Evaluates an encoding A/B experiment's statistical significance by
 * comparing control vs. treatment rebuffering-ratio aggregates, and
 * produces a promote/rollback/inconclusive decision.
 *
 * Decision policy (documented):
 *   - If p-value >= significanceLevelAlpha: `INCONCLUSIVE` -- we cannot
 *     reject the null hypothesis that the two arms have equal mean
 *     rebuffering ratio, so we do not act (neither promote nor rollback);
 *     the experiment should keep collecting data.
 *   - If p-value < significanceLevelAlpha AND treatment mean rebuffering
 *     ratio is LOWER than control: `PROMOTE` -- the new encoding
 *     configuration significantly reduces rebuffering and should ship.
 *   - If p-value < significanceLevelAlpha AND treatment mean rebuffering
 *     ratio is HIGHER than control: `ROLLBACK` -- the new encoding
 *     configuration significantly *increases* rebuffering and must be
 *     reverted.
 *
 * @param controlArm - Control group rebuffering-ratio aggregate.
 * @param treatmentArm - Treatment/experiment group rebuffering-ratio aggregate.
 * @param significanceLevelAlpha - Confidence threshold; defaults to
 * `DEFAULT_SIGNIFICANCE_LEVEL_ALPHA` (0.05, i.e. 95% confidence).
 * @param significanceTestStrategy - Pluggable significance test
 * implementation; defaults to `WelchTTestSignificanceTestStrategy`.
 * @returns The full evaluation result including the decision and rationale.
 */
export function evaluateEncodingExperimentStatisticalSignificance(
  controlArm: ExperimentArmAggregate,
  treatmentArm: ExperimentArmAggregate,
  significanceLevelAlpha: number = DEFAULT_SIGNIFICANCE_LEVEL_ALPHA,
  significanceTestStrategy: SignificanceTestStrategy = new WelchTTestSignificanceTestStrategy(),
): EncodingExperimentEvaluationResult {
  const { pValue, tStatistic, degreesOfFreedom } = significanceTestStrategy.runTest(controlArm, treatmentArm);

  let decision: EncodingExperimentDecision;
  let rationale: string;

  if (pValue >= significanceLevelAlpha) {
    decision = 'INCONCLUSIVE';
    rationale = `p-value ${pValue.toFixed(4)} >= alpha ${significanceLevelAlpha}: cannot reject the null hypothesis of equal mean rebuffering ratio; continue collecting data.`;
  } else if (treatmentArm.meanValue < controlArm.meanValue) {
    decision = 'PROMOTE';
    rationale = `p-value ${pValue.toFixed(4)} < alpha ${significanceLevelAlpha} and treatment mean rebuffering ratio (${treatmentArm.meanValue}) is significantly lower than control (${controlArm.meanValue}); promote the experiment encoding configuration.`;
  } else {
    decision = 'ROLLBACK';
    rationale = `p-value ${pValue.toFixed(4)} < alpha ${significanceLevelAlpha} and treatment mean rebuffering ratio (${treatmentArm.meanValue}) is significantly higher than control (${controlArm.meanValue}); roll back the experiment encoding configuration.`;
  }

  return { tStatistic, degreesOfFreedom, pValue, significanceLevelAlpha, decision, rationale };
}
