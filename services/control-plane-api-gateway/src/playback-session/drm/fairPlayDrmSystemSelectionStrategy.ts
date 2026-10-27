/**
 * @file fairPlayDrmSystemSelectionStrategy.ts
 * Concrete Strategy-pattern implementation for Apple FairPlay Streaming DRM,
 * applicable exclusively to iOS/tvOS/macOS Safari clients.
 */

import { Injectable } from '@nestjs/common';
import {
  DeviceDrmCapabilityProfile,
  DrmSystemNegotiationResult,
  DrmSystemSelectionStrategyInterface,
} from './drmSystemSelectionStrategyInterface';

@Injectable()
export class FairPlayDrmSystemSelectionStrategy implements DrmSystemSelectionStrategyInterface {
  public readonly handledDrmSystemIdentifier = 'FAIRPLAY';

  /** @inheritdoc */
  public isApplicableToDeviceCapabilityProfile(deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): boolean {
    const platformSupportsFairPlayNatively = ['ios', 'tvos', 'macos'].includes(
      deviceDrmCapabilityProfile.devicePlatformFamily.toLowerCase(),
    );
    return (
      platformSupportsFairPlayNatively &&
      deviceDrmCapabilityProfile.clientSupportedDrmSystemIdentifiers.includes(this.handledDrmSystemIdentifier)
    );
  }

  /** @inheritdoc */
  public negotiateDrmConfigurationForDevice(_deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): DrmSystemNegotiationResult {
    return {
      selectedDrmSystemIdentifier: this.handledDrmSystemIdentifier,
      licenseAcquisitionEndpointUrl: 'https://drm-license.example.com/fairplay/license',
      negotiatedSecurityLevel: 'SL3000',
    };
  }
}
