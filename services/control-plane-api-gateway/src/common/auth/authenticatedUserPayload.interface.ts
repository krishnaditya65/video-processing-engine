/**
 * @file authenticatedUserPayload.interface.ts
 * Shape of the decoded JWT payload attached to the request/GraphQL context once a
 * bearer token has passed {@link JsonWebTokenAuthenticationGuard}. Intentionally
 * minimal for this reference implementation — extend with tenant/org claims, scopes,
 * etc. as the real authentication provider (e.g. Auth0/Cognito/internal IdP) demands.
 */

/** Decoded, verified JWT claims describing the caller of a GraphQL/HTTP request. */
export interface AuthenticatedUserPayload {
  /** Stable subject identifier (user id) from the `sub` claim. */
  subjectUserIdentifier: string;
  /** Optional human-readable display name / email, if the issuing IdP includes it. */
  displayNameOrEmail?: string;
  /** Coarse-grained roles/scopes granted to this principal, e.g. ['viewer'], ['admin']. */
  grantedRoles: string[];
  /** Unix epoch seconds the token expires at (`exp` claim), echoed for downstream logging/telemetry. */
  tokenExpiresAtEpochSeconds: number;
}
