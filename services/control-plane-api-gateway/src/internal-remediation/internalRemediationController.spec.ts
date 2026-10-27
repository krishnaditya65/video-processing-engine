/**
 * @file internalRemediationController.spec.ts
 * Unit tests for {@link InternalRemediationController}'s two endpoints, exercised
 * directly via a compiled Nest testing module (no live HTTP listener/supertest) per
 * project convention: records the action in {@link ActiveRemediationActionsRegistry},
 * returns the expected accepted-response shape, and emits a structured log event.
 */

import { Test, TestingModule } from '@nestjs/testing';
import { Logger } from '@nestjs/common';
import { ApplicationConfigurationService } from '../config/applicationConfigurationService';
import { MutualTlsClientCertificateGuard } from '../common/auth/mutualTlsClientCertificateGuard';
import { RedisBackedRateLimitingGuard } from '../common/rate-limiting/redisBackedRateLimitingGuard';
import { InternalRemediationController } from './internalRemediationController';
import { InternalServiceAuthGuard } from './guards/internalServiceAuthGuard';
import { ActiveRemediationActionsRegistry } from './registry/activeRemediationActionsRegistry';

/**
 * Guards applied via `@UseGuards(...)` at the controller level are resolved as
 * injectables when the Nest testing module compiles, even though these tests call
 * controller methods directly rather than going through a live HTTP request pipeline
 * (guard *execution* only happens on real requests). `overrideGuard` supplies a
 * trivial always-allow stand-in for each so the module can compile without needing to
 * also stand up Redis/mTLS/shared-secret configuration here — those guards get their
 * own dedicated unit tests (`internalServiceAuthGuard.spec.ts`,
 * `mutualTlsClientCertificateGuard.spec.ts`) and `RedisBackedRateLimitingGuard`
 * already has its own (`redisRateLimiterService.spec.ts`).
 */
const ALWAYS_ALLOW_GUARD = { canActivate: () => true };

describe('InternalRemediationController', () => {
  let testingModule: TestingModule;
  let controller: InternalRemediationController;
  let registry: ActiveRemediationActionsRegistry;

  beforeEach(async () => {
    testingModule = await Test.createTestingModule({
      controllers: [InternalRemediationController],
      providers: [ActiveRemediationActionsRegistry, ApplicationConfigurationService],
    })
      .overrideGuard(InternalServiceAuthGuard)
      .useValue(ALWAYS_ALLOW_GUARD)
      .overrideGuard(MutualTlsClientCertificateGuard)
      .useValue(ALWAYS_ALLOW_GUARD)
      .overrideGuard(RedisBackedRateLimitingGuard)
      .useValue(ALWAYS_ALLOW_GUARD)
      .compile();

    controller = testingModule.get(InternalRemediationController);
    registry = testingModule.get(ActiveRemediationActionsRegistry);
    jest.spyOn(Logger.prototype, 'log').mockImplementation(() => undefined);
  });

  afterEach(async () => {
    jest.restoreAllMocks();
    await testingModule.close();
  });

  describe('receiveRollbackExperimentRemediationAction', () => {
    it('records the action in the registry and returns an accepted response', () => {
      const response = controller.receiveRollbackExperimentRemediationAction({
        experimentId: 'exp-abc',
        reason: 'elevated rebuffer ratio detected',
        verdict: 'ELEVATED_REBUFFER_RATIO',
      });

      expect(response.accepted).toBe(true);
      expect(response.remediationActionIdentifier).toBeTruthy();
      expect(response.activeUntilIsoTimestamp).toBeTruthy();

      const activeActions = registry.listActiveRemediationActions();
      expect(activeActions).toHaveLength(1);
      expect(activeActions[0]).toMatchObject({
        actionType: 'ROLLBACK_EXPERIMENT',
        experimentId: 'exp-abc',
        verdict: 'ELEVATED_REBUFFER_RATIO',
        remediationActionIdentifier: response.remediationActionIdentifier,
      });
    });

    it('logs a structured RemediationActionReceivedEvent on receipt', () => {
      const logSpy = jest.spyOn(Logger.prototype, 'log');
      controller.receiveRollbackExperimentRemediationAction({
        experimentId: 'exp-log-test',
        reason: 'manual override',
        verdict: 'MANUAL_OPERATOR_OVERRIDE',
      });

      expect(logSpy).toHaveBeenCalledWith(
        expect.objectContaining({ event: 'RemediationActionReceivedEvent', actionType: 'ROLLBACK_EXPERIMENT' }),
      );
    });
  });

  describe('receiveManifestFilterRuleRemediationAction', () => {
    it('records the action in the registry and returns an accepted response', () => {
      const response = controller.receiveManifestFilterRuleRemediationAction({
        deviceCohortMatcher: 'platform:android;model:low-end-set-top-box',
        action: 'CAP_BITRATE_LADDER',
        reason: 'sustained rebuffering on this cohort',
      });

      expect(response.accepted).toBe(true);

      const activeActions = registry.listActiveRemediationActionsByType('MANIFEST_FILTER_RULE');
      expect(activeActions).toHaveLength(1);
      expect(activeActions[0]).toMatchObject({
        deviceCohortMatcher: 'platform:android;model:low-end-set-top-box',
        filterAction: 'CAP_BITRATE_LADDER',
      });
    });
  });
});
