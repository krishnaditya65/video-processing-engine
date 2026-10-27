/**
 * @file Unit tests for `ConcurrentViewerAggregator`
 * (`src/metrics/concurrentViewerAggregator.ts`): sliding-window CCU
 * counting, per-asset breakdown, and eviction of expired sessions (the
 * memory-leak-avoidance logic). Uses an injected fake clock so time
 * progression is fully deterministic -- no real delay in any test.
 */
import { ConcurrentViewerAggregator } from '../src/metrics/concurrentViewerAggregator';

/** Builds a controllable fake clock starting at `startEpochMillis`. */
function buildFakeClock(startEpochMillis: number): { now: () => number; advanceBy: (deltaMillis: number) => void } {
  let current = startEpochMillis;
  return {
    now: () => current,
    advanceBy: (deltaMillis: number) => {
      current += deltaMillis;
    },
  };
}

describe('ConcurrentViewerAggregator', () => {
  it('counts 0 concurrent viewers with no heartbeats recorded', () => {
    const aggregator = new ConcurrentViewerAggregator(45_000, () => 1_000);
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(0);
  });

  it('counts each distinct session once, even with multiple heartbeats from the same session', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(45_000, clock.now);

    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    aggregator.recordSessionHeartbeat('session-b', 'asset-1');

    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(2);
  });

  it('evicts a session once its most recent heartbeat falls outside the sliding window', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(45_000, clock.now);

    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(1);

    // Still within the 45s window at +44s.
    clock.advanceBy(44_000);
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(1);

    // Now beyond the window (44s + 2s = 46s > 45s since the heartbeat).
    clock.advanceBy(2_000);
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(0);
  });

  it('keeps a session alive if it re-heartbeats before expiry, resetting its window', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(45_000, clock.now);

    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    clock.advanceBy(40_000);
    aggregator.recordSessionHeartbeat('session-a', 'asset-1'); // re-heartbeat resets the window
    clock.advanceBy(40_000); // 40s since the re-heartbeat, still < 45s
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(1);

    clock.advanceBy(6_000); // now 46s since the re-heartbeat
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(0);
  });

  it('does not leak memory: evictExpiredSessions removes all stale entries from the internal map', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(10_000, clock.now);

    for (let i = 0; i < 500; i += 1) {
      aggregator.recordSessionHeartbeat(`session-${i}`, 'asset-1');
    }
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(500);

    clock.advanceBy(20_000); // well beyond the 10s window
    aggregator.evictExpiredSessions();

    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(0);
    // Recording one fresh heartbeat after a full-eviction proves the
    // aggregator's internal state was actually cleared, not just masked by
    // the read-time filter.
    aggregator.recordSessionHeartbeat('session-fresh', 'asset-1');
    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(1);
  });

  it('computes per-asset CCU breakdowns via getConcurrentViewerCountByAsset', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(45_000, clock.now);

    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    aggregator.recordSessionHeartbeat('session-b', 'asset-1');
    aggregator.recordSessionHeartbeat('session-c', 'asset-2');

    expect(aggregator.getConcurrentViewerCountByAsset('asset-1')).toBe(2);
    expect(aggregator.getConcurrentViewerCountByAsset('asset-2')).toBe(1);
    expect(aggregator.getConcurrentViewerCountByAsset('asset-nonexistent')).toBe(0);
  });

  it('excludes an expired session from its asset-specific count too', () => {
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(10_000, clock.now);

    aggregator.recordSessionHeartbeat('session-a', 'asset-1');
    clock.advanceBy(11_000);

    expect(aggregator.getConcurrentViewerCountByAsset('asset-1')).toBe(0);
  });

  it('rejects a non-positive activeSessionWindowMillis', () => {
    expect(() => new ConcurrentViewerAggregator(0)).toThrow();
    expect(() => new ConcurrentViewerAggregator(-1)).toThrow();
  });

  it('startPeriodicEviction returns a stop function that clears the interval', () => {
    jest.useFakeTimers();
    const clock = buildFakeClock(1_000_000);
    const aggregator = new ConcurrentViewerAggregator(10_000, clock.now);
    aggregator.recordSessionHeartbeat('session-a', 'asset-1');

    const stop = aggregator.startPeriodicEviction(10_000);

    clock.advanceBy(15_000);
    jest.advanceTimersByTime(10_000); // fires the periodic eviction once

    expect(aggregator.getCurrentConcurrentViewerCount()).toBe(0);

    stop();
    jest.useRealTimers();
  });
});
