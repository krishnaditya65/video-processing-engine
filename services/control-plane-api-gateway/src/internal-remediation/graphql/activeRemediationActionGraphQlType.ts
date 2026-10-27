/**
 * @file activeRemediationActionGraphQlType.ts
 * Code-first GraphQL object type exposing {@link RemediationActionRecord} entries via
 * the `activeRemediationActions` query, following the same
 * entity-to-GraphQL-type mapping-function convention as
 * `mapVideoAssetEntityToGraphQlType`.
 */

import { Field, GraphQLISODateTime, ID, ObjectType } from '@nestjs/graphql';
import { RemediationActionRecord, RemediationActionType } from '../registry/remediationActionRecord.interface';

/** GraphQL object type for a single currently-active remediation action. */
@ObjectType('ActiveRemediationAction', {
  description: 'A remediation action received from an internal caller (e.g. analytics-telemetry-platform) that is still within its active TTL window.',
})
export class ActiveRemediationActionGraphQlType {
  @Field(() => ID)
  remediationActionIdentifier!: string;

  @Field(() => String, { description: 'One of ROLLBACK_EXPERIMENT | MANIFEST_FILTER_RULE.' })
  actionType!: RemediationActionType;

  @Field(() => String)
  reason!: string;

  @Field(() => GraphQLISODateTime)
  receivedAtIsoTimestamp!: string;

  @Field(() => GraphQLISODateTime)
  expiresAtIsoTimestamp!: string;

  @Field(() => String, { nullable: true, description: 'Populated when actionType is ROLLBACK_EXPERIMENT.' })
  experimentId?: string;

  @Field(() => String, { nullable: true, description: 'Populated when actionType is ROLLBACK_EXPERIMENT.' })
  verdict?: string;

  @Field(() => String, { nullable: true, description: 'Populated when actionType is MANIFEST_FILTER_RULE.' })
  deviceCohortMatcher?: string;

  @Field(() => String, { nullable: true, description: 'Populated when actionType is MANIFEST_FILTER_RULE.' })
  filterAction?: string;
}

/**
 * Factory function mapping an internal {@link RemediationActionRecord} (epoch-second
 * timestamps) to its GraphQL-facing {@link ActiveRemediationActionGraphQlType}
 * representation (ISO-8601 timestamps).
 * @param record - The internal registry record.
 * @returns The equivalent GraphQL object type instance.
 */
export function mapRemediationActionRecordToGraphQlType(record: RemediationActionRecord): ActiveRemediationActionGraphQlType {
  const graphQlType = new ActiveRemediationActionGraphQlType();
  graphQlType.remediationActionIdentifier = record.remediationActionIdentifier;
  graphQlType.actionType = record.actionType;
  graphQlType.reason = record.reason;
  graphQlType.receivedAtIsoTimestamp = new Date(record.receivedAtEpochSeconds * 1000).toISOString();
  graphQlType.expiresAtIsoTimestamp = new Date(record.expiresAtEpochSeconds * 1000).toISOString();
  graphQlType.experimentId = record.experimentId;
  graphQlType.verdict = record.verdict;
  graphQlType.deviceCohortMatcher = record.deviceCohortMatcher;
  graphQlType.filterAction = record.filterAction;
  return graphQlType;
}
