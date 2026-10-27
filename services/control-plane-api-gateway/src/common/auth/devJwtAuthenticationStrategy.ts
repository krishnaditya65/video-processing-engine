/**
 * @file devJwtAuthenticationStrategy.ts
 * {@link AuthenticationStrategyInterface} implementation wrapping the same HS256
 * dev-secret verification `JsonWebTokenAuthenticationGuard` performs directly today.
 * Extracted behind the strategy interface so it can be selected/compared uniformly
 * against {@link OidcAuthenticationStrategy} by {@link AuthenticationStrategyFactory}.
 *
 * This remains the default strategy (`AUTHENTICATION_STRATEGY_MODE=dev-jwt`, or unset)
 * so local development and the existing test suite keep working unchanged: no IdP,
 * no network access, no additional local setup — just a shared HS256 secret.
 */

import { Injectable, UnauthorizedException } from '@nestjs/common';
import { JwtService } from '@nestjs/jwt';
import { AuthenticatedUserPayload } from './authenticatedUserPayload.interface';
import { AuthenticationStrategyInterface } from './authenticationStrategy.interface';

/** Verifies locally-signed HS256 JWTs using the shared secret configured via `JwtModule` (see `app.module.ts`). */
@Injectable()
export class DevJwtAuthenticationStrategy implements AuthenticationStrategyInterface {
  public readonly strategyIdentifier = 'dev-jwt' as const;

  public constructor(private readonly jwtService: JwtService) {}

  /**
   * Verifies `bearerTokenValue` via the shared-secret `JwtService` (same mechanism as
   * {@link JsonWebTokenAuthenticationGuard}) and maps the decoded claims to an
   * {@link AuthenticatedUserPayload}.
   * @param bearerTokenValue - Raw JWT string.
   * @returns The verified {@link AuthenticatedUserPayload}.
   * @throws {UnauthorizedException} When the token is malformed, expired, or fails signature verification.
   */
  public async validateBearerTokenAndResolveAuthenticatedUser(bearerTokenValue: string): Promise<AuthenticatedUserPayload> {
    try {
      const decodedPayload = this.jwtService.verify<Record<string, unknown>>(bearerTokenValue);
      return this.mapDecodedJwtClaimsToAuthenticatedUserPayload(decodedPayload);
    } catch {
      throw new UnauthorizedException('The supplied bearer token is missing, malformed, expired, or has an invalid signature.');
    }
  }

  /**
   * Maps loosely-typed decoded JWT claims into the strongly-typed
   * {@link AuthenticatedUserPayload} used throughout the rest of the service.
   * Duplicated intentionally (rather than imported) from
   * `JsonWebTokenAuthenticationGuard` to keep this strategy self-contained and
   * independently testable; see SERVICE_README.md for the consolidation path once the
   * guard is switched over to strategy-based dispatch.
   * @param decodedPayload - Raw decoded JWT claims.
   * @returns A normalised {@link AuthenticatedUserPayload}.
   */
  private mapDecodedJwtClaimsToAuthenticatedUserPayload(decodedPayload: Record<string, unknown>): AuthenticatedUserPayload {
    return {
      subjectUserIdentifier: String(decodedPayload.sub ?? 'unknown-subject'),
      displayNameOrEmail: typeof decodedPayload.email === 'string' ? decodedPayload.email : undefined,
      grantedRoles: Array.isArray(decodedPayload.roles) ? (decodedPayload.roles as string[]) : [],
      tokenExpiresAtEpochSeconds: typeof decodedPayload.exp === 'number' ? decodedPayload.exp : 0,
    };
  }
}
