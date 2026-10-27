/**
 * @file manifestFilterRuleRequestBody.ts
 * Request body contract for `POST /internal/remediation/manifest-filter-rule`.
 *
 * Receiving side of a contract owned jointly with the `analytics-telemetry-platform`
 * service: when its telemetry pipeline detects a specific device cohort (a client
 * app version, device model, or OS version range) suffering from playback quality
 * degradation, it calls this endpoint to have the control plane apply a mitigating
 * rule the next time `resolvePersonalizedManifestUrlForPlaybackSession` resolves a
 * manifest for a matching device — e.g. forcing a lower bitrate ladder ceiling or a
 * more compatible DRM system for that cohort until the underlying issue is fixed.
 * Keep this DTO's field names and types in lockstep with that service's outbound
 * caller — see SERVICE_README.md's "Internal remediation webhook contract" section.
 */

import { IsIn, IsNotEmpty, IsString, MaxLength } from 'class-validator';

/**
 * Mitigating actions the manifest resolution path may apply to a matched device
 * cohort. `OTHER` is a forward-compatible escape hatch, matching
 * {@link KNOWN_EXPERIMENT_ROLLBACK_VERDICTS}'s rationale.
 */
export const KNOWN_MANIFEST_FILTER_RULE_ACTIONS = [
  'CAP_BITRATE_LADDER',
  'FORCE_CLEARKEY_DRM',
  'BLOCK_MANIFEST_RESOLUTION',
  'PIN_LAST_KNOWN_GOOD_MANIFEST_VERSION',
  'OTHER',
] as const;

/** Request body for `POST /internal/remediation/manifest-filter-rule`. */
export class ManifestFilterRuleRequestBody {
  /**
   * Opaque matcher string identifying the affected device cohort (e.g.
   * `"platform:ios;appVersion:<4.2.0"` or a device-model glob) — interpreted by the
   * (future) manifest-resolution filter chain; this receiver stores it verbatim and
   * does not itself parse/validate matcher syntax.
   */
  @IsString()
  @IsNotEmpty()
  @MaxLength(500)
  deviceCohortMatcher!: string;

  /** Mitigating action to apply to manifest resolutions matching this cohort. */
  @IsString()
  @IsIn(KNOWN_MANIFEST_FILTER_RULE_ACTIONS)
  action!: (typeof KNOWN_MANIFEST_FILTER_RULE_ACTIONS)[number];

  /** Free-text, human-readable explanation for the filter rule (surfaced in logs/audit trail). */
  @IsString()
  @IsNotEmpty()
  @MaxLength(2000)
  reason!: string;
}
