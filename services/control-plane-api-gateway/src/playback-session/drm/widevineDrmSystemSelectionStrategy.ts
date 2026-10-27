/**
 * @file widevineDrmSystemSelectionStrategy.ts
 * Concrete Strategy-pattern implementation for Google Widevine DRM, applicable to
 * Android, Web (Chrome/Firefox/Edge), and most Smart TV platforms.
 */

import { Injectable } from '@nestjs/common';
import {
  DeviceDrmCapabilityProfile,
  DrmSystemNegotiationResult,
  DrmSystemSelectionStrategyInterface,
} from './drmSystemSelectionStrategyInterface';

@Injectable()
export class WidevineDrmSystemSelectionStrategy implements DrmSystemSelectionStrategyInterface {
  public readonly handledDrmSystemIdentifier = 'WIDEVINE';

  /** @inheritdoc */
  public isApplicableToDeviceCapabilityProfile(deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): boolean {
    const platformSupportsWidevineNatively = ['android', 'web', 'smarttv'].includes(
      deviceDrmCapabilityProfile.devicePlatformFamily.toLowerCase(),
    );
    return (
      platformSupportsWidevineNatively &&
      deviceDrmCapabilityProfile.clientSupportedDrmSystemIdentifiers.includes(this.handledDrmSystemIdentifier)
    );
  }

  /** @inheritdoc */
  public negotiateDrmConfigurationForDevice(
    deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile,
  ): DrmSystemNegotiationResult {
    // Real deployments would select L1 (hardware-backed) vs L3 (software) based on
    // an on-device attestation result; simulated here as platform-dependent.
    const negotiatedSecurityLevel = deviceDrmCapabilityProfile.devicePlatformFamily.toLowerCase() === 'web' ? 'L3' : 'L1';
    return {
      selectedDrmSystemIdentifier: this.handledDrmSystemIdentifier,
      licenseAcquisitionEndpointUrl: 'https://drm-license.example.com/widevine/license',
      negotiatedSecurityLevel,
    };
  }
}
