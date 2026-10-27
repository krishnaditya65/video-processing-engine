/**
 * @file Shared outbound-webhook HTTP client with retry-with-exponential-backoff.
 *
 * Both `EncodingExperimentAutoRemediationService`
 * (`src/abtest/encodingExperimentAutoRemediationService.ts`) and
 * `ManifestFilterRuleEmitter` (`src/anomaly/manifestFilterRuleEmitter.ts`)
 * need to make a best-effort outbound `POST` to a sibling service's HTTP
 * endpoint and survive transient failures (the sibling service being
 * briefly unavailable, a blip in network connectivity, etc.). This module
 * factors that concern out into one small, independently-tested utility
 * rather than duplicating retry logic in both call sites.
 *
 * Deliberately does NOT reference the global `fetch`/`Response` DOM types
 * directly in its public signatures -- this project's `tsconfig.json` does
 * not include the `DOM` lib (see `tsconfig.json`), and the ambient
 * `fetch`/`Response` globals shipped by `@types/node` require
 * `moduleResolution: "nodenext"` to resolve their `undici-types`
 * dependency, which this project does not use. Instead, a minimal
 * `WebhookFetchLike` function-shape interface is defined and injected,
 * matching the Dependency Inversion / Strategy style already used
 * elsewhere in this codebase (e.g. `AnomalyDetectionStrategy`,
 * `ChunkProvenanceLookup`). At runtime, the real global `fetch` (available
 * natively since Node 18, see `package.json` "engines") is used as the
 * default implementation via a narrow cast; tests inject a fake conforming
 * to the same interface.
 */

/** Minimal HTTP response surface this client depends on -- a structural subset of the real Fetch API `Response`. */
export interface WebhookHttpResponseLike {
  /** HTTP status code of the response. */
  status: number;
  /** `true` for 2xx status codes, matching the Fetch API `Response.ok` semantics. */
  ok: boolean;
  /** Resolves with the response body as text (used only for error-message construction, never parsed). */
  text(): Promise<string>;
}

/** Minimal shape of the global `fetch` function this client depends on, restricted to a JSON `POST`. */
export type WebhookFetchLike = (
  url: string,
  init: { method: 'POST'; headers: Record<string, string>; body: string },
) => Promise<WebhookHttpResponseLike>;

/**
 * Default `WebhookFetchLike` implementation: the real global `fetch`
 * (available natively in Node >= 18, per this service's `engines` field in
 * `package.json`), narrowly cast to `WebhookFetchLike` to avoid depending
 * on the ambient DOM `fetch`/`Response` type declarations (see file
 * header). Resolved lazily inside `postJsonWithRetryBackoff`'s default
 * parameter rather than at module load time so that environments without a
 * global `fetch` (very old Node) don't throw merely from importing this
 * module.
 */
function resolveGlobalFetch(): WebhookFetchLike {
  const globalFetch = (globalThis as unknown as { fetch?: WebhookFetchLike }).fetch;
  if (typeof globalFetch !== 'function') {
    throw new Error(
      'Global fetch is not available in this runtime; inject an explicit WebhookFetchLike implementation.',
    );
  }
  return globalFetch;
}

/** Configuration for `postJsonWithRetryBackoff`'s retry-with-exponential-backoff policy. */
export interface WebhookRetryBackoffOptions {
  /** Maximum number of attempts (including the first), e.g. `3` = 1 initial try + 2 retries. */
  maxAttempts: number;
  /** Delay, in milliseconds, before the first retry. */
  initialBackoffMillis: number;
  /** Multiplier applied to the backoff delay after each failed attempt (exponential backoff). */
  backoffMultiplier: number;
}

/**
 * Default retry policy: 3 total attempts, starting at a 200ms delay and
 * doubling each retry (200ms, then 400ms), which bounds worst-case total
 * added latency for a fully-down webhook receiver to ~600ms before giving
 * up -- short enough not to meaningfully stall the calling
 * detector/evaluator's event-handling path, while still absorbing brief
 * transient blips (a rolling deploy of the receiving service, a single
 * dropped TCP connection).
 */
export const DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS: WebhookRetryBackoffOptions = {
  maxAttempts: 3,
  initialBackoffMillis: 200,
  backoffMultiplier: 2,
};

/** The outcome of a `postJsonWithRetryBackoff` call, for callers that want to log/observe the retry history. */
export interface WebhookDeliveryResult {
  /** Whether the webhook was ultimately delivered successfully (some attempt returned an `ok` response). */
  delivered: boolean;
  /** Total number of attempts made (1 if the first attempt succeeded). */
  attemptsMade: number;
  /** HTTP status code of the final attempt, if any attempt received a response at all. */
  finalStatus: number | null;
  /** Error message from the final failed attempt, if `delivered` is `false`. */
  finalErrorMessage: string | null;
}

/**
 * Sleeps for `delayMillis` milliseconds. Extracted as a standalone,
 * injectable function so tests can use Jest fake timers (`jest.advanceTimersByTimeAsync`)
 * to exercise the full backoff schedule without real wall-clock delay.
 *
 * @param delayMillis - Milliseconds to wait.
 */
export function sleepMillis(delayMillis: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, delayMillis));
}

/**
 * POSTs a JSON body to `url` with retry-with-exponential-backoff on
 * failure (either a thrown network error, or a non-2xx HTTP response).
 *
 * Retry policy: up to `options.maxAttempts` total attempts; after each
 * failed attempt (except the last), waits
 * `initialBackoffMillis * backoffMultiplier ^ (attemptIndex - 1)`
 * milliseconds before retrying (classic exponential backoff). Never
 * throws -- failures after exhausting all attempts are reported via the
 * returned `WebhookDeliveryResult.delivered === false`, so callers (the
 * anomaly detector's event-handling path, the A/B evaluator's
 * verdict-handling path) are never able to have an outbound webhook
 * failure crash their synchronous detection/evaluation logic.
 *
 * @param url - Fully-qualified webhook URL to POST to.
 * @param jsonBody - Request body; JSON-serialized internally via `JSON.stringify`.
 * @param fetchImplementation - Injectable HTTP client; defaults to the real global `fetch`. Tests inject a fake.
 * @param retryOptions - Retry-with-backoff policy; defaults to `DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS`.
 * @param sleepImplementation - Injectable sleep function; defaults to `sleepMillis`. Tests inject a fake to avoid real delay, or use Jest fake timers with the real implementation.
 * @returns A `WebhookDeliveryResult` describing whether delivery ultimately succeeded.
 */
export async function postJsonWithRetryBackoff(
  url: string,
  jsonBody: unknown,
  fetchImplementation: WebhookFetchLike = resolveGlobalFetch(),
  retryOptions: WebhookRetryBackoffOptions = DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
  sleepImplementation: (delayMillis: number) => Promise<void> = sleepMillis,
): Promise<WebhookDeliveryResult> {
  const serializedBody = JSON.stringify(jsonBody);
  let finalStatus: number | null = null;
  let finalErrorMessage: string | null = null;

  for (let attemptIndex = 1; attemptIndex <= retryOptions.maxAttempts; attemptIndex += 1) {
    try {
      const response = await fetchImplementation(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: serializedBody,
      });
      finalStatus = response.status;
      if (response.ok) {
        return { delivered: true, attemptsMade: attemptIndex, finalStatus, finalErrorMessage: null };
      }
      finalErrorMessage = `Webhook POST to ${url} returned non-2xx status ${response.status}: ${await response.text()}`;
    } catch (error) {
      finalErrorMessage = `Webhook POST to ${url} threw: ${error instanceof Error ? error.message : String(error)}`;
    }

    const isLastAttempt = attemptIndex === retryOptions.maxAttempts;
    if (!isLastAttempt) {
      const backoffDelayMillis =
        retryOptions.initialBackoffMillis * retryOptions.backoffMultiplier ** (attemptIndex - 1);
      await sleepImplementation(backoffDelayMillis);
    }
  }

  return { delivered: false, attemptsMade: retryOptions.maxAttempts, finalStatus, finalErrorMessage };
}
