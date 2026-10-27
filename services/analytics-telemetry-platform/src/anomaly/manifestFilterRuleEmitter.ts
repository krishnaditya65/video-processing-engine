/**
 * @file Anomaly auto-remediation: emits `ManifestFilterRule`s for the
 * edge-packaging service in response to confirmed hardware-decoder
 * anomalies.
 *
 * Extends the Observer-pattern chain already documented in
 * `anomalyEventEmitter.ts`: `FirmwareCohortAnomalyDetector` emits an
 * `AnomalyDetectedEvent` through `AnomalyEventEmitter`; this module adds a
 * second observer, `ManifestFilterRuleEmitter`, which reacts to that same
 * event by constructing a structured `ManifestFilterRule` (e.g. "LG TV
 * firmware 4.20.1 cannot decode HEVC reliably -- downgrade its manifest to
 * H.264") and POSTing it to the edge-packaging service's manifest-filter
 * webhook, so affected devices stop being served the broken encoding
 * ladder within one detection cycle, with no human in the loop.
 *
 * Webhook contract (see README "Outbound Webhook Contracts" for the
 * canonical copy):
 *
 *   POST {edgePackagingBaseUrl}/internal/manifest-filters/rules
 *   Content-Type: application/json
 *
 *   {
 *     "deviceCohortMatcher": { "firmwareVersion": string },
 *     "action": "downgradeToH264",
 *     "reason": string,
 *     "sourceAnomaly": {
 *       "firmwareCohortId": string,
 *       "zScore": number,
 *       "observedErrorRate": number,
 *       "baselineMeanErrorRate": number
 *     },
 *     "createdAtEpochMillis": number
 *   }
 */
import type { AnomalyEventEmitter, AnomalyDetectedEvent } from './anomalyEventEmitter';
import {
  postJsonWithRetryBackoff,
  DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS,
  type WebhookFetchLike,
  type WebhookRetryBackoffOptions,
  type WebhookDeliveryResult,
} from '../webhooks/webhookRetryHttpClient';

/**
 * Matches the device cohort a `ManifestFilterRule` applies to. Only
 * firmware-version matching is implemented today (mirroring the
 * `firmwareCohortId` dimension the anomaly detector operates on); documented
 * as extensible to other dimensions (device model, OS version) if future
 * anomaly detectors key on them.
 */
export interface DeviceCohortMatcher {
  /** Exact device firmware version string this rule applies to (matches `AnomalyDetectedEvent.firmwareCohortId`). */
  firmwareVersion: string;
}

/**
 * Remediation actions the edge-packaging service knows how to apply to a
 * matched device cohort's manifest. Only `downgradeToH264` is implemented
 * today (the concrete remediation for "this cohort's hardware decoder
 * cannot reliably decode HEVC"); the union is left open for future actions
 * (e.g. `capMaxBitrate`, `disableCohort`) without a breaking contract
 * change on the receiving side.
 */
export type ManifestFilterRuleAction = 'downgradeToH264';

/** A structured manifest-filtering rule, intended for the edge-packaging service to apply at manifest-generation time. */
export interface ManifestFilterRule {
  /** Which device cohort this rule applies to. */
  deviceCohortMatcher: DeviceCohortMatcher;
  /** The remediation action to apply for matching devices. */
  action: ManifestFilterRuleAction;
  /** Human-readable justification, for the edge-packaging service's audit log / ops dashboard. */
  reason: string;
  /** The anomaly detection data that triggered this rule, for traceability. */
  sourceAnomaly: {
    /** Firmware cohort identifier the anomaly was detected for. */
    firmwareCohortId: string;
    /** Z-score of the observation that triggered the anomaly. */
    zScore: number;
    /** Observed decode-error rate for the cohort. */
    observedErrorRate: number;
    /** Rolling baseline mean error rate the observation was compared against. */
    baselineMeanErrorRate: number;
  };
  /** Epoch-milliseconds timestamp the rule was constructed at. */
  createdAtEpochMillis: number;
}

/** Path appended to `edgePackagingBaseUrl` for the manifest-filter-rule webhook. */
export const MANIFEST_FILTER_RULE_WEBHOOK_PATH = '/internal/manifest-filters/rules';

/** Configuration for `ManifestFilterRuleEmitter`. */
export interface ManifestFilterRuleEmitterOptions {
  /** Base URL of the edge-packaging service (e.g. `https://edge-packaging.internal:8443`); the rules path is appended by this emitter. */
  edgePackagingBaseUrl: string;
  /** Injectable HTTP client; defaults to the real global `fetch`. Tests inject a fake. */
  fetchImplementation?: WebhookFetchLike;
  /** Retry-with-backoff policy for webhook delivery; defaults to `DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS`. */
  retryBackoffOptions?: WebhookRetryBackoffOptions;
  /** Injectable sleep function for retry backoff delays; defaults to real `setTimeout`-backed sleep. */
  sleepImplementation?: (delayMillis: number) => Promise<void>;
  /** Injectable clock; defaults to `Date.now`. */
  clock?: () => number;
}

/**
 * Constructs a `ManifestFilterRule` from a confirmed `AnomalyDetectedEvent`.
 *
 * Pure function, deliberately separated from the webhook-delivery side
 * effect below so the rule-construction logic (the interesting business
 * decision: "this anomaly implies THIS specific remediation action") can be
 * unit-tested without any HTTP mocking at all.
 *
 * Today's fixed mapping -- any confirmed firmware-cohort decode-error
 * anomaly maps to `downgradeToH264` -- reflects the concrete motivating
 * scenario (LG TV firmware that regressed on HEVC hardware decode); see
 * README "Limitations" for the documented extension point (a pluggable
 * anomaly-to-action mapping strategy) if/when other anomaly types need a
 * different remediation.
 *
 * @param event - The confirmed anomaly event to build a rule for.
 * @param clock - Injectable clock; defaults to `Date.now`.
 * @returns The structured `ManifestFilterRule`.
 */
export function buildManifestFilterRuleFromAnomalyEvent(
  event: AnomalyDetectedEvent,
  clock: () => number = Date.now,
): ManifestFilterRule {
  return {
    deviceCohortMatcher: { firmwareVersion: event.firmwareCohortId },
    action: 'downgradeToH264',
    reason:
      `Firmware cohort ${event.firmwareCohortId} decode-error rate ` +
      `(${event.observedErrorRate}) is ${event.zScore.toFixed(2)} standard deviations ` +
      `above its rolling baseline mean (${event.baselineMeanErrorRate}), crossing the ` +
      `${event.thresholdCrossed}-sigma anomaly threshold; downgrading this cohort's manifest ` +
      `to H.264 to avoid HEVC hardware-decode failures.`,
    sourceAnomaly: {
      firmwareCohortId: event.firmwareCohortId,
      zScore: event.zScore,
      observedErrorRate: event.observedErrorRate,
      baselineMeanErrorRate: event.baselineMeanErrorRate,
    },
    createdAtEpochMillis: clock(),
  };
}

/**
 * Observer that subscribes to `AnomalyEventEmitter`'s `anomalyDetected`
 * channel and, for every confirmed anomaly, constructs a `ManifestFilterRule`
 * and POSTs it (with retry-with-backoff) to the edge-packaging service's
 * manifest-filter-rules webhook -- the anomaly-detection analogue of
 * `EncodingExperimentAutoRemediationService` for the A/B-rollback path.
 */
export class ManifestFilterRuleEmitter {
  private readonly edgePackagingBaseUrl: string;
  private readonly fetchImplementation: WebhookFetchLike | undefined;
  private readonly retryBackoffOptions: WebhookRetryBackoffOptions;
  private readonly sleepImplementation: ((delayMillis: number) => Promise<void>) | undefined;
  private readonly clock: () => number;

  /**
   * @param options - Emitter configuration; see `ManifestFilterRuleEmitterOptions`.
   */
  public constructor(options: ManifestFilterRuleEmitterOptions) {
    this.edgePackagingBaseUrl = options.edgePackagingBaseUrl.replace(/\/+$/, '');
    this.fetchImplementation = options.fetchImplementation;
    this.retryBackoffOptions = options.retryBackoffOptions ?? DEFAULT_WEBHOOK_RETRY_BACKOFF_OPTIONS;
    this.sleepImplementation = options.sleepImplementation;
    this.clock = options.clock ?? Date.now;
  }

  /**
   * Subscribes this emitter to an `AnomalyEventEmitter`'s
   * `anomalyDetected` channel (Observer pattern registration). Each
   * emitted event is handled asynchronously and independently; a webhook
   * delivery failure for one anomaly event never affects handling of
   * subsequent events.
   *
   * @param anomalyEventEmitter - The emitter to subscribe to.
   */
  public subscribeToAnomalyEvents(anomalyEventEmitter: AnomalyEventEmitter): void {
    anomalyEventEmitter.onAnomalyDetected((event) => {
      void this.handleAnomalyDetectedEvent(event);
    });
  }

  /**
   * Handles a single `AnomalyDetectedEvent`: builds the corresponding
   * `ManifestFilterRule` and delivers it to the edge-packaging webhook.
   * Never throws -- delivery failures are reported via the returned
   * `WebhookDeliveryResult`, matching
   * `EncodingExperimentAutoRemediationService`'s failure-handling contract.
   *
   * @param event - The confirmed anomaly event.
   * @returns The constructed rule and the webhook delivery outcome.
   */
  public async handleAnomalyDetectedEvent(
    event: AnomalyDetectedEvent,
  ): Promise<{ rule: ManifestFilterRule; deliveryResult: WebhookDeliveryResult }> {
    const rule = buildManifestFilterRuleFromAnomalyEvent(event, this.clock);
    const url = `${this.edgePackagingBaseUrl}${MANIFEST_FILTER_RULE_WEBHOOK_PATH}`;

    const deliveryResult = await postJsonWithRetryBackoff(
      url,
      rule,
      this.fetchImplementation,
      this.retryBackoffOptions,
      this.sleepImplementation,
    );

    return { rule, deliveryResult };
  }
}
