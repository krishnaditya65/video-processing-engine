/**
 * @file Unit tests for `postJsonWithRetryBackoff`
 * (`src/webhooks/webhookRetryHttpClient.ts`): retry-with-exponential-backoff
 * behavior against a fake `fetch` implementation, using an injected fake
 * sleep function so the tests run instantly rather than waiting on real
 * backoff delays.
 */
import {
  postJsonWithRetryBackoff,
  DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
  type WebhookFetchLike,
} from '../src/webhooks/webhookRetryHttpClient';

/** Builds a fake `sleepImplementation` that records every requested delay instead of actually waiting. */
function buildRecordingFakeSleep(): { sleep: (delayMillis: number) => Promise<void>; recordedDelays: number[] } {
  const recordedDelays: number[] = [];
  const sleep = async (delayMillis: number): Promise<void> => {
    recordedDelays.push(delayMillis);
  };
  return { sleep, recordedDelays };
}

describe('postJsonWithRetryBackoff', () => {
  it('delivers on the first attempt when fetch succeeds immediately, with no sleeps', async () => {
    const fetchCalls: unknown[] = [];
    const fakeFetch: WebhookFetchLike = jest.fn(async (url, init) => {
      fetchCalls.push({ url, body: init.body });
      return { status: 200, ok: true, text: async () => 'ok' };
    });
    const { sleep, recordedDelays } = buildRecordingFakeSleep();

    const result = await postJsonWithRetryBackoff(
      'https://control-plane.internal/internal/remediation/rollback-experiment',
      { experimentId: 'exp-1' },
      fakeFetch,
      DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
      sleep,
    );

    expect(result).toEqual({ delivered: true, attemptsMade: 1, finalStatus: 200, finalErrorMessage: null });
    expect(fakeFetch).toHaveBeenCalledTimes(1);
    expect(fetchCalls).toEqual([
      { url: 'https://control-plane.internal/internal/remediation/rollback-experiment', body: JSON.stringify({ experimentId: 'exp-1' }) },
    ]);
    expect(recordedDelays).toEqual([]);
  });

  it('retries with exponential backoff (200ms, then 400ms) and succeeds on the 3rd attempt', async () => {
    let callCount = 0;
    const fakeFetch: WebhookFetchLike = jest.fn(async () => {
      callCount += 1;
      if (callCount < 3) {
        return { status: 503, ok: false, text: async () => 'service unavailable' };
      }
      return { status: 200, ok: true, text: async () => 'ok' };
    });
    const { sleep, recordedDelays } = buildRecordingFakeSleep();

    const result = await postJsonWithRetryBackoff(
      'https://edge-packaging.internal/internal/manifest-filters/rules',
      { action: 'downgradeToH264' },
      fakeFetch,
      { maxAttempts: 3, initialBackoffMillis: 200, backoffMultiplier: 2 },
      sleep,
    );

    expect(result).toEqual({ delivered: true, attemptsMade: 3, finalStatus: 200, finalErrorMessage: null });
    expect(fakeFetch).toHaveBeenCalledTimes(3);
    // Exponential backoff: 200 * 2^0 = 200, then 200 * 2^1 = 400.
    expect(recordedDelays).toEqual([200, 400]);
  });

  it('reports non-delivery (never throws) after exhausting all attempts against a persistently failing endpoint', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => ({
      status: 500,
      ok: false,
      text: async () => 'internal server error',
    }));
    const { sleep, recordedDelays } = buildRecordingFakeSleep();

    const result = await postJsonWithRetryBackoff(
      'https://control-plane.internal/internal/remediation/rollback-experiment',
      { experimentId: 'exp-2' },
      fakeFetch,
      { maxAttempts: 3, initialBackoffMillis: 100, backoffMultiplier: 3 },
      sleep,
    );

    expect(result.delivered).toBe(false);
    expect(result.attemptsMade).toBe(3);
    expect(result.finalStatus).toBe(500);
    expect(result.finalErrorMessage).toContain('non-2xx status 500');
    expect(fakeFetch).toHaveBeenCalledTimes(3);
    // 100 * 3^0 = 100, then 100 * 3^1 = 300. Only 2 sleeps between 3 attempts.
    expect(recordedDelays).toEqual([100, 300]);
  });

  it('treats a thrown network error the same as a failed response and still retries', async () => {
    let callCount = 0;
    const fakeFetch: WebhookFetchLike = jest.fn(async () => {
      callCount += 1;
      if (callCount === 1) {
        throw new Error('ECONNREFUSED');
      }
      return { status: 200, ok: true, text: async () => 'ok' };
    });
    const { sleep, recordedDelays } = buildRecordingFakeSleep();

    const result = await postJsonWithRetryBackoff(
      'https://control-plane.internal/internal/remediation/rollback-experiment',
      { experimentId: 'exp-3' },
      fakeFetch,
      DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
      sleep,
    );

    expect(result.delivered).toBe(true);
    expect(result.attemptsMade).toBe(2);
    expect(fakeFetch).toHaveBeenCalledTimes(2);
    expect(recordedDelays).toEqual([200]);
  });

  it('never sleeps after the final attempt (no wasted delay before returning)', async () => {
    const fakeFetch: WebhookFetchLike = jest.fn(async () => ({
      status: 429,
      ok: false,
      text: async () => 'rate limited',
    }));
    const { sleep, recordedDelays } = buildRecordingFakeSleep();

    await postJsonWithRetryBackoff(
      'https://control-plane.internal/internal/remediation/rollback-experiment',
      {},
      fakeFetch,
      { maxAttempts: 1, initialBackoffMillis: 500, backoffMultiplier: 2 },
      sleep,
    );

    expect(fakeFetch).toHaveBeenCalledTimes(1);
    expect(recordedDelays).toEqual([]);
  });
});
