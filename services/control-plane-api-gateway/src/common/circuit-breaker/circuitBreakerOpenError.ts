/**
 * @file circuitBreakerOpenError.ts
 * Error thrown by {@link CircuitBreakerUtility} when a call is rejected because the
 * breaker is currently in the OPEN state (i.e. the downstream dependency is presumed
 * unhealthy and calls are being short-circuited to protect both caller and callee).
 */

/**
 * Thrown instead of invoking the wrapped operation whenever the circuit breaker is
 * OPEN. Callers should catch this distinctly from downstream operational errors so
 * they can apply fallback behaviour (cached data, degraded response, etc.) instead of
 * retrying immediately.
 */
export class CircuitBreakerOpenError extends Error {
  /**
   * @param circuitBreakerIdentifierName - Human-readable name of the circuit breaker instance that rejected the call, for observability/log correlation.
   */
  public constructor(public readonly circuitBreakerIdentifierName: string) {
    super(`Circuit breaker "${circuitBreakerIdentifierName}" is OPEN; call rejected without invoking the downstream operation.`);
    this.name = 'CircuitBreakerOpenError';
  }
}
