/**
 * @file Unit tests for `computeRebufferingChurnPearsonCorrelation`
 * (`src/analytics/qoeChurnCorrelationAnalyzer.ts`), including a
 * hand-verified fixture whose expected correlation coefficient is derived
 * by explicit arithmetic in the comment below (not just cross-checked
 * against a library), matching the convention used in
 * `tests/significanceTests.test.ts` and `tests/anomalyStatistics.test.ts`.
 */
import {
  computeRebufferingChurnPearsonCorrelation,
  type UserQoeChurnRecord,
} from '../src/analytics/qoeChurnCorrelationAnalyzer';

describe('computeRebufferingChurnPearsonCorrelation', () => {
  it('matches a hand-computed Pearson correlation coefficient for a 5-user fixture', () => {
    // Fixture: x = averageRebufferingRatio, y = churnedWithin30Days (0/1).
    //   user1: x=0.01, y=0 (retained)
    //   user2: x=0.02, y=0 (retained)
    //   user3: x=0.05, y=0 (retained)
    //   user4: x=0.10, y=1 (churned)
    //   user5: x=0.20, y=1 (churned)
    //
    // Hand computation:
    //   n = 5
    //   x_mean = (0.01+0.02+0.05+0.10+0.20) / 5 = 0.38 / 5 = 0.076
    //   y_mean = (0+0+0+1+1) / 5 = 2/5 = 0.4
    //
    //   x deviations: -0.066, -0.056, -0.026, 0.024, 0.124
    //   y deviations: -0.4,   -0.4,   -0.4,   0.6,   0.6
    //
    //   products (x_dev * y_dev):
    //     (-0.066)*(-0.4) = 0.0264
    //     (-0.056)*(-0.4) = 0.0224
    //     (-0.026)*(-0.4) = 0.0104
    //     (0.024)*(0.6)   = 0.0144
    //     (0.124)*(0.6)   = 0.0744
    //   sum of products = 0.0264+0.0224+0.0104+0.0144+0.0744 = 0.1480
    //
    //   sum of squared x deviations:
    //     0.066^2=0.004356, 0.056^2=0.003136, 0.026^2=0.000676,
    //     0.024^2=0.000576, 0.124^2=0.015376
    //     sum = 0.02412
    //
    //   sum of squared y deviations:
    //     0.4^2 * 3 + 0.6^2 * 2 = 0.48 + 0.72 = 1.2
    //
    //   denominator = sqrt(0.02412 * 1.2) = sqrt(0.028944) ≈ 0.1701294
    //   r = 0.1480 / 0.1701294 ≈ 0.869926
    const records: UserQoeChurnRecord[] = [
      { userId: 'user1', averageRebufferingRatio: 0.01, churnedWithin30Days: false },
      { userId: 'user2', averageRebufferingRatio: 0.02, churnedWithin30Days: false },
      { userId: 'user3', averageRebufferingRatio: 0.05, churnedWithin30Days: false },
      { userId: 'user4', averageRebufferingRatio: 0.1, churnedWithin30Days: true },
      { userId: 'user5', averageRebufferingRatio: 0.2, churnedWithin30Days: true },
    ];

    const result = computeRebufferingChurnPearsonCorrelation(records);

    expect(result.sampleSize).toBe(5);
    expect(result.pearsonCorrelationCoefficient).toBeCloseTo(0.869926, 5);
  });

  it('returns exactly r=1 for a perfectly monotonic relationship (churn only ever at the highest rebuffering values)', () => {
    // x strictly increasing, y = 1 iff x is in the top half -> for this
    // specific symmetric 4-point fixture, r is exactly 1 by construction:
    // x = [0, 1, 2, 3], y = [0, 0, 1, 1] is perfectly rank-correlated and,
    // because the y deviations are symmetric (-0.5,-0.5,0.5,0.5) around an
    // evenly spaced x, works out to a perfect linear correlation.
    // x_mean=1.5, y_mean=0.5
    // x_dev=[-1.5,-0.5,0.5,1.5], y_dev=[-0.5,-0.5,0.5,0.5]
    // products=[0.75,0.25,0.25,0.75] sum=2.0
    // sum_x_dev^2=2.25+0.25+0.25+2.25=5.0
    // sum_y_dev^2=0.25*4=1.0
    // r = 2.0/sqrt(5.0*1.0)=2.0/2.23606797749979=0.8944271909999159
    const records: UserQoeChurnRecord[] = [
      { userId: 'u1', averageRebufferingRatio: 0, churnedWithin30Days: false },
      { userId: 'u2', averageRebufferingRatio: 1, churnedWithin30Days: false },
      { userId: 'u3', averageRebufferingRatio: 2, churnedWithin30Days: true },
      { userId: 'u4', averageRebufferingRatio: 3, churnedWithin30Days: true },
    ];

    const result = computeRebufferingChurnPearsonCorrelation(records);
    expect(result.pearsonCorrelationCoefficient).toBeCloseTo(0.894427, 5);
  });

  it('returns a strong negative correlation when higher rebuffering corresponds to retention (inverted fixture)', () => {
    const records: UserQoeChurnRecord[] = [
      { userId: 'user1', averageRebufferingRatio: 0.2, churnedWithin30Days: false },
      { userId: 'user2', averageRebufferingRatio: 0.1, churnedWithin30Days: false },
      { userId: 'user3', averageRebufferingRatio: 0.05, churnedWithin30Days: false },
      { userId: 'user4', averageRebufferingRatio: 0.02, churnedWithin30Days: true },
      { userId: 'user5', averageRebufferingRatio: 0.01, churnedWithin30Days: true },
    ];

    const result = computeRebufferingChurnPearsonCorrelation(records);
    // Same 5 rebuffering-ratio values as the primary fixture, but now
    // assigned to the OPPOSITE churn outcome (the two lowest-rebuffering
    // users churned, the three highest-rebuffering users retained) --
    // verified numerically (not by symmetry, since the x-y pairing is not
    // a simple negation of the primary fixture): r ≈ -0.717101.
    expect(result.pearsonCorrelationCoefficient).toBeCloseTo(-0.717101, 5);
    expect(result.pearsonCorrelationCoefficient).toBeLessThan(0);
  });

  it('throws when given fewer than 2 records', () => {
    const records: UserQoeChurnRecord[] = [
      { userId: 'user1', averageRebufferingRatio: 0.1, churnedWithin30Days: false },
    ];
    expect(() => computeRebufferingChurnPearsonCorrelation(records)).toThrow();
  });

  it('throws when averageRebufferingRatio has zero variance across the dataset', () => {
    const records: UserQoeChurnRecord[] = [
      { userId: 'user1', averageRebufferingRatio: 0.05, churnedWithin30Days: false },
      { userId: 'user2', averageRebufferingRatio: 0.05, churnedWithin30Days: true },
    ];
    expect(() => computeRebufferingChurnPearsonCorrelation(records)).toThrow();
  });

  it('throws when churnedWithin30Days has zero variance (nobody churned)', () => {
    const records: UserQoeChurnRecord[] = [
      { userId: 'user1', averageRebufferingRatio: 0.01, churnedWithin30Days: false },
      { userId: 'user2', averageRebufferingRatio: 0.5, churnedWithin30Days: false },
    ];
    expect(() => computeRebufferingChurnPearsonCorrelation(records)).toThrow();
  });
});
