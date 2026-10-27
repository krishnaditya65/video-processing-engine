/**
 * @file playReadyDrmSystemSelectionStrategy.ts
 * Concrete Strategy-pattern implementation for Microsoft PlayReady DRM, applicable to
 * Xbox, Windows, and many third-party Smart TV/set-top-box platforms.
 */

import { Injectable } from '@nestjs/common';
import {
  DeviceDrmCapabilityProfile,
  DrmSystemNegotiationResult,
  DrmSystemSelectionStrategyInterface,
} from './drmSystemSelectionStrategyInterface';

@Injectable()
export class PlayReadyDrmSystemSelectionStrategy implements DrmSystemSelectionStrategyInterface {
  public readonly handledDrmSystemIdentifier = 'PLAYREADY';

  /** @inheritdoc */
  public isApplicableToDeviceCapabilityProfile(deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): boolean {
    const platformSupportsPlayReadyNatively = ['xbox', 'windows', 'smarttv', 'set-top-box'].includes(
      deviceDrmCapabilityProfile.devicePlatformFamily.toLowerCase(),
    );
    return (
      platformSupportsPlayReadyNatively &&
      deviceDrmCapabilityProfile.clientSupportedDrmSystemIdentifiers.includes(this.handledDrmSystemIdentifier)
    );
  }

  /** @inheritdoc */
  public negotiateDrmConfigurationForDevice(_deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): DrmSystemNegotiationResult {
    return {
      selectedDrmSystemIdentifier: this.handledDrmSystemIdentifier,
      licenseAcquisitionEndpointUrl: 'https://drm-license.example.com/playready/license',
      negotiatedSecurityLevel: 'SL3000',
    };
  }
}
