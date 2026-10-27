/**
 * @file clearKeyDrmSystemSelectionStrategy.ts
 * Fallback Strategy-pattern implementation using the (unencrypted-key-exchange)
 * ClearKey DRM system. Used for local development/testing or devices that declare no
 * supported commercial DRM system, so playback session creation never hard-fails
 * purely due to DRM negotiation — this strategy is intentionally always applicable as
 * the factory's last-resort fallback (see {@link DrmSystemSelectionStrategyFactory}).
 *
 * NOT suitable for protecting premium content in production — ClearKey provides no
 * real content protection. Called out explicitly in SERVICE_README.md's "known
 * limitations / simulated" section.
 */

import { Injectable } from '@nestjs/common';
import {
  DeviceDrmCapabilityProfile,
  DrmSystemNegotiationResult,
  DrmSystemSelectionStrategyInterface,
} from './drmSystemSelectionStrategyInterface';

@Injectable()
export class ClearKeyDrmSystemSelectionStrategy implements DrmSystemSelectionStrategyInterface {
  public readonly handledDrmSystemIdentifier = 'CLEARKEY';

  /** @inheritdoc Always applicable — this is the universal fallback strategy. */
  public isApplicableToDeviceCapabilityProfile(_deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): boolean {
    return true;
  }

  /** @inheritdoc */
  public negotiateDrmConfigurationForDevice(_deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile): DrmSystemNegotiationResult {
    return {
      selectedDrmSystemIdentifier: this.handledDrmSystemIdentifier,
      licenseAcquisitionEndpointUrl: 'https://drm-license.example.com/clearkey/license',
      negotiatedSecurityLevel: 'NONE',
    };
  }
}
