/**
 * @file authenticationStrategyFactory.ts
 * Factory (Factory + Strategy patterns, mirroring `DrmSystemSelectionStrategyFactory`)
 * that selects the {@link AuthenticationStrategyInterface} implementation in effect for
 * this process, based on {@link ApplicationConfigurationService.authenticationStrategyMode}.
 *
 * Consumers (a future `PluggableAuthenticationGuard`, or `JsonWebTokenAuthenticationGuard`
 * once migrated) call {@link resolveActiveAuthenticationStrategy} once per request
 * rather than injecting a concrete strategy directly, so switching the whole service
 * between local dev-JWT mode and OIDC mode is a single environment variable
 * (`AUTHENTICATION_STRATEGY_MODE`) rather than a code change.
 */

import { Injectable } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { AuthenticationStrategyInterface } from './authenticationStrategy.interface';
import { DevJwtAuthenticationStrategy } from './devJwtAuthenticationStrategy';
import { OidcAuthenticationStrategy } from './oidcAuthenticationStrategy';

/** Selects among registered {@link AuthenticationStrategyInterface} implementations based on runtime configuration. */
@Injectable()
export class AuthenticationStrategyFactory {
  public constructor(
    private readonly applicationConfigurationService: ApplicationConfigurationService,
    private readonly devJwtAuthenticationStrategy: DevJwtAuthenticationStrategy,
    private readonly oidcAuthenticationStrategy: OidcAuthenticationStrategy,
  ) {}

  /**
   * Resolves the {@link AuthenticationStrategyInterface} implementation that should
   * verify bearer tokens for the current process, per
   * `applicationConfigurationService.getRuntimeConfiguration().authenticationStrategyMode`.
   * @returns The active {@link AuthenticationStrategyInterface}.
   */
  public resolveActiveAuthenticationStrategy(): AuthenticationStrategyInterface {
    const { authenticationStrategyMode } = this.applicationConfigurationService.getRuntimeConfiguration();
    switch (authenticationStrategyMode) {
      case 'oidc':
        return this.oidcAuthenticationStrategy;
      case 'dev-jwt':
      default:
        return this.devJwtAuthenticationStrategy;
    }
  }
}
