/**
 * @file rollbackExperimentRequestBody.ts
 * Request body contract for `POST /internal/remediation/rollback-experiment`.
 *
 * This shape is the receiving side of a contract owned jointly with the
 * `analytics-telemetry-platform` service, which calls this endpoint when its
 * automated experiment health-check pipeline determines an in-flight
 * experiment/feature-flag is degrading playback quality (e.g. elevated rebuffer
 * ratio, DRM negotiation failures, or a negative guardrail-metric breach in a
 * treatment cohort) and should be rolled back. Keep this DTO's field names and types
 * in lockstep with that service's outbound caller — see SERVICE_README.md's
 * "Internal remediation webhook contract" section for the authoritative shape.
 */

import { IsIn, IsNotEmpty, IsString, MaxLength } from 'class-validator';

/**
 * Verdicts the analytics-telemetry-platform's experiment health-check pipeline may
 * report. `OTHER` is accepted as a forward-compatible escape hatch so this service
 * doesn't reject a request outright just because the upstream service introduces a
 * new verdict value before this DTO is updated to know about it.
 */
export const KNOWN_EXPERIMENT_ROLLBACK_VERDICTS = [
  'NEGATIVE_GUARDRAIL_BREACH',
  'ELEVATED_REBUFFER_RATIO',
  'DRM_NEGOTIATION_FAILURE_SPIKE',
  'MANUAL_OPERATOR_OVERRIDE',
  'OTHER',
] as const;

/** Request body for `POST /internal/remediation/rollback-experiment`. */
export class RollbackExperimentRequestBody {
  /** Identifier of the experiment/feature-flag to roll back, as known to the experimentation platform. */
  @IsString()
  @IsNotEmpty()
  @MaxLength(200)
  experimentId!: string;

  /** Free-text, human-readable explanation for the rollback (surfaced in logs/audit trail). */
  @IsString()
  @IsNotEmpty()
  @MaxLength(2000)
  reason!: string;

  /** Machine-readable classification of why the rollback was triggered. */
  @IsString()
  @IsIn(KNOWN_EXPERIMENT_ROLLBACK_VERDICTS)
  verdict!: (typeof KNOWN_EXPERIMENT_ROLLBACK_VERDICTS)[number];
}
