/**
 * @file remediationActionRecord.interface.ts
 * Shape of a single entry tracked by {@link ActiveRemediationActionsRegistry}.
 */

/** Discriminates which internal remediation webhook produced a given record. */
export type RemediationActionType = 'ROLLBACK_EXPERIMENT' | 'MANIFEST_FILTER_RULE';

/**
 * A single remediation action currently considered "active" (received within the
 * configured TTL window — see
 * {@link ApplicationRuntimeConfigurationShape.remediationActionActiveTimeToLiveInSeconds}).
 * Deliberately flat (rather than a discriminated union per action type) so it maps
 * directly onto a single GraphQL object type ({@link ActiveRemediationActionGraphQlType})
 * without a union type in the schema; fields not relevant to a given `actionType` are
 * simply left `undefined`.
 */
export interface RemediationActionRecord {
  /** Registry-assigned unique identifier for this record. */
  remediationActionIdentifier: string;
  /** Which remediation webhook produced this record. */
  actionType: RemediationActionType;
  /** Caller-supplied human-readable reason. */
  reason: string;
  /** Wall-clock time this record was received, in Unix epoch seconds. */
  receivedAtEpochSeconds: number;
  /** Wall-clock time after which this record is considered expired and excluded from active listings, in Unix epoch seconds. */
  expiresAtEpochSeconds: number;
  /** Present when `actionType === 'ROLLBACK_EXPERIMENT'`: the experiment identifier rolled back. */
  experimentId?: string;
  /** Present when `actionType === 'ROLLBACK_EXPERIMENT'`: the reported verdict that triggered the rollback. */
  verdict?: string;
  /** Present when `actionType === 'MANIFEST_FILTER_RULE'`: the matched device cohort. */
  deviceCohortMatcher?: string;
  /** Present when `actionType === 'MANIFEST_FILTER_RULE'`: the applied mitigating action. */
  filterAction?: string;
}
