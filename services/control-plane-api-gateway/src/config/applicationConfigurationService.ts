/**
 * @file applicationConfigurationService.ts
 * Centralised, strongly-typed access to environment-derived configuration for the
 * Control Plane & API Gateway service. Keeping configuration reads behind a single
 * injectable service (rather than scattering `process.env.X` throughout the codebase)
 * makes the service easy to test (inject a fake configuration) and easy to audit for
 * which environment variables the service actually depends on.
 *
 * Design note: this is intentionally a hand-rolled configuration service rather than
 * `@nestjs/config` to keep the dependency surface small for this reference
 * implementation; swapping to `@nestjs/config` + `Joi` schema validation is a
 * drop-in upgrade path for a real production rollout (see SERVICE_README.md).
 */

import { Injectable } from '@nestjs/common';

/**
 * Strongly-typed shape of every environment-derived configuration value the
 * Control Plane & API Gateway service consumes at runtime.
 */
export interface ApplicationRuntimeConfigurationShape {
  /** TCP port the HTTP + GraphQL server listens on. */
  httpServerListenPort: number;
  /** Symmetric secret used to sign/verify JWT bearer tokens (HS256). Pluggable: swap for RS256/JWKS in production. */
  jsonWebTokenSigningSecret: string;
  /** Full Redis connection URL, e.g. redis://localhost:6379. Used for rate limiting and manifest cache-aside storage. */
  redisConnectionUrl: string;
  /** Time-to-live, in seconds, applied to cached manifest metadata entries under the cache-aside pattern. */
  manifestCacheEntryTimeToLiveInSeconds: number;
  /** Maximum number of requests permitted per rate-limit window per identity key. */
  rateLimitMaximumRequestsPerWindow: number;
  /** Length, in seconds, of the sliding/fixed rate-limit window. */
  rateLimitWindowDurationInSeconds: number;
  /** Temporal server frontend address, e.g. localhost:7233. Only used when actually connecting to a live Temporal cluster. */
  temporalServerFrontendAddress: string;
  /** Temporal namespace the distributed encoding workflows execute under. */
  temporalNamespace: string;
  /** Temporal task queue that the encoding worker polls and workflows/activities are dispatched to. */
  temporalEncodingTaskQueueName: string;
  /** Consecutive-failure threshold before the encoding-orchestrator circuit breaker trips to the OPEN state. */
  encodingOrchestratorCircuitBreakerFailureThreshold: number;
  /** Duration, in milliseconds, the circuit breaker stays OPEN before allowing a HALF_OPEN probe request. */
  encodingOrchestratorCircuitBreakerOpenDurationInMilliseconds: number;

  /**
   * Which {@link AuthenticationStrategyInterface} implementation the
   * {@link AuthenticationStrategyFactory} selects: `'dev-jwt'` (default, HS256
   * dev-secret JWTs signed by this same process — see `JsonWebTokenAuthenticationGuard`)
   * or `'oidc'` (delegates verification to an external OpenID Connect identity
   * provider). Production deployments should set this to `'oidc'` once real IdP
   * configuration (issuer/client id/secret below) is supplied.
   */
  authenticationStrategyMode: 'dev-jwt' | 'oidc';
  /** OIDC issuer URL (the IdP's `.well-known/openid-configuration` base). Dummy default for local dev only. */
  oidcIssuerUrl: string;
  /** OAuth2/OIDC client id registered with the IdP for this service. Dummy default for local dev only. */
  oidcClientId: string;
  /** OAuth2/OIDC client secret registered with the IdP for this service. Dummy default for local dev only — inject via secret manager in production. */
  oidcClientSecret: string;

  /**
   * When `true`, {@link MutualTlsClientCertificateGuard} enforces that requests carry
   * proof of a proxy-verified mTLS client certificate (see the guard's TSDoc and
   * SERVICE_README.md for the nginx/envoy configuration this depends on). Defaults to
   * `false` so the service remains runnable locally without a mTLS-terminating proxy
   * in front of it.
   */
  mutualTlsEnforcementEnabled: boolean;
  /** Name of the header a trusted reverse proxy sets to indicate mTLS client-cert verification succeeded. */
  mutualTlsVerifiedClientCertificateHeaderName: string;

  /**
   * Shared secret internal callers (e.g. the analytics-telemetry-platform service's
   * remediation webhook caller) must present via the `x-internal-service-shared-secret`
   * header to reach `/internal/remediation/*` endpoints. See
   * {@link InternalServiceAuthGuard}. Replace with mTLS/service-mesh identity
   * (e.g. SPIFFE/mTLS peer certificate validation) in production.
   */
  internalServiceSharedSecret: string;
  /** How long, in seconds, a received remediation action stays "active" in {@link ActiveRemediationActionsRegistry} before it is treated as expired. */
  remediationActionActiveTimeToLiveInSeconds: number;
}

/**
 * Injectable configuration accessor. All reads are resolved eagerly at construction
 * time from `process.env`, with sane development-friendly defaults so the service is
 * runnable out-of-the-box without a `.env` file (documented in SERVICE_README.md).
 */
@Injectable()
export class ApplicationConfigurationService {
  /** The resolved, immutable configuration snapshot for the lifetime of the process. */
  private readonly resolvedRuntimeConfiguration: ApplicationRuntimeConfigurationShape;

  /**
   * Reads and coerces every supported environment variable exactly once.
   * Numeric environment variables are parsed with `Number(...)` and fall back to
   * their documented default when unset or unparsable (NaN guard).
   */
  public constructor() {
    this.resolvedRuntimeConfiguration = {
      httpServerListenPort: this.parseIntegerEnvironmentVariableOrDefault('HTTP_SERVER_LISTEN_PORT', 4000),
      jsonWebTokenSigningSecret: process.env.JSON_WEB_TOKEN_SIGNING_SECRET ?? 'development-only-insecure-secret-change-me',
      redisConnectionUrl: process.env.REDIS_CONNECTION_URL ?? 'redis://localhost:6379',
      manifestCacheEntryTimeToLiveInSeconds: this.parseIntegerEnvironmentVariableOrDefault('MANIFEST_CACHE_ENTRY_TTL_SECONDS', 30),
      rateLimitMaximumRequestsPerWindow: this.parseIntegerEnvironmentVariableOrDefault('RATE_LIMIT_MAX_REQUESTS_PER_WINDOW', 100),
      rateLimitWindowDurationInSeconds: this.parseIntegerEnvironmentVariableOrDefault('RATE_LIMIT_WINDOW_DURATION_SECONDS', 60),
      temporalServerFrontendAddress: process.env.TEMPORAL_SERVER_FRONTEND_ADDRESS ?? 'localhost:7233',
      temporalNamespace: process.env.TEMPORAL_NAMESPACE ?? 'default',
      temporalEncodingTaskQueueName: process.env.TEMPORAL_ENCODING_TASK_QUEUE_NAME ?? 'distributed-encoding-task-queue',
      encodingOrchestratorCircuitBreakerFailureThreshold: this.parseIntegerEnvironmentVariableOrDefault(
        'ENCODING_ORCHESTRATOR_CIRCUIT_BREAKER_FAILURE_THRESHOLD',
        5,
      ),
      encodingOrchestratorCircuitBreakerOpenDurationInMilliseconds: this.parseIntegerEnvironmentVariableOrDefault(
        'ENCODING_ORCHESTRATOR_CIRCUIT_BREAKER_OPEN_DURATION_MS',
        30_000,
      ),
      authenticationStrategyMode: process.env.AUTHENTICATION_STRATEGY_MODE === 'oidc' ? 'oidc' : 'dev-jwt',
      // Dummy, non-functional defaults: local dev never actually performs a network
      // handshake against these (see OidcAuthenticationStrategy TSDoc). A production
      // rollout MUST override every OIDC_* variable with real IdP-issued values.
      oidcIssuerUrl: process.env.OIDC_ISSUER_URL ?? 'https://dev-idp.example-invalid.local/oidc',
      oidcClientId: process.env.OIDC_CLIENT_ID ?? 'control-plane-api-gateway-dev-client-id',
      oidcClientSecret: process.env.OIDC_CLIENT_SECRET ?? 'development-only-insecure-oidc-client-secret-change-me',
      mutualTlsEnforcementEnabled: process.env.MUTUAL_TLS_ENFORCEMENT_ENABLED === 'true',
      mutualTlsVerifiedClientCertificateHeaderName:
        process.env.MUTUAL_TLS_VERIFIED_CLIENT_CERTIFICATE_HEADER_NAME ?? 'x-client-cert-verified',
      internalServiceSharedSecret:
        process.env.INTERNAL_SERVICE_SHARED_SECRET ?? 'development-only-insecure-internal-shared-secret-change-me',
      remediationActionActiveTimeToLiveInSeconds: this.parseIntegerEnvironmentVariableOrDefault(
        'REMEDIATION_ACTION_ACTIVE_TTL_SECONDS',
        3600,
      ),
    };
  }

  /**
   * Returns the fully resolved, read-only runtime configuration snapshot.
   * @returns The {@link ApplicationRuntimeConfigurationShape} for this process.
   */
  public getRuntimeConfiguration(): Readonly<ApplicationRuntimeConfigurationShape> {
    return this.resolvedRuntimeConfiguration;
  }

  /**
   * Parses an integer-valued environment variable, falling back to `defaultValue`
   * when the variable is unset, empty, or not a valid integer.
   * @param environmentVariableName - Name of the `process.env` key to read.
   * @param defaultValue - Value to use when parsing fails or the variable is absent.
   * @returns The parsed integer, or `defaultValue`.
   */
  private parseIntegerEnvironmentVariableOrDefault(environmentVariableName: string, defaultValue: number): number {
    const rawValue = process.env[environmentVariableName];
    if (rawValue === undefined || rawValue.trim() === '') {
      return defaultValue;
    }
    const parsedValue = Number.parseInt(rawValue, 10);
    return Number.isNaN(parsedValue) ? defaultValue : parsedValue;
  }
}
