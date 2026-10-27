/**
 * @file authenticationStrategyFactory.spec.ts
 * Unit tests for {@link AuthenticationStrategyFactory}: selects
 * {@link DevJwtAuthenticationStrategy} by default and when explicitly configured, and
 * selects {@link OidcAuthenticationStrategy} when `AUTHENTICATION_STRATEGY_MODE=oidc`.
 * No network access to an IdP is required for any of these.
 */

import { JwtService } from '@nestjs/jwt';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { AuthenticationStrategyFactory } from './authenticationStrategyFactory';
import { DevJwtAuthenticationStrategy } from './devJwtAuthenticationStrategy';
import { OidcAuthenticationStrategy, UnimplementedOidcRelyingPartyClient } from './oidcAuthenticationStrategy';

describe('AuthenticationStrategyFactory', () => {
  const originalAuthenticationStrategyModeEnvironmentVariable = process.env.AUTHENTICATION_STRATEGY_MODE;

  afterEach(() => {
    if (originalAuthenticationStrategyModeEnvironmentVariable === undefined) {
      delete process.env.AUTHENTICATION_STRATEGY_MODE;
    } else {
      process.env.AUTHENTICATION_STRATEGY_MODE = originalAuthenticationStrategyModeEnvironmentVariable;
    }
  });

  function buildFactory(): AuthenticationStrategyFactory {
    const applicationConfigurationService = new ApplicationConfigurationService();
    const devJwtAuthenticationStrategy = new DevJwtAuthenticationStrategy(new JwtService({ secret: 'test-secret' }));
    const oidcAuthenticationStrategy = new OidcAuthenticationStrategy(
      new UnimplementedOidcRelyingPartyClient(),
      applicationConfigurationService,
    );
    return new AuthenticationStrategyFactory(applicationConfigurationService, devJwtAuthenticationStrategy, oidcAuthenticationStrategy);
  }

  it('selects DevJwtAuthenticationStrategy by default (AUTHENTICATION_STRATEGY_MODE unset)', () => {
    delete process.env.AUTHENTICATION_STRATEGY_MODE;
    const factory = buildFactory();
    const selectedStrategy = factory.resolveActiveAuthenticationStrategy();
    expect(selectedStrategy.strategyIdentifier).toBe('dev-jwt');
    expect(selectedStrategy).toBeInstanceOf(DevJwtAuthenticationStrategy);
  });

  it('selects DevJwtAuthenticationStrategy when explicitly configured', () => {
    process.env.AUTHENTICATION_STRATEGY_MODE = 'dev-jwt';
    const factory = buildFactory();
    expect(factory.resolveActiveAuthenticationStrategy().strategyIdentifier).toBe('dev-jwt');
  });

  it('selects OidcAuthenticationStrategy when AUTHENTICATION_STRATEGY_MODE=oidc', () => {
    process.env.AUTHENTICATION_STRATEGY_MODE = 'oidc';
    const factory = buildFactory();
    const selectedStrategy = factory.resolveActiveAuthenticationStrategy();
    expect(selectedStrategy.strategyIdentifier).toBe('oidc');
    expect(selectedStrategy).toBeInstanceOf(OidcAuthenticationStrategy);
  });

  it('falls back to DevJwtAuthenticationStrategy for an unrecognised mode value', () => {
    process.env.AUTHENTICATION_STRATEGY_MODE = 'some-unrecognised-mode';
    const factory = buildFactory();
    expect(factory.resolveActiveAuthenticationStrategy().strategyIdentifier).toBe('dev-jwt');
  });
});
