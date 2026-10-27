/**
 * @file circuitBreakerUtility.ts
 * A small, dependency-free implementation of the Circuit Breaker design pattern
 * (Nygard, "Release It!"). Used to wrap outbound calls from this API gateway to the
 * downstream Encoding Orchestration service so that a failing/slow downstream does
 * not exhaust this gateway's resources (thread pool / event loop / connection pool)
 * under sustained failure, and so that the downstream is given breathing room to
 * recover instead of being hammered with retries.
 *
 * State machine:
 *
 *   CLOSED --(failureCount >= failureThreshold)--> OPEN
 *   OPEN --(openDurationElapsed)--> HALF_OPEN
 *   HALF_OPEN --(next call succeeds)--> CLOSED
 *   HALF_OPEN --(next call fails)--> OPEN
 *
 * This is intentionally generic (`CircuitBreakerUtility<TResult>`) so it can wrap any
 * async operation, not just HTTP/Temporal calls, and intentionally has zero NestJS
 * dependencies so it is trivially unit-testable in isolation.
 */

import { CircuitBreakerOpenError } from './circuitBreakerOpenError';

/** The three canonical circuit breaker states. */
export enum CircuitBreakerOperatingState {
  /** Calls pass through normally; failures are being counted toward the trip threshold. */
  CLOSED = 'CLOSED',
  /** Calls are short-circuited immediately with {@link CircuitBreakerOpenError}. */
  OPEN = 'OPEN',
  /** A single trial call is allowed through to probe downstream health. */
  HALF_OPEN = 'HALF_OPEN',
}

/** Configuration accepted by {@link CircuitBreakerUtility}'s constructor. */
export interface CircuitBreakerConfigurationOptions {
  /** Human-readable identifier used in error messages and (optionally) logs. */
  circuitBreakerIdentifierName: string;
  /** Number of consecutive failures required, while CLOSED, to trip the breaker to OPEN. */
  consecutiveFailureThresholdBeforeOpening: number;
  /** How long, in milliseconds, the breaker remains OPEN before transitioning to HALF_OPEN. */
  openStateDurationInMilliseconds: number;
  /**
   * Optional injectable clock (defaults to `Date.now`), overridable in unit tests so
   * OPEN → HALF_OPEN timing transitions can be tested deterministically without real
   * `setTimeout`/`sleep` delays.
   */
  currentTimeProviderFunction?: () => number;
}

/**
 * Generic, in-memory circuit breaker wrapping any `() => Promise<TResult>` operation.
 *
 * Usage:
 * ```ts
 * const breaker = new CircuitBreakerUtility({
 *   circuitBreakerIdentifierName: 'encoding-orchestrator',
 *   consecutiveFailureThresholdBeforeOpening: 5,
 *   openStateDurationInMilliseconds: 30_000,
 * });
 * const result = await breaker.executeWithCircuitBreakerProtection(() => callDownstream());
 * ```
 */
export class CircuitBreakerUtility<TResult = unknown> {
  /** Current state machine position. Starts CLOSED (optimistic). */
  private currentOperatingState: CircuitBreakerOperatingState = CircuitBreakerOperatingState.CLOSED;
  /** Count of consecutive failures observed while CLOSED (reset on any success). */
  private consecutiveFailureCount = 0;
  /** Wall-clock timestamp (ms) at which the breaker most recently transitioned into OPEN. */
  private timestampWhenBreakerLastOpenedInMilliseconds = 0;
  /** Injected/real clock function, see {@link CircuitBreakerConfigurationOptions.currentTimeProviderFunction}. */
  private readonly currentTimeProviderFunction: () => number;

  public constructor(private readonly configurationOptions: CircuitBreakerConfigurationOptions) {
    this.currentTimeProviderFunction = configurationOptions.currentTimeProviderFunction ?? (() => Date.now());
  }

  /**
   * Exposes the current state for observability/testing purposes.
   * @returns The breaker's current {@link CircuitBreakerOperatingState}.
   */
  public getCurrentOperatingState(): CircuitBreakerOperatingState {
    this.reevaluateOpenToHalfOpenTransitionIfDue();
    return this.currentOperatingState;
  }

  /**
   * Executes `protectedOperation` under circuit breaker protection.
   *
   * - If CLOSED: runs the operation; on failure increments the failure counter and
   *   trips to OPEN once the configured threshold is reached; on success resets the
   *   failure counter.
   * - If OPEN and the open-duration has not yet elapsed: rejects immediately with
   *   {@link CircuitBreakerOpenError} *without* invoking `protectedOperation`.
   * - If OPEN and the open-duration has elapsed: transitions to HALF_OPEN and allows
   *   exactly this one call through as a health probe.
   * - If HALF_OPEN: runs the operation; success closes the breaker (full reset);
   *   failure re-opens the breaker and resets the open-state timer.
   *
   * @param protectedOperation - The async operation to guard (e.g. an outbound RPC).
   * @returns The resolved value of `protectedOperation` when it is allowed to run and succeeds.
   * @throws {CircuitBreakerOpenError} When the breaker is OPEN and short-circuits the call.
   * @throws Whatever `protectedOperation` itself throws/rejects with, re-thrown after bookkeeping.
   */
  public async executeWithCircuitBreakerProtection(protectedOperation: () => Promise<TResult>): Promise<TResult> {
    this.reevaluateOpenToHalfOpenTransitionIfDue();

    if (this.currentOperatingState === CircuitBreakerOperatingState.OPEN) {
      throw new CircuitBreakerOpenError(this.configurationOptions.circuitBreakerIdentifierName);
    }

    try {
      const operationResult = await protectedOperation();
      this.recordSuccessfulCallOutcome();
      return operationResult;
    } catch (thrownError) {
      this.recordFailedCallOutcome();
      throw thrownError;
    }
  }

  /**
   * Transitions OPEN → HALF_OPEN once {@link CircuitBreakerConfigurationOptions.openStateDurationInMilliseconds}
   * has elapsed since the breaker opened. No-op in any other state. Called at the top
   * of every public entry point so external callers never observe a stale OPEN state
   * past its configured cool-down window.
   */
  private reevaluateOpenToHalfOpenTransitionIfDue(): void {
    if (this.currentOperatingState !== CircuitBreakerOperatingState.OPEN) {
      return;
    }
    const elapsedMillisecondsSinceOpened = this.currentTimeProviderFunction() - this.timestampWhenBreakerLastOpenedInMilliseconds;
    if (elapsedMillisecondsSinceOpened >= this.configurationOptions.openStateDurationInMilliseconds) {
      this.currentOperatingState = CircuitBreakerOperatingState.HALF_OPEN;
    }
  }

  /** Bookkeeping for a successful call: resets failure count and fully closes the breaker. */
  private recordSuccessfulCallOutcome(): void {
    this.consecutiveFailureCount = 0;
    this.currentOperatingState = CircuitBreakerOperatingState.CLOSED;
  }

  /**
   * Bookkeeping for a failed call. From HALF_OPEN, any single failure immediately
   * re-opens the breaker (fail fast on a bad probe). From CLOSED, increments the
   * failure counter and trips to OPEN once the configured threshold is met.
   */
  private recordFailedCallOutcome(): void {
    if (this.currentOperatingState === CircuitBreakerOperatingState.HALF_OPEN) {
      this.transitionToOpenState();
      return;
    }
    this.consecutiveFailureCount += 1;
    if (this.consecutiveFailureCount >= this.configurationOptions.consecutiveFailureThresholdBeforeOpening) {
      this.transitionToOpenState();
    }
  }

  /** Moves the breaker to OPEN and stamps the timestamp used for the HALF_OPEN cool-down check. */
  private transitionToOpenState(): void {
    this.currentOperatingState = CircuitBreakerOperatingState.OPEN;
    this.timestampWhenBreakerLastOpenedInMilliseconds = this.currentTimeProviderFunction();
  }
}
