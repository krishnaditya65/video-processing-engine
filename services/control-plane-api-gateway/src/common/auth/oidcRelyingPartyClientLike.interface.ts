/**
 * @file oidcRelyingPartyClientLike.interface.ts
 * A narrow interface capturing exactly the OIDC relying-party operation
 * {@link OidcAuthenticationStrategy} depends on (fetching the caller's identity claims
 * for a presented access/ID token). Depending on this interface instead of the concrete
 * `openid-client` `Client` throughout {@link OidcAuthenticationStrategy} means:
 *  1. The strategy-selection and claims-mapping logic can be unit tested against a
 *     trivial in-memory fake, with zero network dependency on a real identity provider.
 *  2. The concrete OIDC client implementation (`openid-client`, `openid-client`'s
 *     successor, or `passport-openidconnect`) can be swapped later without touching
 *     {@link OidcAuthenticationStrategy}'s business logic — an application of the same
 *     Dependency Inversion pattern used for `RedisClientLike` (see
 *     `src/redis/redisClientLike.interface.ts`).
 *
 * A production implementation (not wired into unit tests) would perform OIDC discovery
 * against `oidcIssuerUrl` and either verify the ID token's signature against the IdP's
 * JWKS or call the `/userinfo` endpoint with the access token; both require live network
 * access to the identity provider, which this reference implementation's test suite
 * deliberately never performs. See SERVICE_README.md's "Authentication strategy
 * architecture" section for the full production wiring story.
 */

/** Identity claims resolved for a caller by an OIDC relying-party client. */
export interface OidcResolvedIdentityClaims {
  /** Stable subject identifier (the `sub` claim) issued by the identity provider. */
  subjectIdentifier: string;
  /** Email address claim, if the IdP/scope set includes it. */
  emailAddress?: string;
  /** Roles/groups claim, if the IdP includes one (claim name varies by IdP — mapped upstream). */
  roles?: string[];
  /** Unix epoch seconds the underlying token expires at. */
  expiresAtEpochSeconds: number;
}

/**
 * TypeScript interfaces have no runtime representation, so NestJS dependency injection
 * needs a concrete token to bind {@link OidcRelyingPartyClientLike} implementations to
 * — inject via `@Inject(OIDC_RELYING_PARTY_CLIENT)`. Mirrors the `VIDEO_ASSET_REPOSITORY`
 * token pattern used for the Repository pattern (see
 * `src/asset-registry/repositories/videoAssetRepository.token.ts`): swapping the bound
 * implementation in `AuthModule` is the entire "wire up a real OIDC client" story.
 */
export const OIDC_RELYING_PARTY_CLIENT = Symbol('OIDC_RELYING_PARTY_CLIENT');

/**
 * Minimal OIDC relying-party client surface consumed by {@link OidcAuthenticationStrategy}.
 * A real implementation wraps `openid-client` (installed as a dependency of this
 * service — see package.json) and performs the actual network handshake with the
 * identity provider configured via `OIDC_ISSUER_URL`/`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET`.
 */
export interface OidcRelyingPartyClientLike {
  /**
   * Resolves the identity claims for a presented bearer token, verifying it against
   * the configured identity provider.
   * @param bearerTokenValue - The raw access/ID token presented by the caller.
   * @returns The resolved {@link OidcResolvedIdentityClaims}.
   * @throws An implementation-specific error when the token is invalid, expired, or verification against the IdP fails.
   */
  resolveIdentityClaimsForBearerToken(bearerTokenValue: string): Promise<OidcResolvedIdentityClaims>;
}
