/**
 * @file drmSystemSelectionStrategyFactory.ts
 * Factory that selects the appropriate {@link DrmSystemSelectionStrategyInterface}
 * implementation for a given device's capability profile, respecting the client's
 * own declared preference order and falling back to {@link ClearKeyDrmSystemSelectionStrategy}
 * when no commercial DRM system matches (Strategy + Factory patterns working
 * together: the factory chooses among interchangeable strategies at runtime).
 */

import { Injectable } from '@nestjs/common';
import { ClearKeyDrmSystemSelectionStrategy } from './clearKeyDrmSystemSelectionStrategy';
import { FairPlayDrmSystemSelectionStrategy } from './fairPlayDrmSystemSelectionStrategy';
import { PlayReadyDrmSystemSelectionStrategy } from './playReadyDrmSystemSelectionStrategy';
import { WidevineDrmSystemSelectionStrategy } from './widevineDrmSystemSelectionStrategy';
import {
  DeviceDrmCapabilityProfile,
  DrmSystemNegotiationResult,
  DrmSystemSelectionStrategyInterface,
} from './drmSystemSelectionStrategyInterface';

@Injectable()
export class DrmSystemSelectionStrategyFactory {
  /**
   * All registered strategies, ordered by the platform's general preference (most
   * specific/robust DRM systems first); the client's own
   * `clientSupportedDrmSystemIdentifiers` order is still respected first (see
   * {@link selectDrmStrategyForDeviceCapabilityProfile}), this ordering only matters
   * as the tie-breaker when the client expresses no preference of its own.
   */
  private readonly registeredStrategies: DrmSystemSelectionStrategyInterface[];

  public constructor(
    widevineDrmSystemSelectionStrategy: WidevineDrmSystemSelectionStrategy,
    fairPlayDrmSystemSelectionStrategy: FairPlayDrmSystemSelectionStrategy,
    playReadyDrmSystemSelectionStrategy: PlayReadyDrmSystemSelectionStrategy,
    private readonly clearKeyDrmSystemSelectionStrategy: ClearKeyDrmSystemSelectionStrategy,
  ) {
    this.registeredStrategies = [
      widevineDrmSystemSelectionStrategy,
      fairPlayDrmSystemSelectionStrategy,
      playReadyDrmSystemSelectionStrategy,
    ];
  }

  /**
   * Selects and applies the best-matching DRM strategy for a device, preferring
   * whichever applicable strategy appears earliest in the *client's own* declared
   * preference order (`clientSupportedDrmSystemIdentifiers`), and falling back to
   * ClearKey (always applicable) if no commercial strategy matches.
   *
   * Edge cases:
   * - Client declares support for a DRM system this platform family doesn't
   *   naturally support (e.g. FairPlay on an Android device): that strategy reports
   *   itself not applicable, so it is skipped.
   * - Client declares an unrecognised/empty `clientSupportedDrmSystemIdentifiers`:
   *   falls through to ClearKey rather than throwing, so session creation always
   *   succeeds with *some* DRM configuration.
   *
   * @param deviceDrmCapabilityProfile - The requesting device's capability hints.
   * @returns The negotiated {@link DrmSystemNegotiationResult} from the selected strategy.
   */
  public selectDrmStrategyForDeviceCapabilityProfile(
    deviceDrmCapabilityProfile: DeviceDrmCapabilityProfile,
  ): DrmSystemNegotiationResult {
    for (const clientPreferredDrmSystemIdentifier of deviceDrmCapabilityProfile.clientSupportedDrmSystemIdentifiers) {
      const matchingStrategy = this.registeredStrategies.find(
        (strategy) =>
          strategy.handledDrmSystemIdentifier === clientPreferredDrmSystemIdentifier &&
          strategy.isApplicableToDeviceCapabilityProfile(deviceDrmCapabilityProfile),
      );
      if (matchingStrategy) {
        return matchingStrategy.negotiateDrmConfigurationForDevice(deviceDrmCapabilityProfile);
      }
    }

    // No client-declared preference matched an applicable commercial strategy: fall
    // back to the universally-applicable ClearKey strategy.
    return this.clearKeyDrmSystemSelectionStrategy.negotiateDrmConfigurationForDevice(deviceDrmCapabilityProfile);
  }
}
