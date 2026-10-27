/**
 * @file activeRemediationActionsRegistry.spec.ts
 * Unit tests for {@link ActiveRemediationActionsRegistry}: recording + listing active
 * actions, per-type filtering, and TTL-based expiry using an injected fake clock (no
 * real timers/delays).
 */

import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { ActiveRemediationActionsRegistry } from './activeRemediationActionsRegistry';

describe('ActiveRemediationActionsRegistry', () => {
  let fakeCurrentTimeInMilliseconds: number;
  let registry: ActiveRemediationActionsRegistry;

  beforeEach(() => {
    process.env.REMEDIATION_ACTION_ACTIVE_TTL_SECONDS = '60';
    fakeCurrentTimeInMilliseconds = 0;
    registry = new ActiveRemediationActionsRegistry(
      new ApplicationConfigurationService(),
      () => fakeCurrentTimeInMilliseconds,
    );
  });

  afterEach(() => {
    delete process.env.REMEDIATION_ACTION_ACTIVE_TTL_SECONDS;
  });

  it('starts with no active remediation actions', () => {
    expect(registry.listActiveRemediationActions()).toEqual([]);
  });

  it('records a rollback-experiment action and returns it as active', () => {
    const record = registry.recordRemediationAction({
      actionType: 'ROLLBACK_EXPERIMENT',
      reason: 'guardrail breach',
      experimentId: 'exp-123',
      verdict: 'NEGATIVE_GUARDRAIL_BREACH',
    });

    expect(record.remediationActionIdentifier).toBeTruthy();
    expect(record.expiresAtEpochSeconds).toBe(60);

    const activeActions = registry.listActiveRemediationActions();
    expect(activeActions).toHaveLength(1);
    expect(activeActions[0]).toMatchObject({ experimentId: 'exp-123', actionType: 'ROLLBACK_EXPERIMENT' });
  });

  it('records a manifest-filter-rule action independently', () => {
    registry.recordRemediationAction({
      actionType: 'MANIFEST_FILTER_RULE',
      reason: 'cohort degradation',
      deviceCohortMatcher: 'platform:ios;appVersion:<4.2.0',
      filterAction: 'CAP_BITRATE_LADDER',
    });

    const active = registry.listActiveRemediationActionsByType('MANIFEST_FILTER_RULE');
    expect(active).toHaveLength(1);
    expect(active[0].deviceCohortMatcher).toBe('platform:ios;appVersion:<4.2.0');
    expect(registry.listActiveRemediationActionsByType('ROLLBACK_EXPERIMENT')).toEqual([]);
  });

  it('assigns distinct identifiers to separately recorded actions', () => {
    const first = registry.recordRemediationAction({ actionType: 'ROLLBACK_EXPERIMENT', reason: 'a', experimentId: 'e1' });
    const second = registry.recordRemediationAction({ actionType: 'ROLLBACK_EXPERIMENT', reason: 'b', experimentId: 'e2' });
    expect(first.remediationActionIdentifier).not.toBe(second.remediationActionIdentifier);
  });

  it('orders active actions most-recently-received first', () => {
    registry.recordRemediationAction({ actionType: 'ROLLBACK_EXPERIMENT', reason: 'first', experimentId: 'e1' });
    fakeCurrentTimeInMilliseconds += 5_000;
    registry.recordRemediationAction({ actionType: 'ROLLBACK_EXPERIMENT', reason: 'second', experimentId: 'e2' });

    const active = registry.listActiveRemediationActions();
    expect(active[0].experimentId).toBe('e2');
    expect(active[1].experimentId).toBe('e1');
  });

  it('excludes a record once its TTL has elapsed', () => {
    registry.recordRemediationAction({ actionType: 'ROLLBACK_EXPERIMENT', reason: 'expiring soon', experimentId: 'e1' });
    expect(registry.listActiveRemediationActions()).toHaveLength(1);

    fakeCurrentTimeInMilliseconds += 60_001; // just past the 60s TTL

    expect(registry.listActiveRemediationActions()).toEqual([]);
    expect(registry.listActiveRemediationActionsByType('ROLLBACK_EXPERIMENT')).toEqual([]);
  });
});
