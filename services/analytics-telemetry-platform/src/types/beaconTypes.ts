/**
 * @file Core domain types for QoE (Quality of Experience) playback telemetry
 * beacons emitted by client video players (web, mobile, TV/OTT apps, set-top
 * boxes) across the streaming platform.
 *
 * A "beacon" is a single telemetry event describing either a discrete
 * playback occurrence (e.g. a rebuffering stall, a bitrate switch, a decode
 * error) or a periodic heartbeat describing the current playback state
 * (bitrate, position, buffer health). Beacons are the atomic unit ingested
 * by the Kafka/Redpanda consumer, validated/normalized by the ingestion
 * pipeline, and ultimately persisted to ClickHouse for analytical querying.
 */

/**
 * The discrete kind of playback occurrence a beacon reports. This enumerates
 * every beacon "type" the platform's client SDKs are contracted to emit.
 */
export type PlaybackBeaconEventType =
  | 'session_start'
  | 'first_frame'
  | 'rebuffer_start'
  | 'rebuffer_end'
  | 'bitrate_switch'
  | 'frame_drop'
  | 'decode_error'
  | 'heartbeat'
  | 'session_end';

/**
 * Raw, untrusted shape of a playback beacon exactly as received on the wire
 * (JSON payload of a Kafka/Redpanda message value) before validation and
 * normalization. Field names mirror the wire protocol emitted by client
 * SDKs, which may include extraneous or malformed data -- this type is
 * intentionally permissive (`unknown`/optional-heavy) to reflect that it has
 * not yet passed through the Zod validation stage of the ingestion
 * pipeline. See `src/schemas/playbackBeaconSchema.ts` for the authoritative
 * validation contract.
 */
export type RawPlaybackBeaconWireMessage = Record<string, unknown>;

/**
 * A playback beacon that has been validated against
 * `playbackBeaconWireSchema` and normalized into strongly typed,
 * canonical-unit fields. This is the shape produced by the
 * validation+normalization pipeline stages and consumed by every
 * downstream layer (enrichment, ClickHouse insertion, QoE metric
 * computation, anomaly detection).
 */
export interface NormalizedPlaybackBeacon {
  /** Globally unique identifier for this individual beacon event. */
  beaconId: string;
  /** Identifier of the playback session this beacon belongs to. */
  sessionId: string;
  /** Identifier of the video asset/title being played. */
  assetId: string;
  /** Opaque identifier of the viewing account/profile, if available. */
  userId: string | null;
  /** The discrete kind of event this beacon reports. */
  eventType: PlaybackBeaconEventType;
  /** Wall-clock UTC timestamp (epoch milliseconds) the event occurred at the client. */
  eventTimestampEpochMillis: number;
  /**
   * Time-To-First-Frame in milliseconds, present only on `first_frame`
   * beacons: elapsed time from playback request to first rendered video
   * frame.
   */
  timeToFirstFrameMillis: number | null;
  /**
   * Duration of a rebuffering stall in milliseconds, present only on
   * `rebuffer_end` beacons (paired with a preceding `rebuffer_start`).
   */
  rebufferDurationMillis: number | null;
  /** Bitrate, in kilobits per second, actively being played at event time. */
  bitrateKbps: number | null;
  /** Video codec profile / rendition ladder rung identifier, e.g. "1080p_6000k". */
  renditionId: string | null;
  /** Count of dropped frames observed in the current measurement interval. */
  frameDropCount: number | null;
  /** Decoder error code, present only on `decode_error` beacons. */
  decodeErrorCode: string | null;
  /** Free-text/short decoder error message, present only on `decode_error` beacons. */
  decodeErrorMessage: string | null;
  /** Client-reported GPU/hardware decoder model string, e.g. "Amazon-AV1-ASIC-v2". */
  hardwareDecoderModel: string | null;
  /** Client device firmware version string, used to cohort decode-error anomaly detection. */
  deviceFirmwareVersion: string | null;
  /** Client device operating system, e.g. "tvOS 17.5", "Android 14". */
  deviceOperatingSystem: string;
  /** Autonomous System Number of the client's network provider, derived from IP at edge. */
  networkAutonomousSystemNumber: number | null;
  /** Human-readable ISP name associated with `networkAutonomousSystemNumber`. */
  internetServiceProviderName: string | null;
  /** Current playback position within the asset, in seconds, at event time. */
  playbackPositionSeconds: number | null;
  /** Total duration of the asset, in seconds, used to compute completion percentage. */
  assetDurationSeconds: number | null;
  /**
   * Distributed trace identifier propagated onto the beacon by the
   * micro-session tracing layer (see `src/tracing/otelSetup.ts`). Enables
   * linking a beacon back to the encoding worker/container that produced
   * the video chunk being played, via `ChunkProvenanceLookup`.
   */
  traceId: string | null;
}

/**
 * A `NormalizedPlaybackBeacon` after the enrichment pipeline stage has
 * attached derived/joined fields (e.g. resolved chunk provenance). This is
 * the final shape written to ClickHouse.
 */
export interface EnrichedPlaybackBeacon extends NormalizedPlaybackBeacon {
  /**
   * Identifier of the encoding worker/container that produced the video
   * chunk associated with this beacon's `traceId`, resolved via
   * `ChunkProvenanceLookup`. `null` when no provenance record could be
   * resolved (e.g. trace expired from the lookup window).
   */
  sourceEncodingWorkerId: string | null;
  /** UTC epoch-millisecond timestamp at which the beacon was ingested by this service. */
  ingestedAtEpochMillis: number;
}
