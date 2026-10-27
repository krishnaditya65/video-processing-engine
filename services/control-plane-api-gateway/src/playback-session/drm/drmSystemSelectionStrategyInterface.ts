/**
 * @file drmSystemSelectionStrategyInterface.ts
 * Strategy pattern contract for DRM system negotiation. Each supported DRM system
 * (Widevine, FairPlay, PlayReady, or none/ClearKey for dev/testing) implements this
 * interface; {@link DrmSystemSelectionStrategyFactory} selects the right strategy at
 * runtime based on the requesting device's declared capabilities. Adding a new DRM
 * system means adding one new strategy class and one factory branch — no existing
 * code changes required (Open/Closed Principle).
 */

/** Device/client capability hints relevant to DRM negotiation. */
export interface DeviceDrmCapabilityProfile {
  /** Coarse device platform family, e.g. 'ios', 'android', 'web', 'smarttv', 'set-top-box'. */
  devicePlatformFamily: string;
  /** DRM systems the requesting client declares support for, in the client's own preference order. */
  clientSupportedDrmSystemIdentifiers: string[];
}

/** Result of a successful DRM negotiation for a playback session. */
export interface DrmSystemNegotiationResult {
  /** The DRM system selected for this session, e.g. 'WIDEVINE', 'FAIRPLAY', 'PLAYREADY', 'CLEARKEY'. */
  selectedDrmSystemIdentifier: string;
  /** URL of the license acquisition endpoint the client should use to fetch content keys. */
  licenseAcquisitionEndpointUrl: string;
  /** Security level/robustness tier negotiated (DRM-system-specific meaning, e.g. Widevine L1/L3). */
  negotiatedSecurityLevel: string;
}

/** Strategy interface every concrete DRM system selection strategy must implement. */
export interface DrmSystemSelectionStrategyInterface {
  /** The DRM system identifier this strategy handles, e.g. 'WIDEVINE'. */
  readonly handledDrmSystemIdentifier: string;

  /**
   * Determines whether this strategy is applicable to the given device capability
   * profile (i.e. the device both platform-natively and declaratively supports this
   * DRM system).
   * @param deviceDrmCapabilityProfile - The requesting device's capability hints.
   * @returns `true` if this strategy can service the request.
   */
  isApplicableToDeviceCapabilityProfile(deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): boolean;

  /**
   * Produces the negotiated DRM configuration for a playback session using this
   * strategy's DRM system.
   * @param deviceDrmCapabilityProfile - The requesting device's capability hints.
   * @returns The negotiated {@link DrmSystemNegotiationResult}.
   */
  negotiateDrmConfigurationForDevice(deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): DrmSystemNegotiationResult;
}
