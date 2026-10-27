/**
 * @file internalServiceAuthGuard.ts
 * NestJS guard protecting internal, service-to-service-only HTTP endpoints (currently
 * just `/internal/remediation/*`, see {@link InternalRemediationController}) with a
 * simple shared-secret header check.
 *
 * This is a deliberately minimal placeholder for a production-grade internal-caller
 * identity story. **Replace this with mTLS peer-certificate identity (see
 * {@link MutualTlsClientCertificateGuard}) or service-mesh identity (SPIFFE/SPIRE,
 * Istio/Linkerd peer authentication) in production** — a shared secret is simple to
 * reason about and test, but it is a single static credential that, if leaked, grants
 * any holder access, and it does not by itself prove *which* service is calling. A
 * mesh-based identity (mTLS client cert with a verified SPIFFE ID, or an
 * Envoy/Istio-injected identity header validated by the mesh control plane) both
 * authenticates *and* identifies the caller. The two mechanisms compose naturally:
 * this guard can run alongside {@link MutualTlsClientCertificateGuard} as
 * defence-in-depth during a migration window.
 */

import { CanActivate, ExecutionContext, Injectable, UnauthorizedException } from '@nestjs/common';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';

/** Header internal callers must present the shared secret in. */
export const INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME = 'x-internal-service-shared-secret';

/** Minimal request shape this guard needs. */
interface RequestForInternalServiceAuthCheck {
  headers: Record<string, string | string[] | undefined>;
}

/**
 * Guard requiring the `x-internal-service-shared-secret` header to exactly match
 * {@link ApplicationConfigurationService.internalServiceSharedSecret}. Intended for
 * REST controllers reached only by trusted internal callers (sibling services within
 * the same platform), never by client applications.
 */
@Injectable()
export class InternalServiceAuthGuard implements CanActivate {
  public constructor(private readonly applicationConfigurationService: ApplicationConfigurationService) {}

  /**
   * @param executionContext - The current Nest execution context (HTTP controller).
   * @returns `true` when the presented shared secret matches configuration.
   * @throws {UnauthorizedException} When the header is missing or does not match.
   */
  public canActivate(executionContext: ExecutionContext): boolean {
    const request = executionContext.switchToHttp().getRequest<RequestForInternalServiceAuthCheck>();
    const presentedSecretHeaderValue = request.headers[INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME];
    const normalisedPresentedSecret = Array.isArray(presentedSecretHeaderValue)
      ? presentedSecretHeaderValue[0]
      : presentedSecretHeaderValue;

    const { internalServiceSharedSecret } = this.applicationConfigurationService.getRuntimeConfiguration();

    if (!normalisedPresentedSecret || normalisedPresentedSecret !== internalServiceSharedSecret) {
      throw new UnauthorizedException(
        `Missing or invalid "${INTERNAL_SERVICE_SHARED_SECRET_HEADER_NAME}" header. This endpoint is for internal service-to-service calls only.`,
      );
    }
    return true;
  }
}
