/**
 * @file Real-time concurrent-viewer (CCU) aggregation.
 *
 * `ConcurrentViewerAggregator` maintains an in-memory sliding-window count
 * of "active" playback sessions, derived from beacon heartbeats
 * (`recordSessionHeartbeat(sessionId, assetId, timestampEpochMillis)`). A
 * session is considered "active"/"concurrent" if its most recent recorded
 * heartbeat falls within `activeSessionWindowMillis` of the current time --
 * the standard sliding-window definition of CCU used by streaming
 * platforms (a viewer who stopped heartbeating more than N seconds ago is
 * no longer counted, without needing an explicit "session ended" beacon,
 * which is unreliable in practice -- players crash, lose connectivity, or
 * are killed without a clean teardown).
 *
 * This is intentionally an in-memory, single-process aggregator (documented
 * limitation, mirroring `InMemoryChunkProvenanceLookup`): a
 * production multi-replica deployment would need either sticky routing of
 * a given session's heartbeats to the same replica, or a shared store
 * (e.g. Redis with per-session TTL keys, counted via `SCAN`/keyspace
 * cardinality, or a CRDT-style approximate counter) to produce a
 * cluster-wide CCU figure; see README "Limitations".
 *
 * Memory-leak avoidance: naively storing "every heartbeat ever received"
 * would grow the in-memory map unboundedly. Instead, only the MOST RECENT
 * heartbeat per `sessionId` is retained (a `Map<sessionId, ...>` is a
 * natural fit -- inserting the same key again overwrites, it does not
 * grow), and `evictExpiredSessions` (called both lazily on every read/write
 * and explicitly by the returned `startPeriodicEviction` interval) removes
 * entries whose most recent heartbeat has fallen outside the active window,
 * bounding memory to O(currently-or-recently-active session count)
 * regardless of how long the process has been running or how many total
 * heartbeats it has ever received.
 */

/** A single session-heartbeat record retained by `ConcurrentViewerAggregator`, keyed by `sessionId`. */
interface SessionHeartbeatRecord {
  /** The asset (title) this session is currently playing, for per-asset CCU breakdowns. */
  assetId: string;
  /** Epoch-milliseconds timestamp of the most recent heartbeat received for this session. */
  lastHeartbeatEpochMillis: number;
}

/**
 * Default sliding window used to decide whether a session is still
 * "active"/concurrent: 45 seconds. Chosen to comfortably exceed a typical
 * player SDK heartbeat interval (commonly every 10-30 seconds in
 * production QoE beacon implementations) with margin for a couple of
 * missed/delayed heartbeats before a session is considered to have ended,
 * while still being short enough that CCU reacts to real audience drop-off
 * within under a minute.
 */
export const DEFAULT_ACTIVE_SESSION_WINDOW_MILLIS = 45_000;

/**
 * Maintains a sliding-window, in-memory count of concurrently active
 * playback sessions from beacon heartbeats.
 */
export class ConcurrentViewerAggregator {
  private readonly heartbeatsBySessionId = new Map<string, SessionHeartbeatRecord>();
  private readonly activeSessionWindowMillis: number;
  private readonly clock: () => number;

  /**
   * @param activeSessionWindowMillis - Sliding window, in milliseconds, within which a session's most recent heartbeat must fall to be counted as active; defaults to `DEFAULT_ACTIVE_SESSION_WINDOW_MILLIS`.
   * @param clock - Injectable clock; defaults to `Date.now`. Injected (rather than reading `Date.now()` inline) so tests can deterministically simulate the passage of time without real delay.
   */
  public constructor(
    activeSessionWindowMillis: number = DEFAULT_ACTIVE_SESSION_WINDOW_MILLIS,
    clock: () => number = Date.now,
  ) {
    if (activeSessionWindowMillis <= 0) {
      throw new Error('activeSessionWindowMillis must be positive');
    }
    this.activeSessionWindowMillis = activeSessionWindowMillis;
    this.clock = clock;
  }

  /**
   * Records a heartbeat for a session, marking it active as of the given
   * timestamp. Overwrites any prior heartbeat for the same `sessionId`
   * (only the most recent is retained -- see file header "Memory-leak
   * avoidance"). Also opportunistically evicts expired sessions, so memory
   * usage stays bounded even if `startPeriodicEviction` is never invoked
   * (e.g. in short-lived test processes).
   *
   * @param sessionId - Unique identifier of the playback session.
   * @param assetId - Identifier of the asset the session is currently playing.
   * @param heartbeatEpochMillis - Epoch-milliseconds timestamp of this heartbeat; defaults to the injected clock's current time.
   */
  public recordSessionHeartbeat(
    sessionId: string,
    assetId: string,
    heartbeatEpochMillis: number = this.clock(),
  ): void {
    this.heartbeatsBySessionId.set(sessionId, { assetId, lastHeartbeatEpochMillis: heartbeatEpochMillis });
    this.evictExpiredSessions();
  }

  /**
   * Returns the current concurrent-viewer count: the number of distinct
   * sessions whose most recent heartbeat is within `activeSessionWindowMillis`
   * of now.
   *
   * @returns The current CCU count across all assets.
   */
  public getCurrentConcurrentViewerCount(): number {
    this.evictExpiredSessions();
    return this.heartbeatsBySessionId.size;
  }

  /**
   * Returns the current concurrent-viewer count restricted to a single asset.
   *
   * @param assetId - Asset identifier to filter by.
   * @returns The current CCU count for `assetId` only.
   */
  public getConcurrentViewerCountByAsset(assetId: string): number {
    this.evictExpiredSessions();
    let count = 0;
    for (const record of this.heartbeatsBySessionId.values()) {
      if (record.assetId === assetId) {
        count += 1;
      }
    }
    return count;
  }

  /**
   * Removes every session whose most recent heartbeat has fallen outside
   * the active window, i.e. `now - lastHeartbeatEpochMillis > activeSessionWindowMillis`.
   * This is the core sliding-window eviction logic that keeps memory usage
   * bounded to (roughly) the currently-active session count rather than
   * growing forever as more sessions come and go over the process
   * lifetime.
   *
   * Called automatically by every read (`getCurrentConcurrentViewerCount`,
   * `getConcurrentViewerCountByAsset`) and write
   * (`recordSessionHeartbeat`), so correctness never depends on
   * `startPeriodicEviction` having been started; that helper exists purely
   * to bound memory during long idle periods with no reads or writes at
   * all (an aggregator that receives a burst of heartbeats and then goes
   * fully silent would otherwise hold those stale entries in memory
   * indefinitely until the next call).
   */
  public evictExpiredSessions(): void {
    const now = this.clock();
    for (const [sessionId, record] of this.heartbeatsBySessionId.entries()) {
      if (now - record.lastHeartbeatEpochMillis > this.activeSessionWindowMillis) {
        this.heartbeatsBySessionId.delete(sessionId);
      }
    }
  }

  /**
   * Starts a periodic timer that calls `evictExpiredSessions` on an
   * interval, bounding memory usage during idle periods (see
   * `evictExpiredSessions` doc comment). Returns a stop function; callers
   * (typically the service's composition root) are responsible for calling
   * it during shutdown to avoid leaking the interval timer itself.
   *
   * @param intervalMillis - How often to run eviction; defaults to the configured active-session window (a reasonable default cadence -- no session can go stale and then un-stale within less than the window, so evicting more often than the window itself has no correctness benefit).
   * @returns A function that clears the interval timer.
   */
  public startPeriodicEviction(intervalMillis: number = this.activeSessionWindowMillis): () => void {
    const intervalHandle = setInterval(() => this.evictExpiredSessions(), intervalMillis);
    // `unref` lets this timer not keep the Node.js process alive by itself
    // (matches the pattern of not blocking process exit for a purely
    // maintenance-oriented background timer); not all timer implementations
    // in all environments support `unref`, hence the guard.
    if (typeof intervalHandle.unref === 'function') {
      intervalHandle.unref();
    }
    return () => clearInterval(intervalHandle);
  }
}
