/**
 * @file Pure QoE (Quality of Experience) metric computation functions.
 *
 * Every function in this file is pure: given an array of
 * `NormalizedPlaybackBeacon` objects, it returns a computed metric with no
 * side effects and no dependency on ClickHouse, Kafka, or any other I/O.
 * This is intentional -- it is what makes this layer exhaustively
 * unit-testable with hand-computed fixtures (see
 * `tests/qoeMetrics.test.ts`), independent of any live infrastructure.
 */
import type { NormalizedPlaybackBeacon } from '../types/beaconTypes';

/**
 * Computes the Rebuffering Ratio for a batch of beacons: the fraction of
 * total playback time spent rebuffering.
 *
 * Formula:
 *   rebufferingRatio = totalRebufferDurationMillis /
 *                       (totalRebufferDurationMillis + totalWatchTimeMillis)
 *
 * where `totalWatchTimeMillis` is approximated as the sum of the deltas
 * between consecutive heartbeat beacons' `playbackPositionSeconds` within
 * each session (i.e. actual video-time-advanced, not wall-clock time,
 * which is robust to paused sessions). `totalRebufferDurationMillis` is
 * the sum of every `rebuffer_end` beacon's `rebufferDurationMillis`.
 *
 * This is the industry-standard QoE definition (used by, e.g., the CTA
 * WAVE / IAB video QoE guidelines): the proportion of a viewer's total
 * time-in-player that was spent stalled rather than watching.
 *
 * @param beacons - Batch of normalized beacons, potentially spanning
 * multiple sessions/assets; the ratio is computed in aggregate across
 * whatever beacons are passed in (callers filter/group by asset/session as
 * needed before calling).
 * @returns The rebuffering ratio in the closed interval [0, 1]. Returns 0
 * if there is no measurable watch time or rebuffering (empty/degenerate
 * input).
 */
export function computeRebufferingRatioFromPlaybackBeaconBatch(
  beacons: NormalizedPlaybackBeacon[],
): number {
  const totalRebufferDurationMillis = beacons
    .filter((beacon) => beacon.eventType === 'rebuffer_end' && beacon.rebufferDurationMillis !== null)
    .reduce((sum, beacon) => sum + (beacon.rebufferDurationMillis as number), 0);

  const totalWatchTimeMillis = computeTotalWatchTimeMillisFromHeartbeats(beacons);

  const denominator = totalRebufferDurationMillis + totalWatchTimeMillis;
  if (denominator === 0) {
    return 0;
  }
  return totalRebufferDurationMillis / denominator;
}

/**
 * Sums positive playback-position deltas between consecutive `heartbeat`
 * beacons within each session, used as the "total watch time" denominator
 * input for `computeRebufferingRatioFromPlaybackBeaconBatch`. Beacons are
 * grouped by `sessionId` and sorted by `eventTimestampEpochMillis` before
 * differencing so out-of-order delivery does not produce negative deltas.
 *
 * @param beacons - Batch of normalized beacons.
 * @returns Total watch time in milliseconds, summed across all sessions
 * present in the batch.
 */
function computeTotalWatchTimeMillisFromHeartbeats(beacons: NormalizedPlaybackBeacon[]): number {
  const heartbeatsBySessionId = new Map<string, NormalizedPlaybackBeacon[]>();

  for (const beacon of beacons) {
    if (beacon.eventType !== 'heartbeat' || beacon.playbackPositionSeconds === null) {
      continue;
    }
    const sessionHeartbeats = heartbeatsBySessionId.get(beacon.sessionId) ?? [];
    sessionHeartbeats.push(beacon);
    heartbeatsBySessionId.set(beacon.sessionId, sessionHeartbeats);
  }

  let totalWatchTimeMillis = 0;
  for (const sessionHeartbeats of heartbeatsBySessionId.values()) {
    const sorted = [...sessionHeartbeats].sort(
      (a, b) => a.eventTimestampEpochMillis - b.eventTimestampEpochMillis,
    );
    for (let i = 1; i < sorted.length; i += 1) {
      const previous = sorted[i - 1] as NormalizedPlaybackBeacon;
      const current = sorted[i] as NormalizedPlaybackBeacon;
      const deltaSeconds =
        (current.playbackPositionSeconds as number) - (previous.playbackPositionSeconds as number);
      if (deltaSeconds > 0) {
        totalWatchTimeMillis += deltaSeconds * 1000;
      }
    }
  }
  return totalWatchTimeMillis;
}

/**
 * Computes Time-To-First-Frame (TTFF) percentiles (p50/p95/p99) across all
 * `first_frame` beacons in a batch, using the nearest-rank percentile
 * method.
 *
 * Nearest-rank method: for a sorted array of N values and target
 * percentile p (0-100), the rank is `ceil(p / 100 * N)`, and the
 * percentile value is the element at that rank (1-indexed). This method is
 * chosen over linear interpolation because it always returns an actually
 * observed sample value, which is desirable for latency-percentile
 * reporting (avoids fabricating a "phantom" value between two real
 * samples).
 *
 * @param beacons - Batch of normalized beacons.
 * @returns Object with `p50`, `p95`, `p99` TTFF in milliseconds. All zero
 * if no `first_frame` beacons with a non-null `timeToFirstFrameMillis` are
 * present.
 */
export function computeTimeToFirstFramePercentilesFromPlaybackBeaconBatch(
  beacons: NormalizedPlaybackBeacon[],
): { p50: number; p95: number; p99: number } {
  const ttffValues = beacons
    .filter((beacon) => beacon.eventType === 'first_frame' && beacon.timeToFirstFrameMillis !== null)
    .map((beacon) => beacon.timeToFirstFrameMillis as number)
    .sort((a, b) => a - b);

  if (ttffValues.length === 0) {
    return { p50: 0, p95: 0, p99: 0 };
  }

  return {
    p50: computeNearestRankPercentile(ttffValues, 50),
    p95: computeNearestRankPercentile(ttffValues, 95),
    p99: computeNearestRankPercentile(ttffValues, 99),
  };
}

/**
 * Computes the nearest-rank percentile of a pre-sorted-ascending numeric
 * array.
 *
 * @param sortedAscendingValues - Numeric values sorted ascending.
 * @param percentile - Target percentile, 0-100.
 * @returns The value at the computed nearest rank.
 */
function computeNearestRankPercentile(sortedAscendingValues: number[], percentile: number): number {
  const rank = Math.ceil((percentile / 100) * sortedAscendingValues.length);
  const clampedIndex = Math.min(Math.max(rank, 1), sortedAscendingValues.length) - 1;
  return sortedAscendingValues[clampedIndex] as number;
}

/**
 * Computes the (unweighted-by-duration) average bitrate played across all
 * beacons in a batch that report a `bitrateKbps` value (i.e. `heartbeat`
 * and `bitrate_switch` beacons).
 *
 * Formula: arithmetic mean of every non-null `bitrateKbps` sample in the
 * batch. This is a simple sample-average, not time-weighted; a
 * time-weighted variant would additionally require the duration each
 * bitrate was held, which is a documented possible extension (see README
 * "Limitations").
 *
 * @param beacons - Batch of normalized beacons.
 * @returns Average bitrate in kbps, or 0 if no beacons report a bitrate.
 */
export function computeAverageBitratePlayedFromPlaybackBeaconBatch(
  beacons: NormalizedPlaybackBeacon[],
): number {
  const bitrateSamples = beacons
    .filter((beacon) => beacon.bitrateKbps !== null)
    .map((beacon) => beacon.bitrateKbps as number);

  if (bitrateSamples.length === 0) {
    return 0;
  }
  const sum = bitrateSamples.reduce((total, value) => total + value, 0);
  return sum / bitrateSamples.length;
}

/**
 * Computes the Completion Percentage for a single playback session: the
 * furthest `playbackPositionSeconds` reached, divided by
 * `assetDurationSeconds`, expressed as a percentage in [0, 100].
 *
 * Formula: completionPercentage = min(maxPlaybackPositionSeconds /
 * assetDurationSeconds, 1) * 100. Clamped at 100 to defend against
 * clients reporting a position slightly beyond duration due to
 * segment-boundary rounding.
 *
 * @param sessionBeacons - Beacons belonging to a single playback session
 * (mixed event types are fine; only `playbackPositionSeconds` and
 * `assetDurationSeconds` are consulted).
 * @returns Completion percentage in [0, 100], or 0 if no position/duration
 * data is present in the batch.
 */
export function computeCompletionPercentageFromPlaybackSessionBeaconBatch(
  sessionBeacons: NormalizedPlaybackBeacon[],
): number {
  const positions = sessionBeacons
    .filter((beacon) => beacon.playbackPositionSeconds !== null)
    .map((beacon) => beacon.playbackPositionSeconds as number);

  const assetDurationSeconds = sessionBeacons.find(
    (beacon) => beacon.assetDurationSeconds !== null,
  )?.assetDurationSeconds;

  if (positions.length === 0 || !assetDurationSeconds || assetDurationSeconds <= 0) {
    return 0;
  }

  const maxPlaybackPositionSeconds = Math.max(...positions);
  const rawPercentage = (maxPlaybackPositionSeconds / assetDurationSeconds) * 100;
  return Math.min(rawPercentage, 100);
}
