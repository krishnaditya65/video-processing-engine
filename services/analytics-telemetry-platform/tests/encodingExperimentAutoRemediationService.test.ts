/**
 * @file Unit tests for `EncodingExperimentAutoRemediationService`
 * (`src/abtest/encodingExperimentAutoRemediationService.ts`): verifies it
 * only calls the rollback webhook for `ROLLBACK` verdicts, POSTs to the
 * documented URL with the documented JSON body shape, and surfaces webhook
 * failures without throwing. The HTTP layer is a fake `fetch`; no real
 * network call is made.
 */
import {
  EncodingExperimentAutoRemediationService,
  ROLLBACK_EXPERIMENT_WEBHOOK_PATH,
  type EncodingExperimentRollbackWebhookRequestBody,
} from '../src/abtest/encodingExperimentAutoRemediationService';
import { evaluateEncodingExperimentStatisticalSignificance } from '../src/abtest/encodingExperimentEvaluator';
import type { ExperimentArmAggregate } from '../src/abtest/significanceTests';
import type { WebhookFetchLike } from '../src/webhooks/webhookRetryHttpClient';

const instantSleep = async (): Promise<void> => {
  // Instant, no real delay -- keeps failure-path tests fast.
};

describe('EncodingExperimentAutoRemediationService', () => {
  it('does nothing (returns null, calls no webhook) for a PROMOTE verdict', async () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };
    const evaluationResult = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);
    expect(evaluationResult.decision).toBe('PROMOTE');

    const fakeFetch: WebhookFetchLike = jest.fn();
    const service = new EncodingExperimentAutoRemediationService({
      controlPlaneBaseUrl: 'https://control-plane.internal',
      fetchImplementation: fakeFetch,
      sleepImplementation: instantSleep,
    });

    const result = await service.handleEncodingExperimentVerdict('exp-promote', controlArm, treatmentArm, evaluationResult);

    expect(result).toBeNull();
    expect(fakeFetch).not.toHaveBeenCalled();
  });

  it('does nothing for an INCONCLUSIVE verdict', async () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.03, variance: 0.0005, sampleSize: 40 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.03, variance: 0.0006, sampleSize: 40 };
    const evaluationResult = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);
    expect(evaluationResult.decision).toBe('INCONCLUSIVE');

    const fakeFetch: WebhookFetchLike = jest.fn();
    const service = new EncodingExperimentAutoRemediationService({
      controlPlaneBaseUrl: 'https://control-plane.internal',
      fetchImplementation: fakeFetch,
    });

    const result = await service.handleEncodingExperimentVerdict('exp-inconclusive', controlArm, treatmentArm, evaluationResult);

    expect(result).toBeNull();
    expect(fakeFetch).not.toHaveBeenCalled();
  });

  it('POSTs the documented webhook body to the documented URL for a ROLLBACK verdict', async () => {
    const controlArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const evaluationResult = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);
    expect(evaluationResult.decision).toBe('ROLLBACK');

    let capturedUrl: string | undefined;
    let capturedBody: EncodingExperimentRollbackWebhookRequestBody | undefined;
    const fakeFetch: WebhookFetchLike = jest.fn(async (url, init) => {
      capturedUrl = url;
      capturedBody = JSON.parse(init.body) as EncodingExperimentRollbackWebhookRequestBody;
      return { status: 200, ok: true, text: async () => 'ok' };
    });

    const service = new EncodingExperimentAutoRemediationService({
      controlPlaneBaseUrl: 'https://control-plane.internal/',
      fetchImplementation: fakeFetch,
      clock: () => 1_700_000_000_000,
    });

    const result = await service.handleEncodingExperimentVerdict('exp-rollback-1', controlArm, treatmentArm, evaluationResult);

    expect(result?.delivered).toBe(true);
    expect(capturedUrl).toBe(`https://control-plane.internal${ROLLBACK_EXPERIMENT_WEBHOOK_PATH}`);
    expect(capturedBody).toEqual<EncodingExperimentRollbackWebhookRequestBody>({
      experimentId: 'exp-rollback-1',
      decision: 'ROLLBACK',
      controlArmMeanRebufferingRatio: 0.01,
      treatmentArmMeanRebufferingRatio: 0.05,
      pValue: evaluationResult.pValue,
      significanceLevelAlpha: evaluationResult.significanceLevelAlpha,
      rationale: evaluationResult.rationale,
      detectedAtEpochMillis: 1_700_000_000_000,
    });
  });

  it('strips a trailing slash from controlPlaneBaseUrl exactly once', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => ({ status: 200, ok: true, text: async () => 'ok' }));
    const service = new EncodingExperimentAutoRemediationService({
      controlPlaneBaseUrl: 'https://control-plane.internal///',
      fetchImplementation: fakeFetch,
    });

    const controlArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const evaluationResult = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);

    await service.handleEncodingExperimentVerdict('exp-slash', controlArm, treatmentArm, evaluationResult);

    expect(fakeFetch).toHaveBeenCalledWith(
      `https://control-plane.internal${ROLLBACK_EXPERIMENT_WEBHOOK_PATH}`,
      expect.anything(),
    );
  });

  it('retries on webhook failure and reports non-delivery without throwing, using the configured retry policy', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => ({
      status: 500,
      ok: false,
      text: async () => 'boom',
    }));
    const service = new EncodingExperimentAutoRemediationService({
      controlPlaneBaseUrl: 'https://control-plane.internal',
      fetchImplementation: fakeFetch,
      retryBackoffOptions: { maxAttempts: 2, initialBackoffMillis: 10, backoffMultiplier: 2 },
      sleepImplementation: instantSleep,
    });

    const controlArm: ExperimentArmAggregate = { meanValue: 0.01, variance: 0.0001, sampleSize: 100 };
    const treatmentArm: ExperimentArmAggregate = { meanValue: 0.05, variance: 0.0001, sampleSize: 100 };
    const evaluationResult = evaluateEncodingExperimentStatisticalSignificance(controlArm, treatmentArm);

    const result = await service.handleEncodingExperimentVerdict('exp-fail', controlArm, treatmentArm, evaluationResult);

    expect(result?.delivered).toBe(false);
    expect(result?.attemptsMade).toBe(2);
    expect(fakeFetch).toHaveBeenCalledTimes(2);
  });
});
