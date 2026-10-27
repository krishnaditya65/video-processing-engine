/**
 * @file Live A/B rollback automation.
 *
 * `evaluateEncodingExperimentStatisticalSignificance`
 * (`encodingExperimentEvaluator.ts`) is a pure decision function -- it
 * decides `ROLLBACK`/`PROMOTE`/`INCONCLUSIVE` but takes no action.
 * `EncodingExperimentAutoRemediationService` is the piece that closes the
 * loop: given an `EncodingExperimentEvaluationResult`, when the decision is
 * `ROLLBACK` it calls the control-plane-api-gateway's remediation webhook
 * (`POST {controlPlaneBaseUrl}/internal/remediation/rollback-experiment`) so
 * the encoding experiment can be reverted automatically, without a human in
 * the loop, within seconds of the statistical significance threshold being
 * crossed.
 *
 * This mirrors the Observer pattern already used for anomaly detection
 * (`AnomalyEventEmitter`): rather than the evaluator calling this service
 * directly, callers explicitly route each verdict into
 * `handleEncodingExperimentVerdict` (typically from a scheduled job or a
 * stream-processing loop that periodically re-evaluates running
 * experiments) -- decoupling "decide" from "act", the same separation of
 * concerns documented in README "Design Patterns" for anomaly detection.
 *
 * Webhook contract (see README "Outbound Webhook Contracts" for the
 * canonical copy consumed by the control-plane-api-gateway team):
 *
 *   POST {controlPlaneBaseUrl}/internal/remediation/rollback-experiment
 *   Content-Type: application/json
 *
 *   {
 *     "experimentId": string,
 *     "decision": "ROLLBACK",
 *     "controlArmMeanRebufferingRatio": number,
 *     "treatmentArmMeanRebufferingRatio": number,
 *     "pValue": number,
 *     "significanceLevelAlpha": number,
 *     "rationale": string,
 *     "detectedAtEpochMillis": number
 *   }
 *
 * The endpoint itself is being implemented by the control-plane-api-gateway
 * team against this same contract; this service only needs to call it, and
 * degrades gracefully (logs + reports non-delivery, never throws) if it is
 * unreachable.
 */
import type { ExperimentArmAggregate } from './significanceTests';
import type { EncodingExperimentEvaluationResult } from './encodingExperimentEvaluator';
import {
  postJsonWithRetryBackoff,
  DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
  type WebhookFetchLike,
  type WebhookRetryBackoffOptions,
  type WebhookDeliveryResult,
} from '../webhooks/webhookRetryHttpClient';

/** JSON request body POSTed to the control-plane-api-gateway's rollback-experiment webhook. See file header for the full contract. */
export interface EncodingExperimentRollbackWebhookRequestBody {
  /** Identifier of the encoding experiment being rolled back. */
  experimentId: string;
  /** Always `'ROLLBACK'` -- this webhook is only ever called for rollback verdicts. */
  decision: 'ROLLBACK';
  /** Control arm's mean rebuffering ratio at the time of the verdict. */
  controlArmMeanRebufferingRatio: number;
  /** Treatment arm's mean rebuffering ratio at the time of the verdict. */
  treatmentArmMeanRebufferingRatio: number;
  /** Two-tailed p-value that triggered the rollback decision. */
  pValue: number;
  /** Significance threshold (alpha) the decision was evaluated against. */
  significanceLevelAlpha: number;
  /** Human-readable rationale, propagated from `EncodingExperimentEvaluationResult.rationale`, for audit trails on the receiving side. */
  rationale: string;
  /** Epoch-milliseconds timestamp at which the rollback verdict was produced. */
  detectedAtEpochMillis: number;
}

/** Configuration for `EncodingExperimentAutoRemediationService`. */
export interface EncodingExperimentAutoRemediationServiceOptions {
  /** Base URL of the control-plane-api-gateway (e.g. `https://control-plane.internal:8443`); the remediation path is appended by this service. */
  controlPlaneBaseUrl: string;
  /** Injectable HTTP client; defaults to the real global `fetch`. Tests inject a fake. */
  fetchImplementation?: WebhookFetchLike;
  /** Retry-with-backoff policy for webhook delivery; defaults to `DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS`. */
  retryBackoffOptions?: WebhookRetryBackoffOptions;
  /** Injectable sleep function for retry backoff delays; defaults to real `setTimeout`-backed sleep. */
  sleepImplementation?: (delayMillis: number) => Promise<void>;
  /** Injectable clock; defaults to `Date.now`. */
  clock?: () => number;
}

/** Path appended to `controlPlaneBaseUrl` for the rollback-experiment webhook. Exported so tests and README stay in sync with the real value. */
export const ROLLBACK_EXPERIMENT_WEBHOOK_PATH = '/internal/remediation/rollback-experiment';

/**
 * Subscribes to encoding-experiment evaluation verdicts and, for `ROLLBACK`
 * verdicts, calls the control-plane-api-gateway's remediation webhook with
 * retry-with-backoff, closing the loop from "statistically significant
 * regression detected" to "experiment automatically reverted" without
 * human intervention.
 */
export class EncodingExperimentAutoRemediationService {
  private readonly controlPlaneBaseUrl: string;
  private readonly fetchImplementation: WebhookFetchLike | undefined;
  private readonly retryBackoffOptions: WebhookRetryBackoffOptions;
  private readonly sleepImplementation: ((delayMillis: number) => Promise<void>) | undefined;
  private readonly clock: () => number;

  /**
   * @param options - Service configuration; see `EncodingExperimentAutoRemediationServiceOptions`.
   */
  public constructor(options: EncodingExperimentAutoRemediationServiceOptions) {
    this.controlPlaneBaseUrl = options.controlPlaneBaseUrl.replace(/\/+$/, '');
    this.fetchImplementation = options.fetchImplementation;
    this.retryBackoffOptions = options.retryBackoffOptions ?? DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS;
    this.sleepImplementation = options.sleepImplementation;
    this.clock = options.clock ?? Date.now;
  }

  /**
   * Handles a single encoding-experiment evaluation verdict. No-ops
   * (returns `null`) for `PROMOTE`/`INCONCLUSIVE` decisions -- only
   * `ROLLBACK` triggers the outbound webhook. Never throws: webhook
   * delivery failures are reported in the returned `WebhookDeliveryResult`,
   * not raised, so a flaky control-plane endpoint cannot crash the caller's
   * evaluation loop.
   *
   * @param experimentId - Identifier of the encoding experiment that was evaluated.
   * @param controlArm - The control arm aggregate used to produce the verdict (for the webhook body).
   * @param treatmentArm - The treatment arm aggregate used to produce the verdict (for the webhook body).
   * @param evaluationResult - The evaluator's output for this experiment.
   * @returns The webhook `WebhookDeliveryResult` if a rollback webhook was sent, or `null` if the verdict did not require action.
   */
  public async handleEncodingExperimentVerdict(
    experimentId: string,
    controlArm: ExperimentArmAggregate,
    treatmentArm: ExperimentArmAggregate,
    evaluationResult: EncodingExperimentEvaluationResult,
  ): Promise<WebhookDeliveryResult | null> {
    if (evaluationResult.decision !== 'ROLLBACK') {
      return null;
    }

    const requestBody: EncodingExperimentRollbackWebhookRequestBody = {
      experimentId,
      decision: 'ROLLBACK',
      controlArmMeanRebufferingRatio: controlArm.meanValue,
      treatmentArmMeanRebufferingRatio: treatmentArm.meanValue,
      pValue: evaluationResult.pValue,
      significanceLevelAlpha: evaluationResult.significanceLevelAlpha,
      rationale: evaluationResult.rationale,
      detectedAtEpochMillis: this.clock(),
    };

    const url = `${this.controlPlaneBaseUrl}${ROLLBACK_EXPERIMENT_WEBHOOK_PATH}`;

    // Passing `undefined` explicitly for an omitted injected dependency is
    // intentional: JavaScript default-parameter substitution applies
    // whenever the argument is `undefined`, so `postJsonWithRetryBackoff`
    // falls back to the real global `fetch` / real `sleepMillis` exactly as
    // if the argument had been omitted entirely.
    return postJsonWithRetryBackoff(
      url,
      requestBody,
      this.fetchImplementation,
      this.retryBackoffOptions,
      this.sleepImplementation,
    );
  }
}
