/**
 * @file Unit tests for `ManifestFilterRuleEmitter` and
 * `buildManifestFilterRuleFromAnomalyEvent`
 * (`src/anomaly/manifestFilterRuleEmitter.ts`): rule-construction structure,
 * Observer-pattern subscription to `AnomalyEventEmitter`, and webhook
 * delivery against a fake `fetch`.
 */
import { AnomalyEventEmitter, type AnomalyDetectedEvent } from '../src/anomaly/anomalyEventEmitter';
import {
  ManifestFilterRuleEmitter,
  MANIFEST_FILTER_RULE_WEBHOOK_PATH,
  buildManifestFilterRuleFromAnomalyEvent,
  type ManifestFilterRule,
} from '../src/anomaly/manifestFilterRuleEmitter';
import type { WebhookFetchLike } from '../src/webhooks/webhookRetryHttpClient';

/** A realistic confirmed anomaly: LG TV firmware breaking HEVC hardware decode. */
const lgTvHevcDecodeAnomalyEvent: AnomalyDetectedEvent = {
  firmwareCohortId: 'LG-webOS-06.40.11',
  observedErrorRate: 0.42,
  baselineMeanErrorRate: 0.01,
  baselineStandardDeviationErrorRate: 0.005,
  zScore: 82.0,
  thresholdCrossed: 3.0,
  detectedAtEpochMillis: 1_700_000_000_000,
};

describe('buildManifestFilterRuleFromAnomalyEvent', () => {
  it('builds a structurally correct ManifestFilterRule with downgradeToH264 action', () => {
    const rule = buildManifestFilterRuleFromAnomalyEvent(lgTvHevcDecodeAnomalyEvent, () => 1_700_000_001_000);

    expect(rule).toEqual<ManifestFilterRule>({
      deviceCohortMatcher: { firmwareVersion: 'LG-webOS-06.40.11' },
      action: 'downgradeToH264',
      reason: expect.stringContaining('LG-webOS-06.40.11') as unknown as string,
      sourceAnomaly: {
        firmwareCohortId: 'LG-webOS-06.40.11',
        zScore: 82.0,
        observedErrorRate: 0.42,
        baselineMeanErrorRate: 0.01,
      },
      createdAtEpochMillis: 1_700_000_001_000,
    });
  });

  it('mentions the zScore and threshold in the human-readable reason', () => {
    const rule = buildManifestFilterRuleFromAnomalyEvent(lgTvHevcDecodeAnomalyEvent);
    expect(rule.reason).toContain('82.00');
    expect(rule.reason).toContain('3-sigma');
  });
});

describe('ManifestFilterRuleEmitter', () => {
  it('POSTs the constructed rule to the documented webhook URL when handling an event directly', async () => {
    let capturedUrl: string | undefined;
    let capturedBody: ManifestFilterRule | undefined;
    const fakeFetch: WebhookFetchLike = jest.fn(async (url, init) => {
      capturedUrl = url;
      capturedBody = JSON.parse(init.body) as ManifestFilterRule;
      return { status: 202, ok: true, text: async () => 'accepted' };
    });

    const emitter = new ManifestFilterRuleEmitter({
      edgePackagingBaseUrl: 'https://edge-packaging.internal',
      fetchImplementation: fakeFetch,
      clock: () => 1_700_000_002_000,
    });

    const { rule, deliveryResult } = await emitter.handleAnomalyDetectedEvent(lgTvHevcDecodeAnomalyEvent);

    expect(deliveryResult.delivered).toBe(true);
    expect(capturedUrl).toBe(`https://edge-packaging.internal${MANIFEST_FILTER_RULE_WEBHOOK_PATH}`);
    expect(capturedBody).toEqual(rule);
    expect(rule.action).toBe('downgradeToH264');
    expect(rule.deviceCohortMatcher.firmwareVersion).toBe('LG-webOS-06.40.11');
  });

  it('subscribes to AnomalyEventEmitter and fires the webhook when an anomaly is emitted (Observer pattern)', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => ({ status: 200, ok: true, text: async () => 'ok' }));
    const emitter = new ManifestFilterRuleEmitter({
      edgePackagingBaseUrl: 'https://edge-packaging.internal',
      fetchImplementation: fakeFetch,
    });
    const anomalyEventEmitter = new AnomalyEventEmitter();
    emitter.subscribeToAnomalyEvents(anomalyEventEmitter);

    anomalyEventEmitter.emitAnomalyDetected(lgTvHevcDecodeAnomalyEvent);

    // The handler is async (fire-and-forget from the emitter's perspective);
    // flush microtasks so the fake fetch call has resolved.
    await new Promise((resolve) => setImmediate(resolve));

    expect(fakeFetch).toHaveBeenCalledTimes(1);
  });

  it('reports non-delivery without throwing when the edge-packaging webhook is unreachable', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => {
      throw new Error('ECONNREFUSED');
    });
    const emitter = new ManifestFilterRuleEmitter({
      edgePackagingBaseUrl: 'https://edge-packaging.internal',
      fetchImplementation: fakeFetch,
      retryBackoffOptions: { maxAttempts: 1, initialBackoffMillis: 10, backoffMultiplier: 2 },
    });

    const { deliveryResult } = await emitter.handleAnomalyDetectedEvent(lgTvHevcDecodeAnomalyEvent);

    expect(deliveryResult.delivered).toBe(false);
    expect(deliveryResult.finalErrorMessage).toContain('ECONNREFUSED');
  });
});
