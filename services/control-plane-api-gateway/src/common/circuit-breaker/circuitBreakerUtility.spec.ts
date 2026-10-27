/**
 * @file circuitBreakerUtility.spec.ts
 * Unit tests for {@link CircuitBreakerUtility} covering the full CLOSED → OPEN →
 * HALF_OPEN → CLOSED/OPEN state machine, using an injected fake clock so timing
 * transitions are deterministic and the suite runs instantly (no real timers).
 */

import { CircuitBreakerOpenError } from './circuitBreakerOpenError';
import { CircuitBreakerOperatingState, CircuitBreakerUtility } from './circuitBreakerUtility';

describe('CircuitBreakerUtility', () => {
  /** Mutable fake "now" used as the injected clock so OPEN-duration expiry is deterministic. */
  let fakeCurrentTimeInMilliseconds: number;

  beforeEach(() => {
    fakeCurrentTimeInMilliseconds = 0;
  });

  /** Builds a breaker wired to the local fake clock with small, test-friendly thresholds. */
  function createCircuitBreakerUnderTest(): CircuitBreakerUtility<string> {
    return new CircuitBreakerUtility<string>({
      circuitBreakerIdentifierName: 'test-downstream',
      consecutiveFailureThresholdBeforeOpening: 3,
      openStateDurationInMilliseconds: 1_000,
      currentTimeProviderFunction: () => fakeCurrentTimeInMilliseconds,
    });
  }

  it('starts CLOSED and passes successful calls through unchanged', async () => {
    const circuitBreaker = createCircuitBreakerUnderTest();
    const result = await circuitBreaker.executeWithCircuitBreakerProtection(async () => 'ok');
    expect(result).toBe('ok');
    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.CLOSED);
  });

  it('remains CLOSED and increments the failure counter below the trip threshold', async () => {
    const circuitBreaker = createCircuitBreakerUnderTest();
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(async () => {
      throw new Error('downstream failure');
    })).rejects.toThrow('downstream failure');
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(async () => {
      throw new Error('downstream failure');
    })).rejects.toThrow('downstream failure');
    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.CLOSED);
  });

  it('trips to OPEN after reaching the consecutive failure threshold and short-circuits subsequent calls', async () => {
    const circuitBreaker = createCircuitBreakerUnderTest();
    const failingOperation = async () => {
      throw new Error('downstream failure');
    };
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow('downstream failure');
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow('downstream failure');
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow('downstream failure');

    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.OPEN);

    const operationSpy = jest.fn(async () => 'should-not-be-called');
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(operationSpy)).rejects.toBeInstanceOf(CircuitBreakerOpenError);
    expect(operationSpy).not.toHaveBeenCalled();
  });

  it('transitions OPEN -> HALF_OPEN once the open duration elapses, and CLOSED on a successful probe', async () => {
    const circuitBreaker = createCircuitBreakerUnderTest();
    const failingOperation = async () => {
      throw new Error('downstream failure');
    };
    for (let attemptIndex = 0; attemptIndex < 3; attemptIndex += 1) {
      await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow();
    }
    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.OPEN);

    // Advance the fake clock past the configured open duration.
    fakeCurrentTimeInMilliseconds += 1_001;

    const result = await circuitBreaker.executeWithCircuitBreakerProtection(async () => 'recovered');
    expect(result).toBe('recovered');
    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.CLOSED);
  });

  it('re-opens immediately if the HALF_OPEN probe call fails', async () => {
    const circuitBreaker = createCircuitBreakerUnderTest();
    const failingOperation = async () => {
      throw new Error('downstream failure');
    };
    for (let attemptIndex = 0; attemptIndex < 3; attemptIndex += 1) {
      await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow();
    }
    fakeCurrentTimeInMilliseconds += 1_001;

    await expect(circuitBreaker.executeWithCircuitBreakerProtection(failingOperation)).rejects.toThrow('downstream failure');
    expect(circuitBreaker.getCurrentOperatingState()).toBe(CircuitBreakerOperatingState.OPEN);

    // Still within the new open window: calls are short-circuited without invoking the operation.
    const operationSpy = jest.fn(async () => 'unused');
    await expect(circuitBreaker.executeWithCircuitBreakerProtection(operationSpy)).rejects.toBeInstanceOf(CircuitBreakerOpenError);
    expect(operationSpy).not.toHaveBeenCalled();
  });
});
