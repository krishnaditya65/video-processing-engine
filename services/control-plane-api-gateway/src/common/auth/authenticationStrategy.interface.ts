/**
 * @file authenticationStrategy.interface.ts
 * Strategy-pattern seam for bearer-token verification. `JsonWebTokenAuthenticationGuard`
 * (the guard actually applied to resolvers today) hard-codes the HS256 dev-JWT
 * verification path directly via `JwtService`. This interface — and its two
 * implementations, {@link DevJwtAuthenticationStrategy} and
 * {@link OidcAuthenticationStrategy} — formalises that verification step as an
 * interchangeable strategy so a future `PluggableAuthenticationGuard` (or a swap of
 * `JsonWebTokenAuthenticationGuard`'s internals) can select the strategy at runtime via
 * {@link AuthenticationStrategyFactory} without resolvers ever knowing which
 * verification mechanism is in effect.
 *
 * This is deliberately scoped as a well-tested *scaffold*: the strategy-selection
 * logic and the token-validation *interface* are real, exercised by unit tests. The
 * OIDC strategy's actual network handshake against a live identity provider is out of
 * scope for this reference implementation (see {@link OidcAuthenticationStrategy}'s
 * TSDoc for what a production rollout still needs to wire up).
 */

import { AuthenticatedUserPayload } from './authenticatedUserPayload.interface';

/** Discriminates which {@link AuthenticationStrategyInterface} implementation is in effect. */
export type AuthenticationStrategyIdentifier = 'dev-jwt' | 'oidc';

/**
 * Common contract every bearer-token verification strategy implements, so callers
 * (guards, the factory) can treat "verify this token and give me back the caller's
 * identity" uniformly regardless of whether the token is a locally-signed HS256 dev
 * JWT or an ID token issued by an external OIDC provider.
 */
export interface AuthenticationStrategyInterface {
  /** Stable identifier for this strategy, used for logging/diagnostics and by {@link AuthenticationStrategyFactory} selection tests. */
  readonly strategyIdentifier: AuthenticationStrategyIdentifier;

  /**
   * Verifies the supplied bearer token and resolves the authenticated caller's
   * identity.
   * @param bearerTokenValue - The raw token string (already stripped of the `Bearer ` prefix).
   * @returns A promise resolving to the verified {@link AuthenticatedUserPayload}.
   * @throws An error (implementation-specific) when the token is missing, malformed, expired, or fails signature/issuer verification. Callers are expected to normalise this into an `UnauthorizedException`.
   */
  validateBearerTokenAndResolveAuthenticatedUser(bearerTokenValue: string): Promise<AuthenticatedUserPayload>;
}
