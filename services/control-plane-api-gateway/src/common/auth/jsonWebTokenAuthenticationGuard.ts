/**
 * @file jsonWebTokenAuthenticationGuard.ts
 * A deliberately simple, well-documented JWT bearer-token guard for GraphQL requests.
 * Auth is explicitly *not* the focus of this service (per project scope) — this guard
 * exists to demonstrate a clean, pluggable seam: swap {@link JsonWebTokenAuthenticationGuard}
 * for an OIDC/JWKS-validating guard, or add a `Strategy` layer (Passport) without
 * touching resolvers, by keeping all auth concerns behind this single Nest `CanActivate`.
 *
 * Usage: annotate a resolver/field with `@UseGuards(JsonWebTokenAuthenticationGuard)`.
 * On success, the decoded {@link AuthenticatedUserPayload} is attached to
 * `request.authenticatedUser` for retrieval via `CurrentAuthenticatedUser()`.
 */

import { CanActivate, ExecutionContext, Injectable, UnauthorizedException } from '@nestjs/common';
import { GqlExecutionContext } from '@nestjs/graphql';
import { JwtService } from '@nestjs/jwt';
import { AuthenticatedUserPayload } from './authenticatedUserPayload.interface';

/**
 * Express-compatible request augmented with the field this guard populates after a
 * successful verification, so downstream decorators/resolvers get type safety.
 */
interface RequestAugmentedWithAuthenticatedUser {
  headers: Record<string, string | string[] | undefined>;
  authenticatedUser?: AuthenticatedUserPayload;
}

/**
 * NestJS guard that extracts a `Bearer <token>` from the `Authorization` header of a
 * GraphQL request, verifies its signature/expiry via {@link JwtService}, and attaches
 * the decoded claims to the request for retrieval by resolvers.
 *
 * Edge cases handled explicitly:
 * - Missing `Authorization` header → 401 Unauthorized.
 * - Header present but not in `Bearer <token>` form → 401 Unauthorized.
 * - Token present but invalid signature / expired / malformed → 401 Unauthorized
 *   (the underlying `JsonWebTokenError`/`TokenExpiredError` is caught and normalised).
 */
@Injectable()
export class JsonWebTokenAuthenticationGuard implements CanActivate {
  public constructor(private readonly jwtService: JwtService) {}

  /**
   * NestJS guard entry point. Resolves the underlying request from either a GraphQL
   * or plain HTTP execution context, verifies the bearer token, and attaches the
   * decoded payload for downstream use.
   * @param executionContext - The current Nest execution context (GraphQL or HTTP).
   * @returns `true` when authentication succeeds.
   * @throws {UnauthorizedException} When the header is missing/malformed or the token fails verification.
   */
  public canActivate(executionContext: ExecutionContext): boolean {
    const request = this.resolveUnderlyingRequestFromExecutionContext(executionContext);
    const bearerToken = this.extractBearerTokenFromAuthorizationHeader(request.headers.authorization);

    try {
      const decodedPayload = this.jwtService.verify<Record<string, unknown>>(bearerToken);
      request.authenticatedUser = this.mapDecodedJwtClaimsToAuthenticatedUserPayload(decodedPayload);
      return true;
    } catch {
      throw new UnauthorizedException('The supplied bearer token is missing, malformed, expired, or has an invalid signature.');
    }
  }

  /**
   * Resolves the raw request object regardless of whether this guard runs in a
   * GraphQL resolver context (the common case for this gateway) or a plain REST
   * controller context, so it remains reusable across both transport styles.
   * @param executionContext - The current Nest execution context.
   * @returns The underlying HTTP request, augmented with the `authenticatedUser` slot.
   */
  private resolveUnderlyingRequestFromExecutionContext(
    executionContext: ExecutionContext,
  ): RequestAugmentedWithAuthenticatedUser {
    const gqlExecutionContext = GqlExecutionContext.create(executionContext);
    const gqlContext = gqlExecutionContext.getContext<{ req?: RequestAugmentedWithAuthenticatedUser }>();
    if (gqlContext?.req) {
      return gqlContext.req;
    }
    return executionContext.switchToHttp().getRequest<RequestAugmentedWithAuthenticatedUser>();
  }

  /**
   * Parses the `Authorization` header, requiring the standard `Bearer <token>` form.
   * @param authorizationHeaderValue - Raw header value, possibly absent.
   * @returns The extracted token string.
   * @throws {UnauthorizedException} When the header is absent or not `Bearer`-prefixed.
   */
  private extractBearerTokenFromAuthorizationHeader(authorizationHeaderValue: string | string[] | undefined): string {
    if (typeof authorizationHeaderValue !== 'string' || !authorizationHeaderValue.startsWith('Bearer ')) {
      throw new UnauthorizedException('Expected an "Authorization: Bearer <token>" header.');
    }
    return authorizationHeaderValue.slice('Bearer '.length).trim();
  }

  /**
   * Maps the loosely-typed decoded JWT claim object into the strongly-typed
   * {@link AuthenticatedUserPayload} used throughout the rest of the service.
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
