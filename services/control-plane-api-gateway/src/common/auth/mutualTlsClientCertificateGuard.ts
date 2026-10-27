/**
 * @file mutualTlsClientCertificateGuard.ts
 * NestJS guard enforcing mutual TLS (mTLS) at the application layer — in the common,
 * real-world sense of that phrase for a service running behind a reverse
 * proxy/service-mesh sidecar: **TLS termination and the actual client-certificate
 * cryptographic verification happen at the proxy** (nginx, Envoy, an Istio/Linkerd
 * sidecar), not in this Node process. This guard trusts a header the proxy sets *only
 * after* it has itself verified the client certificate, and rejects any request that
 * reaches this process without that header — i.e. it defends against someone bypassing
 * the proxy and hitting this service directly.
 *
 * This is a standard, widely-used pattern (see e.g. Envoy's
 * `x-forwarded-client-cert`, or a simpler custom boolean header as modelled here) and
 * is *not* a weaker substitute for real mTLS: the cryptographic verification still
 * happens, just one hop earlier, where TLS is naturally terminated.
 *
 * ## Required nginx configuration (production)
 * ```nginx
 * server {
 *   listen 443 ssl;
 *   ssl_certificate     /etc/nginx/certs/server.crt;
 *   ssl_certificate_key /etc/nginx/certs/server.key;
 *
 *   # Require and verify the client certificate against a trusted internal CA.
 *   ssl_client_certificate /etc/nginx/certs/internal-ca.crt;
 *   ssl_verify_client on;
 *   ssl_verify_depth 2;
 *
 *   location / {
 *     # Only forwarded when nginx itself verified the presented cert against the CA
 *     # above ($ssl_client_verify is exactly the string "SUCCESS" on success).
 *     proxy_set_header x-client-cert-verified $ssl_client_verify;
 *     proxy_pass http://control-plane-api-gateway:4000;
 *   }
 * }
 * ```
 *
 * ## Required Envoy configuration (production, equivalent intent)
 * ```yaml
 * transport_socket:
 *   name: envoy.transport_sockets.tls
 *   typed_config:
 *     "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
 *     require_client_certificate: true
 *     common_tls_context:
 *       validation_context:
 *         trusted_ca: { filename: /etc/envoy/certs/internal-ca.crt }
 * # Then, in the HTTP filter chain, inject the verified-cert header, e.g. via a Lua/
 * # header-mutation filter that sets `x-client-cert-verified: SUCCESS` only when
 * # request.sslPeerCertificatePresented() is true — mirroring the nginx snippet above.
 * ```
 *
 * ## What this guard does NOT do
 * - It does not parse or validate certificate contents (CN/SAN/expiry) itself — that
 *   is the proxy's job, upstream of this process.
 * - It provides no protection if the proxy is misconfigured or bypassed at the
 *   network layer (e.g. the app port is directly reachable) — network-level isolation
 *   (security groups / service mesh mTLS between mesh sidecars) is still required.
 * - It is disabled by default (`mutualTlsEnforcementEnabled=false`) so the service
 *   remains runnable locally without a proxy in front of it; enable it only once the
 *   proxy config above is actually in place.
 */

import { CanActivate, ExecutionContext, ForbiddenException, Injectable } from '@nestjs/common';
import { GqlExecutionContext } from '@nestjs/graphql';
import { ApplicationConfigurationService } from '../../config/applicationConfigurationService';

/** Minimal request shape this guard needs. */
interface RequestForMutualTlsVerification {
  headers: Record<string, string | string[] | undefined>;
}

/** Value the trusted proxy is expected to set on a successfully-verified mTLS connection (see file TSDoc's nginx `$ssl_client_verify` snippet). */
const EXPECTED_VERIFIED_HEADER_VALUE = 'SUCCESS';

/**
 * Guard that, when {@link ApplicationConfigurationService.mutualTlsEnforcementEnabled}
 * is `true`, requires the configured proxy-verified-client-certificate header to be
 * present and equal to `"SUCCESS"`. No-ops (always allows) when the flag is `false`,
 * so it is safe to apply broadly (e.g. alongside `RedisBackedRateLimitingGuard` on the
 * internal remediation controller) without affecting environments that don't yet run
 * behind an mTLS-terminating proxy.
 */
@Injectable()
export class MutualTlsClientCertificateGuard implements CanActivate {
  public constructor(private readonly applicationConfigurationService: ApplicationConfigurationService) {}

  /**
   * @param executionContext - The current Nest execution context (GraphQL or HTTP).
   * @returns `true` when mTLS enforcement is disabled, or the proxy-verified header is present and successful.
   * @throws {ForbiddenException} When enforcement is enabled and the header is missing/not "SUCCESS".
   */
  public canActivate(executionContext: ExecutionContext): boolean {
    const { mutualTlsEnforcementEnabled, mutualTlsVerifiedClientCertificateHeaderName } =
      this.applicationConfigurationService.getRuntimeConfiguration();

    if (!mutualTlsEnforcementEnabled) {
      return true;
    }

    const request = this.resolveUnderlyingRequest(executionContext);
    const headerValue = request.headers[mutualTlsVerifiedClientCertificateHeaderName.toLowerCase()];
    const normalisedHeaderValue = Array.isArray(headerValue) ? headerValue[0] : headerValue;

    if (normalisedHeaderValue !== EXPECTED_VERIFIED_HEADER_VALUE) {
      throw new ForbiddenException(
        `Request is missing a proxy-verified mTLS client certificate (expected header "${mutualTlsVerifiedClientCertificateHeaderName}: ${EXPECTED_VERIFIED_HEADER_VALUE}"). ` +
          'This request must be routed through the mTLS-terminating reverse proxy — see MutualTlsClientCertificateGuard TSDoc.',
      );
    }
    return true;
  }

  /**
   * Resolves the underlying request from either a GraphQL or plain HTTP execution
   * context, matching the pattern used by the other guards in `common/auth` and
   * `common/rate-limiting`.
   * @param executionContext - The current Nest execution context.
   * @returns The underlying request.
   */
  private resolveUnderlyingRequest(executionContext: ExecutionContext): RequestForMutualTlsVerification {
    const gqlExecutionContext = GqlExecutionContext.create(executionContext);
    const gqlContext = gqlExecutionContext.getContext<{ req?: RequestForMutualTlsVerification }>();
    if (gqlContext?.req) {
      return gqlContext.req;
    }
    return executionContext.switchToHttp().getRequest<RequestForMutualTlsVerification>();
  }
}
