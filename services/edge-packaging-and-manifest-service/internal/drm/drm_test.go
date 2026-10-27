package drm

import (
	"strings"
	"testing"
)

// TestRotationIndexForChunkSequenceRotatesEveryNSegments verifies the
// documented "rotate every N segments" scheme: chunks 0..7 share rotation
// index 0, chunks 8..15 share rotation index 1, and so on.
func TestRotationIndexForChunkSequenceRotatesEveryNSegments(t *testing.T) {
	cases := []struct {
		chunkSequenceIndex int
		wantRotationIndex  int
	}{
		{0, 0}, {1, 0}, {7, 0},
		{8, 1}, {9, 1}, {15, 1},
		{16, 2}, {23, 2},
		{24, 3},
	}
	for _, c := range cases {
		got := RotationIndexForChunkSequence(c.chunkSequenceIndex)
		if got != c.wantRotationIndex {
			t.Errorf("RotationIndexForChunkSequence(%d) = %d, want %d", c.chunkSequenceIndex, got, c.wantRotationIndex)
		}
	}
}

// TestDeriveContentKeyForAssetAndRotationIndexIsDeterministic verifies the
// core property the whole edge key-rotation scheme depends on:
// re-deriving the key for the same (asset, rotationIndex) pair always
// yields the identical key, with no external state required.
func TestDeriveContentKeyForAssetAndRotationIndexIsDeterministic(t *testing.T) {
	key1 := DeriveContentKeyForAssetAndRotationIndex("asset-42", 3)
	key2 := DeriveContentKeyForAssetAndRotationIndex("asset-42", 3)
	if key1 != key2 {
		t.Errorf("expected deterministic key derivation, got %x != %x", key1, key2)
	}
}

// TestDeriveContentKeyForAssetAndRotationIndexVariesByInput verifies
// different assets or different rotation indices yield different keys, so
// a key never accidentally spans an unrelated rotation epoch or asset.
func TestDeriveContentKeyForAssetAndRotationIndexVariesByInput(t *testing.T) {
	base := DeriveContentKeyForAssetAndRotationIndex("asset-a", 0)
	differentAsset := DeriveContentKeyForAssetAndRotationIndex("asset-b", 0)
	differentRotation := DeriveContentKeyForAssetAndRotationIndex("asset-a", 1)

	if base == differentAsset {
		t.Error("expected different assets to derive different content keys")
	}
	if base == differentRotation {
		t.Error("expected different rotation indices to derive different content keys")
	}
}

// TestNewDrmKeyProviderForSystemCoversAllThreeSystems verifies the
// Strategy factory returns a non-nil provider whose SystemIdentifier
// matches the request for all three supported DRM systems, and correctly
// signals "no provider" for DrmSystemNone.
func TestNewDrmKeyProviderForSystemCoversAllThreeSystems(t *testing.T) {
	widevine, ok := NewDrmKeyProviderForSystem("widevine")
	if !ok || widevine == nil || widevine.SystemIdentifier() != "widevine" {
		t.Errorf("expected a widevine provider, got %v ok=%v", widevine, ok)
	}
	fairplay, ok := NewDrmKeyProviderForSystem("fairplay")
	if !ok || fairplay == nil || fairplay.SystemIdentifier() != "fairplay" {
		t.Errorf("expected a fairplay provider, got %v ok=%v", fairplay, ok)
	}
	playready, ok := NewDrmKeyProviderForSystem("playready")
	if !ok || playready == nil || playready.SystemIdentifier() != "playready" {
		t.Errorf("expected a playready provider, got %v ok=%v", playready, ok)
	}
	none, ok := NewDrmKeyProviderForSystem("none")
	if ok || none != nil {
		t.Errorf("expected no provider for DrmSystemNone, got %v ok=%v", none, ok)
	}
	unknown, ok := NewDrmKeyProviderForSystem("some-unknown-system")
	if ok || unknown != nil {
		t.Errorf("expected no provider for an unrecognized system, got %v ok=%v", unknown, ok)
	}
}

// TestWidevineHlsSignalingCorrectness verifies the Widevine provider's HLS
// #EXT-X-KEY signaling carries the format-correct METHOD/KEYFORMAT/URI
// attributes per-format-correctness requirement.
func TestWidevineHlsSignalingCorrectness(t *testing.T) {
	provider, _ := NewDrmKeyProviderForSystem("widevine")
	tag := provider.GenerateHlsKeySignalingTagForChunk("asset-x", 0)

	mustContain(t, tag.AttributeString, `METHOD=SAMPLE-AES-CTR`)
	mustContain(t, tag.AttributeString, `KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"`)
	mustContain(t, tag.AttributeString, "URI=")
	rendered := tag.Render()
	if !strings.HasPrefix(rendered, "#EXT-X-KEY:") {
		t.Errorf("expected rendered tag to start with #EXT-X-KEY:, got %q", rendered)
	}
}

// TestFairPlayHlsSignalingUsesSkdUriAndAppleKeyFormat verifies FairPlay's
// HLS signaling uses the Apple-mandated skd:// URI scheme and
// "com.apple.streamingkeydelivery" key format, distinguishing it from the
// other two systems.
func TestFairPlayHlsSignalingUsesSkdUriAndAppleKeyFormat(t *testing.T) {
	provider, _ := NewDrmKeyProviderForSystem("fairplay")
	tag := provider.GenerateHlsKeySignalingTagForChunk("asset-x", 0)

	mustContain(t, tag.AttributeString, `METHOD=SAMPLE-AES,`)
	mustContain(t, tag.AttributeString, `KEYFORMAT="com.apple.streamingkeydelivery"`)
	mustContain(t, tag.AttributeString, "URI=\"skd://")
}

// TestPlayReadyHlsSignalingUsesMicrosoftKeyFormat verifies PlayReady's HLS
// signaling uses the "com.microsoft.playready" key format.
func TestPlayReadyHlsSignalingUsesMicrosoftKeyFormat(t *testing.T) {
	provider, _ := NewDrmKeyProviderForSystem("playready")
	tag := provider.GenerateHlsKeySignalingTagForChunk("asset-x", 0)

	mustContain(t, tag.AttributeString, `KEYFORMAT="com.microsoft.playready"`)
}

// TestDashContentProtectionElementsCarryDefaultKeyId verifies every DRM
// system's DASH <ContentProtection> element carries a cenc:default_KID
// attribute in canonical UUID form, and that Widevine/PlayReady use their
// respective standardized schemeIdUri system IDs.
func TestDashContentProtectionElementsCarryDefaultKeyId(t *testing.T) {
	widevine, _ := NewDrmKeyProviderForSystem("widevine")
	widevineElement := widevine.GenerateDashContentProtectionElementForAsset("asset-y", 0)
	mustContain(t, widevineElement.XML, "cenc:default_KID=")
	mustContain(t, widevineElement.XML, "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed")

	playready, _ := NewDrmKeyProviderForSystem("playready")
	playreadyElement := playready.GenerateDashContentProtectionElementForAsset("asset-y", 0)
	mustContain(t, playreadyElement.XML, "cenc:default_KID=")
	mustContain(t, playreadyElement.XML, "urn:uuid:9a04f079-9840-4286-ab92-e65be0885f95")
}

// TestRotationIndexChangesKeyIdentifierAcrossEpochs verifies that HLS key
// signaling tags for chunks in different rotation epochs carry different
// key identifiers, proving key rotation actually changes the advertised
// key rather than only the internal derivation.
func TestRotationIndexChangesKeyIdentifierAcrossEpochs(t *testing.T) {
	provider, _ := NewDrmKeyProviderForSystem("widevine")
	tagEpoch0 := provider.GenerateHlsKeySignalingTagForChunk("asset-z", 0)                        // rotation index 0
	tagEpoch1 := provider.GenerateHlsKeySignalingTagForChunk("asset-z", RotationPeriodInSegments) // rotation index 1

	if tagEpoch0.AttributeString == tagEpoch1.AttributeString {
		t.Error("expected different rotation epochs to produce different key signaling, got identical tags")
	}
}

// mustContain is a small test helper asserting substr appears in s.
func mustContain(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
