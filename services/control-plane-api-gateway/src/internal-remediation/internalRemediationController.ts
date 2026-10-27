/**
 * @file internalRemediationController.ts
 * REST (not GraphQL) controller receiving automated remediation actions from trusted
 * internal callers — currently, the `analytics-telemetry-platform` service's
 * experiment-health-check and cohort-degradation-detection pipelines. Plain REST
 * (rather than this gateway's usual GraphQL surface) is used deliberately here: this
 * is a service-to-service webhook receiver, not a client-facing API, and the caller's
 * outbound HTTP client is simpler to build against a fixed REST contract than a
 * GraphQL mutation.
 *
 * See SERVICE_README.md's "Internal remediation webhook contract" section for the
 * authoritative request/response shapes this must stay in sync with.
 *
 * Guard ordering: {@link InternalServiceAuthGuard} runs first (cheapest check, and the
 * one that actually authenticates the caller), then
 * {@link MutualTlsClientCertificateGuard} (a no-op unless explicitly enabled), then
 * {@link RedisBackedRateLimitingGuard} (protects the in-memory registry / downstream
 * Redis from an internal-caller bug that retries in a tight loop).
 */

import { Body, Controller, HttpCode, HttpStatus, Logger, Post, UseGuards } from '@nestjs/common';
import { RedisBackedRateLimitingGuard } from '../common/rate-limiting/redisBackedRateLimitingGuard';
import { MutualTlsClientCertificateGuard } from '../common/auth/mutualTlsClientCertificateGuard';
import { ManifestFilterRuleRequestBody } from './dto/manifestFilterRuleRequestBody';
import { RollbackExperimentRequestBody } from './dto/rollbackExperimentRequestBody';
import { RemediationActionReceivedEvent } from './events/remediationActionReceivedEvent';
import { InternalServiceAuthGuard } from './guards/internalServiceAuthGuard';
import { ActiveRemediationActionsRegistry } from './registry/activeRemediationActionsRegistry';
import { RemediationActionRecord } from './registry/remediationActionRecord.interface';

/** Response body echoed back to the calling internal service on successful receipt. */
export interface RemediationActionAcceptedResponseBody {
  /** Always `true` — presence of this field lets callers assert on shape without inspecting the HTTP status separately. */
  accepted: true;
  /** The registry-assigned identifier for the recorded action, useful for the caller's own audit trail / correlation. */
  remediationActionIdentifier: string;
  /** ISO-8601 timestamp after which this action will no longer be considered active by `activeRemediationActions`. */
  activeUntilIsoTimestamp: string;
}

@Controller('internal/remediation')
@UseGuards(InternalServiceAuthGuard, MutualTlsClientCertificateGuard, RedisBackedRateLimitingGuard)
export class InternalRemediationController {
  private readonly logger = new Logger(InternalRemediationController.name);

  public constructor(private readonly activeRemediationActionsRegistry: ActiveRemediationActionsRegistry) {}

  /**
   * Receives a rollback-experiment remediation action from the analytics-telemetry-platform
   * service's experiment health-check pipeline.
   * @param requestBody - Validated {@link RollbackExperimentRequestBody}.
   * @returns A {@link RemediationActionAcceptedResponseBody} confirming receipt.
   */
  @Post('rollback-experiment')
  @HttpCode(HttpStatus.ACCEPTED)
  public receiveRollbackExperimentRemediationAction(
    @Body() requestBody: RollbackExperimentRequestBody,
  ): RemediationActionAcceptedResponseBody {
    const record = this.activeRemediationActionsRegistry.recordRemediationAction({
      actionType: 'ROLLBACK_EXPERIMENT',
      reason: requestBody.reason,
      experimentId: requestBody.experimentId,
      verdict: requestBody.verdict,
    });
    this.logAndReturnAcceptedResponse(record);
    return this.buildAcceptedResponseBody(record);
  }

  /**
   * Receives a manifest-filter-rule remediation action from the
   * analytics-telemetry-platform service's device-cohort-degradation-detection
   * pipeline.
   * @param requestBody - Validated {@link ManifestFilterRuleRequestBody}.
   * @returns A {@link RemediationActionAcceptedResponseBody} confirming receipt.
   */
  @Post('manifest-filter-rule')
  @HttpCode(HttpStatus.ACCEPTED)
  public receiveManifestFilterRuleRemediationAction(
    @Body() requestBody: ManifestFilterRuleRequestBody,
  ): RemediationActionAcceptedResponseBody {
    const record = this.activeRemediationActionsRegistry.recordRemediationAction({
      actionType: 'MANIFEST_FILTER_RULE',
      reason: requestBody.reason,
      deviceCohortMatcher: requestBody.deviceCohortMatcher,
      filterAction: requestBody.action,
    });
    this.logAndReturnAcceptedResponse(record);
    return this.buildAcceptedResponseBody(record);
  }

  /**
   * Emits the structured {@link RemediationActionReceivedEvent} log line for a newly
   * recorded action.
   * @param record - The just-recorded {@link RemediationActionRecord}.
   */
  private logAndReturnAcceptedResponse(record: RemediationActionRecord): void {
    const event = new RemediationActionReceivedEvent(
      record.actionType,
      record.remediationActionIdentifier,
      record.reason,
      new Date(record.receivedAtEpochSeconds * 1000).toISOString(),
    );
    this.logger.log(event.toStructuredLogPayload());
  }

  /**
   * Builds the HTTP response body confirming receipt of a remediation action.
   * @param record - The just-recorded {@link RemediationActionRecord}.
   * @returns The {@link RemediationActionAcceptedResponseBody}.
   */
  private buildAcceptedResponseBody(record: RemediationActionRecord): RemediationActionAcceptedResponseBody {
    return {
      accepted: true,
      remediationActionIdentifier: record.remediationActionIdentifier,
      activeUntilIsoTimestamp: new Date(record.expiresAtEpochSeconds * 1000).toISOString(),
    };
  }
}
