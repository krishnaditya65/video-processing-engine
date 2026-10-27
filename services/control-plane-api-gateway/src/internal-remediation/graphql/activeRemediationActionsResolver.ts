/**
 * @file activeRemediationActionsResolver.ts
 * GraphQL resolver exposing the {@link ActiveRemediationActionsRegistry} contents to
 * client applications / internal dashboards via the `activeRemediationActions` query,
 * so operators/tooling can see what remediations the platform has automatically
 * applied without needing direct access to this process's logs.
 */

import { UseGuards } from '@nestjs/common';
import { Query, Resolver } from '@nestjs/graphql';
import { RedisBackedRateLimitingGuard } from '../../common/rate-limiting/redisBackedRateLimitingGuard';
import { ActiveRemediationActionsRegistry } from '../registry/activeRemediationActionsRegistry';
import {
  ActiveRemediationActionGraphQlType,
  mapRemediationActionRecordToGraphQlType,
} from './activeRemediationActionGraphQlType';

/** Resolver for the read-only `activeRemediationActions` query. Rate-limited like every other resolver in this gateway. */
@Resolver(() => ActiveRemediationActionGraphQlType)
@UseGuards(RedisBackedRateLimitingGuard)
export class ActiveRemediationActionsResolver {
  public constructor(private readonly activeRemediationActionsRegistry: ActiveRemediationActionsRegistry) {}

  /**
   * Lists every remediation action currently considered active (received within its
   * TTL window), most recently received first.
   * @returns Active actions as {@link ActiveRemediationActionGraphQlType}.
   */
  @Query(() => [ActiveRemediationActionGraphQlType], {
    name: 'activeRemediationActions',
    description:
      'Lists remediation actions (experiment rollbacks, manifest filter rules) received from internal callers ' +
      '(e.g. analytics-telemetry-platform) that are still within their active TTL window.',
  })
  public activeRemediationActions(): ActiveRemediationActionGraphQlType[] {
    return this.activeRemediationActionsRegistry.listActiveRemediationActions().map(mapRemediationActionRecordToGraphQlType);
  }
}
