/**
 * @file redisBackedRateLimitingGuard.ts
 * NestJS guard applying Redis-backed fixed-window rate limiting to GraphQL
 * operations. Delegates the actual counting logic to {@link RedisRateLimiterService}
 * (kept separate and framework-agnostic so it stays independently unit-testable) and
 * translates the result into either "allow" or a thrown {@link RateLimitExceededException}.
 *
 * Identity resolution: prefers the authenticated user's subject id (set by
 * {@link JsonWebTokenAuthenticationGuard} when both guards are combined), falling
 * back to the caller's IP address for unauthenticated/public operations — so even
 * anonymous traffic is protected against abuse.
 */

import { CanActivate, ExecutionContext, Injectable } from '@nestjs/common';
import { GqlExecutionContext } from '@nestjs/graphql';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';
import { RedisRateLimiterService } from '../../redis/redisRateLimiterService';
import { RateLimitExceededException } from './rateLimitExceededException';
import { AuthenticatedUserPayload } from '../auth/authenticatedUserPayload.interface';

/** Minimal request shape this guard needs to resolve a rate-limit identity key. */
interface RequestForRateLimitIdentityResolution {
  ip?: string;
  socket?: { remoteAddress?: string };
  authenticatedUser?: AuthenticatedUserPayload;
}

/**
 * Guard that must run on every rate-limited GraphQL resolver (typically applied at
 * the module/resolver level via `@UseGuards(RedisBackedRateLimitingGuard)`, or
 * globally via `APP_GUARD`). On exceeding the configured window limit, throws
 * {@link RateLimitExceededException} (HTTP 429) rather than allowing the request
 * through, protecting downstream services (asset registry, encoding orchestrator)
 * from being overwhelmed by a single noisy client.
 */
@Injectable()
export class RedisBackedRateLimitingGuard implements CanActivate {
  public constructor(
    private readonly redisRateLimiterService: RedisRateLimiterService,
    private readonly applicationConfigurationService: ApplicationConfigurationService,
  ) {}

  /**
   * Resolves the caller's rate-limit identity, records this attempt, and either
   * allows the request through or throws.
   * @param executionContext - The current Nest execution context.
   * @returns `true` when the request is within the configured rate limit.
   * @throws {RateLimitExceededException} When the caller has exceeded their window limit.
   */
  public async canActivate(executionContext: ExecutionContext): Promise<boolean> {
    const request = this.resolveUnderlyingRequest(executionContext);
    const rateLimitIdentityKey = this.resolveRateLimitIdentityKeyForRequest(request);
    const { rateLimitMaximumRequestsPerWindow, rateLimitWindowDurationInSeconds } =
      this.applicationConfigurationService.getRuntimeConfiguration();

    const rateLimitCheckResult = await this.redisRateLimiterService.recordRequestAttemptAndCheckRateLimit(
      rateLimitIdentityKey,
      rateLimitMaximumRequestsPerWindow,
      rateLimitWindowDurationInSeconds,
    );

    if (!rateLimitCheckResult.isRequestPermitted) {
      throw new RateLimitExceededException(rateLimitIdentityKey, rateLimitCheckResult.secondsRemainingInCurrentWindow);
    }
    return true;
  }

  /**
   * Resolves the underlying HTTP-like request object from either a GraphQL or plain
   * HTTP execution context.
   * @param executionContext - The current Nest execution context.
   * @returns The underlying request.
   */
  private resolveUnderlyingRequest(executionContext: ExecutionContext): RequestForRateLimitIdentityResolution {
    const gqlExecutionContext = GqlExecutionContext.create(executionContext);
    const gqlContext = gqlExecutionContext.getContext<{ req?: RequestForRateLimitIdentityResolution }>();
    if (gqlContext?.req) {
      return gqlContext.req;
    }
    return executionContext.switchToHttp().getRequest<RequestForRateLimitIdentityResolution>();
  }

  /**
   * Chooses the identity key to rate-limit on: authenticated subject id when
   * available (stronger, per-user fairness), else the remote IP address, else a
   * shared `anonymous` bucket as a last-resort fallback so the guard never throws
   * due to an unresolvable identity.
   * @param request - The resolved request object.
   * @returns A stable string key used to bucket rate-limit counters.
   */
  private resolveRateLimitIdentityKeyForRequest(request: RequestForRateLimitIdentityResolution): string {
    if (request.authenticatedUser?.subjectUserIdentifier) {
      return `user:${request.authenticatedUser.subjectUserIdentifier}`;
    }
    const remoteAddress = request.ip ?? request.socket?.remoteAddress;
    return remoteAddress ? `ip:${remoteAddress}` : 'ip:anonymous';
  }
}
