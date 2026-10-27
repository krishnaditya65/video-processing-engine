/**
 * @file playbackSessionEntity.ts
 * Domain entity for a playback session: the record created when a client requests to
 * begin watching an asset, capturing device type, network hints, and the negotiated
 * DRM configuration. Session *state* (not the full record) is additionally mirrored
 * into Redis by {@link PlaybackSessionOrchestrationService} for fast reads/heartbeats
 * from edge-facing services — the entity here is the authoritative, richer record.
 */

import { DrmSystemNegotiationResult } from '../drm/drmSystemSelectionStrategyInterface';

/** Coarse network condition hint supplied by the client at session-creation time. */
export type PlaybackSessionNetworkHint = 'CELLULAR_LOW' | 'CELLULAR_HIGH' | 'WIFI' | 'WIRED_ETHERNET' | 'UNKNOWN';

/** A single playback session record. */
export interface PlaybackSessionEntity {
  /** Globally unique session identifier (UUID v4). */
  playbackSessionIdentifier: string;
  /** The video asset this session is playing. */
  videoAssetIdentifier: string;
  /** Subject id of the authenticated user who created the session. */
  requestingUserIdentifier: string;
  /** Device platform family, reused for DRM negotiation, e.g. 'ios', 'android', 'web'. */
  devicePlatformFamily: string;
  /** Client-declared network condition hint, used for initial bitrate/manifest personalisation. */
  networkConditionHint: PlaybackSessionNetworkHint;
  /** The DRM configuration negotiated for this session via the DRM selection strategy factory. */
  negotiatedDrmConfiguration: DrmSystemNegotiationResult;
  /** ISO-8601 timestamp the session was created. */
  createdAtIsoTimestamp: string;
  /** Seconds after which this session is considered expired if not renewed/heartbeat-ed. */
  sessionTimeToLiveInSeconds: number;
}
