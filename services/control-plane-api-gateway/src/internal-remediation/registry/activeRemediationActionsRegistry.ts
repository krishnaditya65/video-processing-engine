/**
 * @file activeRemediationActionsRegistry.ts
 * In-memory, process-local registry of recently-received remediation actions (see
 * {@link InternalRemediationController}), so other parts of the application — today,
 * the `activeRemediationActions` GraphQL query; in future, e.g. the manifest
 * resolution path actually applying `MANIFEST_FILTER_RULE` records — can read "what
 * remediations are currently in effect" without re-deriving it from logs.
 *
 * Deliberately in-memory (mirrors {@link InMemoryVideoAssetRepository}'s documented
 * trade-off): this resets on process restart and is not shared across horizontally
 * scaled instances. A production rollout serving this from more than one replica
 * should back this with Redis (this service already depends on `ioredis` for rate
 * limiting/cache-aside — a Redis-backed implementation of the same read/write
 * interface below is the natural upgrade path, following the Repository pattern used
 * for `VideoAssetRepositoryInterface`) or a shared datastore.
 *
 * TTL/expiry: each record carries its own `expiresAtEpochSeconds`
 * (`receivedAt + remediationActionActiveTimeToLiveInSeconds`, see
 * {@link ApplicationConfigurationService}); {@link listActiveRemediationActions} lazily
 * filters out expired records on every read (no background sweep timer, keeping this
 * dependency-free and trivially unit-testable with an injectable clock) — an
 * expired-but-not-yet-swept record still counts against nothing (it is simply hidden
 * from every read), so no separate cleanup pass is required for correctness.
 */

import { Injectable, Optional } from '@nestjs/common';
import { randomUUID } from 'crypto';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { RemediationActionRecord, RemediationActionType } from './remediationActionRecord.interface';

/** Fields callers supply when recording a new remediation action; identifier/timestamps are assigned by the registry. */
export type RemediationActionRecordInput = Omit<
  RemediationActionRecord,
  'remediationActionIdentifier' | 'receivedAtEpochSeconds' | 'expiresAtEpochSeconds'
>;

@Injectable()
export class ActiveRemediationActionsRegistry {
  /** All records ever recorded this process lifetime, keyed by identifier; expired entries are filtered at read time rather than eagerly evicted. */
  private readonly recordsByIdentifier = new Map<string, RemediationActionRecord>();

  /**
   * @param applicationConfigurationService - Supplies the active-TTL window.
   * @param currentTimeProviderFunction - Injectable clock (defaults to `Date.now`), overridable in unit tests for deterministic TTL-expiry assertions.
   */
  public constructor(
    private readonly applicationConfigurationService: ApplicationConfigurationService,
    @Optional() private readonly currentTimeProviderFunction: () => number = () => Date.now(),
  ) {}

  /**
   * Records a newly-received remediation action, assigning it a unique identifier and
   * an expiry timestamp derived from the configured TTL.
   * @param recordInput - The action's type-specific fields (see {@link RemediationActionRecordInput}).
   * @returns The fully-populated {@link RemediationActionRecord} that was stored.
   */
  public recordRemediationAction(recordInput: RemediationActionRecordInput): RemediationActionRecord {
    const { remediationActionActiveTimeToLiveInSeconds } = this.applicationConfigurationService.getRuntimeConfiguration();
    const nowInEpochSeconds = Math.floor(this.currentTimeProviderFunction() / 1000);

    const record: RemediationActionRecord = {
      ...recordInput,
      remediationActionIdentifier: randomUUID(),
      receivedAtEpochSeconds: nowInEpochSeconds,
      expiresAtEpochSeconds: nowInEpochSeconds + remediationActionActiveTimeToLiveInSeconds,
    };
    this.recordsByIdentifier.set(record.remediationActionIdentifier, record);
    return record;
  }

  /**
   * Lists every currently-active (non-expired) remediation action, most recently
   * received first.
   * @returns Active {@link RemediationActionRecord} entries.
   */
  public listActiveRemediationActions(): RemediationActionRecord[] {
    const nowInEpochSeconds = Math.floor(this.currentTimeProviderFunction() / 1000);
    return Array.from(this.recordsByIdentifier.values())
      .filter((record) => record.expiresAtEpochSeconds > nowInEpochSeconds)
      .sort((first, second) => second.receivedAtEpochSeconds - first.receivedAtEpochSeconds);
  }

  /**
   * Lists only active records of a given type — used e.g. to answer "which experiment
   * rollbacks are currently in effect".
   * @param actionType - The {@link RemediationActionType} to filter by.
   * @returns Active {@link RemediationActionRecord} entries of the requested type.
   */
  public listActiveRemediationActionsByType(actionType: RemediationActionType): RemediationActionRecord[] {
    return this.listActiveRemediationActions().filter((record) => record.actionType === actionType);
  }
}
