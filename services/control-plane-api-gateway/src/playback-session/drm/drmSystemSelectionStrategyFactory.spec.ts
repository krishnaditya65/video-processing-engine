/**
 * @file drmSystemSelectionStrategyFactory.spec.ts
 * Unit tests for {@link DrmSystemSelectionStrategyFactory} covering: correct strategy
 * chosen per platform + client preference, client preference ordering being
 * respected, and fallback to ClearKey when nothing else matches.
 */

import { ClearKeyDrmSystemSelectionStrategy } from './clearKeyDrmSystemSelectionStrategy';
import { DrmSystemSelectionStrategyFactory } from './drmSystemSelectionStrategyFactory';
import { FairPlayDrmSystemSelectionStrategy } from './fairPlayDrmSystemSelectionStrategy';
import { PlayReadyDrmSystemSelectionStrategy } from './playReadyDrmSystemSelectionStrategy';
import { WidevineDrmSystemSelectionStrategy } from './widevineDrmSystemSelectionStrategy';

describe('DrmSystemSelectionStrategyFactory', () => {
  let drmSystemSelectionStrategyFactory: DrmSystemSelectionStrategyFactory;

  beforeEach(() => {
    drmSystemSelectionStrategyFactory = new DrmSystemSelectionStrategyFactory(
      new WidevineDrmSystemSelectionStrategy(),
      new FairPlayDrmSystemSelectionStrategy(),
      new PlayReadyDrmSystemSelectionStrategy(),
      new ClearKeyDrmSystemSelectionStrategy(),
    );
  });

  it('selects Widevine for an Android device that declares Widevine support', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'android',
      clientSupportedDrmSystemIdentifiers: ['WIDEVINE'],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('WIDEVINE');
  });

  it('selects FairPlay for an iOS device that declares FairPlay support', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'ios',
      clientSupportedDrmSystemIdentifiers: ['FAIRPLAY'],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('FAIRPLAY');
  });

  it('selects PlayReady for an Xbox device that declares PlayReady support', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'xbox',
      clientSupportedDrmSystemIdentifiers: ['PLAYREADY'],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('PLAYREADY');
  });

  it('respects the client\'s declared preference order over registration order', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'web',
      clientSupportedDrmSystemIdentifiers: ['PLAYREADY', 'WIDEVINE'],
    });
    // 'web' platform does not natively support PlayReady in this strategy set, so it
    // should be skipped and Widevine (second preference, and applicable) selected.
    expect(result.selectedDrmSystemIdentifier).toBe('WIDEVINE');
  });

  it('falls back to ClearKey when the device declares an unsupported/unrecognised DRM system', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'unknown-embedded-platform',
      clientSupportedDrmSystemIdentifiers: ['SOME_UNKNOWN_DRM_SYSTEM'],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('CLEARKEY');
  });

  it('falls back to ClearKey when the device declares no supported DRM systems at all', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'web',
      clientSupportedDrmSystemIdentifiers: [],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('CLEARKEY');
  });

  it('does not select FairPlay for a non-Apple platform even if the client claims support for it', () => {
    const result = drmSystemSelectionStrategyFactory.selectDrmStrategyForDeviceCapabilityProfile({
      devicePlatformFamily: 'android',
      clientSupportedDrmSystemIdentifiers: ['FAIRPLAY'],
    });
    expect(result.selectedDrmSystemIdentifier).toBe('CLEARKEY');
  });
});
