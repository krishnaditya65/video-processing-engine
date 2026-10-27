/**
 * @file oidcAuthenticationStrategy.ts
 * {@link AuthenticationStrategyInterface} implementation delegating bearer-token
 * verification to an external OpenID Connect identity provider, via the injected
 * {@link OidcRelyingPartyClientLike}.
 *
 * ## Production wiring status (read before enabling `AUTHENTICATION_STRATEGY_MODE=oidc`)
 *
 * This service installs `passport-openidconnect`, `openid-client`, `passport`, and
 * `@nestjs/passport` as real dependencies (see package.json) so the authorization-code
 * login flow and token verification can be wired up without further package
 * installation. However, this reference implementation intentionally stops short of
 * performing a live OIDC discovery/handshake against a real identity provider, because:
 *
 * 1. Unit tests must not require network access to an IdP (project constraint).
 * 2. A real rollout needs IdP-specific decisions (claim names for roles, whether to
 *    validate via `/userinfo` vs. local JWKS-based ID-token verification, session vs.
 *    stateless bearer tokens) that vary per identity provider (Auth0, Okta, Cognito,
 *    Azure AD, etc.) and shouldn't be guessed at here.
 *
 * What IS real and tested: the {@link AuthenticationStrategyInterface} contract, this
 * class's claims-mapping logic, and {@link AuthenticationStrategyFactory}'s selection
 * logic (`oidcAuthenticationStrategy.spec.ts`, `authenticationStrategyFactory.spec.ts`).
 *
 * To finish production wiring:
 * 1. Implement a concrete {@link OidcRelyingPartyClientLike} using `openid-client`:
 *    `Issuer.discover(oidcIssuerUrl)` once at startup (or lazily, cached), then either
 *    `client.userinfo(bearerTokenValue)` or local JWKS-based ID-token verification.
 * 2. Provide that implementation in {@link AuthModule} (replacing the guard-rail
 *    `UnimplementedOidcRelyingPartyClient` this module provides by default) via a
 *    factory keyed off `authenticationStrategyMode === 'oidc'`.
 * 3. Set real `OIDC_ISSUER_URL` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` env vars —
 *    the defaults baked into `ApplicationConfigurationService` are dummy, clearly
 *    non-functional placeholders for local dev only.
 */

import { Injectable, Optional, UnauthorizedException } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { AuthenticatedUserPayload } from './authenticatedUserPayload.interface';
import { AuthenticationStrategyInterface } from './authenticationStrategy.interface';
import { OIDC_RELYING_PARTY_CLIENT, OidcRelyingPartyClientLike } from './oidcRelyingPartyClientLike.interface';

/**
 * Default {@link OidcRelyingPartyClientLike} bound when no real OIDC client
 * implementation has been provided (i.e. in local dev / test / any environment that
 * hasn't completed the production wiring described in this file's module TSDoc).
 * Always rejects, with a message pointing at what's missing, rather than silently
 * pretending to authenticate callers — a misconfigured "always succeeds" stub would be
 * a serious security bug, so this fails safe (closed) instead.
 */
@Injectable()
export class UnimplementedOidcRelyingPartyClient implements OidcRelyingPartyClientLike {
  public async resolveIdentityClaimsForBearerToken(): Promise<never> {
    throw new Error(
      'OidcAuthenticationStrategy has no real OidcRelyingPartyClientLike implementation wired up. ' +
        'This is expected in local dev/test (AUTHENTICATION_STRATEGY_MODE defaults to "dev-jwt"). ' +
        'To enable OIDC in production, provide a real openid-client-backed OIDC_RELYING_PARTY_CLIENT ' +
        'in AuthModule — see oidcAuthenticationStrategy.ts TSDoc.',
    );
  }
}

/**
 * Verifies bearer tokens by delegating to an OIDC identity provider (via the injected
 * {@link OidcRelyingPartyClientLike}) rather than verifying a locally-signed secret, as
 * {@link DevJwtAuthenticationStrategy} does. Selected at runtime by
 * {@link AuthenticationStrategyFactory} when `AUTHENTICATION_STRATEGY_MODE=oidc`.
 */
@Injectable()
export class OidcAuthenticationStrategy implements AuthenticationStrategyInterface {
  public readonly strategyIdentifier = 'oidc' as const;

  public constructor(
    @Optional() private readonly oidcRelyingPartyClient: OidcRelyingPartyClientLike = new UnimplementedOidcRelyingPartyClient(),
    private readonly applicationConfigurationService: ApplicationConfigurationService,
  ) {}

  /**
   * Verifies `bearerTokenValue` against the configured OIDC identity provider (via the
   * injected {@link OidcRelyingPartyClientLike}) and maps the resolved claims to an
   * {@link AuthenticatedUserPayload}.
   * @param bearerTokenValue - Raw access/ID token string.
   * @returns The verified {@link AuthenticatedUserPayload}.
   * @throws {UnauthorizedException} When the token fails IdP verification, or no real OIDC client has been wired up (see class TSDoc).
   */
  public async validateBearerTokenAndResolveAuthenticatedUser(bearerTokenValue: string): Promise<AuthenticatedUserPayload> {
    // Read (but do not otherwise use here) the configured issuer so misconfiguration
    // is visible in the thrown error's context during local troubleshooting.
    const { oidcIssuerUrl } = this.applicationConfigurationService.getRuntimeConfiguration();

    try {
      const resolvedClaims = await this.oidcRelyingPartyClient.resolveIdentityClaimsForBearerToken(bearerTokenValue);
      return {
        subjectUserIdentifier: resolvedClaims.subjectIdentifier,
        displayNameOrEmail: resolvedClaims.emailAddress,
        grantedRoles: resolvedClaims.roles ?? [],
        tokenExpiresAtEpochSeconds: resolvedClaims.expiresAtEpochSeconds,
      };
    } catch (oidcVerificationError) {
      throw new UnauthorizedException(
        `OIDC bearer token verification failed against issuer "${oidcIssuerUrl}": ${
          oidcVerificationError instanceof Error ? oidcVerificationError.message : 'unknown error'
        }`,
      );
    }
  }
}

// Re-exported for convenience so `AuthModule` can import both the token and the
// interface type from this file's sibling without a second import line.
export { OIDC_RELYING_PARTY_CLIENT };
