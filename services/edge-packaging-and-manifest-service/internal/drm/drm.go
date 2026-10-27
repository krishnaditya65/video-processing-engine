// Package drm implements the multi-DRM key-stitching abstraction: a
// Strategy-pattern DrmKeyProvider interface with one mock implementation
// per supported system (Widevine, FairPlay, PlayReady), plus a shared,
// deterministic key-rotation scheme every implementation builds on.
//
// # Key rotation scheme
//
// Content keys rotate every RotationPeriodInSegments segments. A segment at
// ChunkSequenceIndex i belongs to rotation index:
//
//	rotationIndex = i / RotationPeriodInSegments   (integer division)
//
// The content key for a given (assetIdentifier, rotationIndex) pair is
// derived deterministically as:
//
//	contentKey = HMAC-SHA256(masterSecret, assetIdentifier || ":" || rotationIndex)[:16]
//
// truncated to 16 bytes (128 bits), matching common CENC content-key
// widths. Determinism means any edge node can independently recompute the
// same key for the same (asset, rotation index) without a round trip to a
// central key server, which is the entire point of doing key derivation at
// the edge; a real deployment would replace the local HMAC master secret
// with a per-tenant secret fetched from a KMS/HSM and cached, but the
// derivation formula and rotation cadence stay the same.
package drm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
)

// RotationPeriodInSegments is N in "rotate the content key every N
// segments": the number of consecutive chunks that share one content key
// before the deterministic derivation formula advances to the next
// rotation index. A smaller N shrinks the blast radius of a leaked key at
// the cost of more key-exchange overhead; this service uses a fixed
// platform-wide value for simplicity, though production systems commonly
// make it per-asset configurable.
const RotationPeriodInSegments = 8

// mockMasterDerivationSecret stands in for a per-tenant secret that in
// production would be sourced from a KMS/HSM and never hard-coded. It
// exists only so this simulation's key derivation is reproducible without
// external dependencies.
var mockMasterDerivationSecret = []byte("edge-packaging-engine-mock-master-secret-v1")

// RotationIndexForChunkSequence computes which rotation epoch a chunk at
// the given zero-based sequence index falls into, per the
// "rotate every N segments" scheme documented at the package level.
func RotationIndexForChunkSequence(chunkSequenceIndex int) int {
	if chunkSequenceIndex < 0 {
		chunkSequenceIndex = 0
	}
	return chunkSequenceIndex / RotationPeriodInSegments
}

// DeriveContentKeyForAssetAndRotationIndex deterministically derives the
// 128-bit content key for (assetIdentifier, rotationIndex) using the
// HMAC-SHA256-based formula documented at the package level. Because the
// derivation is a pure function of its inputs, any edge node fronting the
// same asset independently arrives at the identical key without a central
// key-server round trip.
func DeriveContentKeyForAssetAndRotationIndex(assetIdentifier string, rotationIndex int) [16]byte {
	mac := hmac.New(sha256.New, mockMasterDerivationSecret)
	mac.Write([]byte(fmt.Sprintf("%s:%d", assetIdentifier, rotationIndex)))
	digest := mac.Sum(nil)
	var key [16]byte
	copy(key[:], digest[:16])
	return key
}

// KeyRotationPlanForChunkSequence bundles everything a manifest generator
// or JIT packager needs to signal DRM for one chunk: which rotation epoch
// it belongs to and that epoch's derived content key and initialization
// vector (see encryption.go for how ContentInitializationVector is used to
// actually encrypt packaged fMP4 mdat payload bytes, as opposed to
// ContentKey/RotationIndex which are also used for manifest-layer
// signaling).
type KeyRotationPlanForChunkSequence struct {
	// RotationIndex is the rotation epoch this chunk belongs to.
	RotationIndex int
	// ContentKey is the 128-bit key for RotationIndex, derived per the
	// package-level formula.
	ContentKey [16]byte
	// ContentInitializationVector is the 128-bit AES-CTR initialization
	// vector for RotationIndex, derived per the formula documented on
	// DeriveContentInitializationVectorForAssetAndRotationIndex.
	ContentInitializationVector [16]byte
}

// ResolveKeyRotationPlanForChunk resolves the full rotation plan for a
// given asset/chunk-sequence-index pair in one call.
func ResolveKeyRotationPlanForChunk(assetIdentifier string, chunkSequenceIndex int) KeyRotationPlanForChunkSequence {
	rotationIndex := RotationIndexForChunkSequence(chunkSequenceIndex)
	return KeyRotationPlanForChunkSequence{
		RotationIndex:               rotationIndex,
		ContentKey:                  DeriveContentKeyForAssetAndRotationIndex(assetIdentifier, rotationIndex),
		ContentInitializationVector: DeriveContentInitializationVectorForAssetAndRotationIndex(assetIdentifier, rotationIndex),
	}
}

// HlsKeySignalingTag is the fully-rendered contents of one HLS
// #EXT-X-KEY tag (everything after the "#EXT-X-KEY:" prefix is carried in
// AttributeString; Render includes the prefix).
type HlsKeySignalingTag struct {
	// AttributeString is the comma-separated attribute-list body of the
	// tag, e.g. `METHOD=SAMPLE-AES,KEYFORMAT="com.apple.streamingkeydelivery",URI="skd://...",KEYFORMATVERSIONS="1"`.
	AttributeString string
}

// Render returns the full "#EXT-X-KEY:..." playlist line for this tag.
func (tag HlsKeySignalingTag) Render() string {
	return "#EXT-X-KEY:" + tag.AttributeString
}

// DashContentProtectionElement is the fully-rendered contents of one DASH
// MPD <ContentProtection> element for a single AdaptationSet.
type DashContentProtectionElement struct {
	// XML is the complete `<ContentProtection ...>...</ContentProtection>`
	// (or self-closed `<ContentProtection .../>`) markup for this DRM
	// system.
	XML string
}

// DrmKeyProvider is the Strategy interface every supported multi-DRM
// system implements: given an asset and a chunk sequence index, produce
// the format-specific signaling markup that tells the client player which
// key/license-server to fetch a license from, using the shared,
// deterministic key-rotation scheme for the current rotation epoch's key
// identifier.
//
// Selecting which concrete strategy to use for a given PlaybackContext is
// the caller's responsibility (see NewDrmKeyProviderForSystem), keeping the
// manifest generators themselves DRM-system-agnostic.
type DrmKeyProvider interface {
	// SystemIdentifier returns which domain.DrmSystemIdentifier this
	// provider implements.
	SystemIdentifier() domain.DrmSystemIdentifier
	// GenerateHlsKeySignalingTagForChunk returns the #EXT-X-KEY tag that
	// should precede the given chunk's segment URI in an HLS media
	// playlist.
	GenerateHlsKeySignalingTagForChunk(assetIdentifier string, chunkSequenceIndex int) HlsKeySignalingTag
	// GenerateDashContentProtectionElementForAsset returns the
	// <ContentProtection> element that should appear on the protected
	// AdaptationSet in a DASH MPD. DASH signals the *scheme* at the
	// AdaptationSet level (not per-segment); per-segment key rotation is
	// instead carried by the CENC 'senc'/'pssh' boxes in each segment,
	// which is outside this simulation's scope (see README limitations).
	GenerateDashContentProtectionElementForAsset(assetIdentifier string, rotationIndex int) DashContentProtectionElement
}

// NewDrmKeyProviderForSystem is the Strategy factory: it returns the
// concrete DrmKeyProvider implementation for the requested system, or nil
// (with ok=false) for domain.DrmSystemNone or an unrecognized identifier,
// signaling to the caller that no DRM signaling should be attached.
func NewDrmKeyProviderForSystem(system domain.DrmSystemIdentifier) (provider DrmKeyProvider, ok bool) {
	switch system {
	case domain.DrmSystemWidevine:
		return mockWidevineKeyProvider{}, true
	case domain.DrmSystemFairPlay:
		return mockFairPlayKeyProvider{}, true
	case domain.DrmSystemPlayReady:
		return mockPlayReadyKeyProvider{}, true
	default:
		return nil, false
	}
}

// keyIdentifierHexForChunk renders the derived content key's rotation
// index into a stable hex key-identifier string, used across all three
// mock providers as the license-server-addressable key ID.
func keyIdentifierHexForChunk(assetIdentifier string, chunkSequenceIndex int) (keyIDHex string, plan KeyRotationPlanForChunkSequence) {
	plan = ResolveKeyRotationPlanForChunk(assetIdentifier, chunkSequenceIndex)
	return hex.EncodeToString(plan.ContentKey[:]), plan
}

// mockWidevineKeyProvider is the mock Widevine implementation of
// DrmKeyProvider. It is "mock" in that it does not call a real Widevine
// license server or produce a cryptographically valid PSSH box; it
// produces structurally-correct signaling (URIs, key IDs, scheme URNs)
// using the shared deterministic key derivation so tests can assert on
// per-format correctness without external services.
type mockWidevineKeyProvider struct{}

// widevineCommonSystemID is the standardized DRM system ID for Widevine as
// used in CENC 'pssh' boxes and DASH ContentProtection schemeIdUri.
const widevineCommonSystemID = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"

func (mockWidevineKeyProvider) SystemIdentifier() domain.DrmSystemIdentifier {
	return domain.DrmSystemWidevine
}

func (mockWidevineKeyProvider) GenerateHlsKeySignalingTagForChunk(assetIdentifier string, chunkSequenceIndex int) HlsKeySignalingTag {
	keyIDHex, plan := keyIdentifierHexForChunk(assetIdentifier, chunkSequenceIndex)
	attrs := fmt.Sprintf(
		`METHOD=SAMPLE-AES-CTR,KEYFORMAT="urn:uuid:%s",KEYFORMATVERSIONS="1",URI="https://license.edge-packaging.example/widevine/%s?kid=%s",KEYID=0x%s`,
		widevineCommonSystemID, assetIdentifier, keyIDHex, keyIDHex,
	)
	_ = plan
	return HlsKeySignalingTag{AttributeString: attrs}
}

func (mockWidevineKeyProvider) GenerateDashContentProtectionElementForAsset(assetIdentifier string, rotationIndex int) DashContentProtectionElement {
	key := DeriveContentKeyForAssetAndRotationIndex(assetIdentifier, rotationIndex)
	keyIDHex := hex.EncodeToString(key[:])
	xml := fmt.Sprintf(
		`<ContentProtection schemeIdUri="urn:uuid:%s" value="Widevine" cenc:default_KID="%s"><widevine:license_url>https://license.edge-packaging.example/widevine/%s</widevine:license_url></ContentProtection>`,
		widevineCommonSystemID, formatKeyIDAsUUID(keyIDHex), assetIdentifier,
	)
	return DashContentProtectionElement{XML: xml}
}

// mockFairPlayKeyProvider is the mock Apple FairPlay Streaming
// implementation of DrmKeyProvider, targeting HLS (FairPlay's only
// supported delivery format) with an `skd://` key URI and the
// "com.apple.streamingkeydelivery" key format Apple's spec requires.
type mockFairPlayKeyProvider struct{}

func (mockFairPlayKeyProvider) SystemIdentifier() domain.DrmSystemIdentifier {
	return domain.DrmSystemFairPlay
}

func (mockFairPlayKeyProvider) GenerateHlsKeySignalingTagForChunk(assetIdentifier string, chunkSequenceIndex int) HlsKeySignalingTag {
	keyIDHex, _ := keyIdentifierHexForChunk(assetIdentifier, chunkSequenceIndex)
	attrs := fmt.Sprintf(
		`METHOD=SAMPLE-AES,KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1",URI="skd://license.edge-packaging.example/fairplay/%s?kid=%s"`,
		assetIdentifier, keyIDHex,
	)
	return HlsKeySignalingTag{AttributeString: attrs}
}

func (mockFairPlayKeyProvider) GenerateDashContentProtectionElementForAsset(assetIdentifier string, rotationIndex int) DashContentProtectionElement {
	// FairPlay Streaming is HLS-only in real deployments; DASH support
	// is included here only so the Strategy interface is uniform and
	// callers can render a placeholder rather than special-casing
	// FairPlay out of DASH manifest generation entirely.
	key := DeriveContentKeyForAssetAndRotationIndex(assetIdentifier, rotationIndex)
	keyIDHex := hex.EncodeToString(key[:])
	xml := fmt.Sprintf(
		`<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="%s"><!-- FairPlay Streaming does not define a DASH ContentProtection scheme; included for interface uniformity only --></ContentProtection>`,
		formatKeyIDAsUUID(keyIDHex),
	)
	return DashContentProtectionElement{XML: xml}
}

// mockPlayReadyKeyProvider is the mock Microsoft PlayReady implementation
// of DrmKeyProvider.
type mockPlayReadyKeyProvider struct{}

// playReadyCommonSystemID is the standardized DRM system ID for PlayReady
// as used in CENC 'pssh' boxes and DASH ContentProtection schemeIdUri.
const playReadyCommonSystemID = "9a04f079-9840-4286-ab92-e65be0885f95"

func (mockPlayReadyKeyProvider) SystemIdentifier() domain.DrmSystemIdentifier {
	return domain.DrmSystemPlayReady
}

func (mockPlayReadyKeyProvider) GenerateHlsKeySignalingTagForChunk(assetIdentifier string, chunkSequenceIndex int) HlsKeySignalingTag {
	keyIDHex, _ := keyIdentifierHexForChunk(assetIdentifier, chunkSequenceIndex)
	attrs := fmt.Sprintf(
		`METHOD=SAMPLE-AES,KEYFORMAT="com.microsoft.playready",KEYFORMATVERSIONS="1",URI="https://license.edge-packaging.example/playready/%s?kid=%s"`,
		assetIdentifier, keyIDHex,
	)
	return HlsKeySignalingTag{AttributeString: attrs}
}

func (mockPlayReadyKeyProvider) GenerateDashContentProtectionElementForAsset(assetIdentifier string, rotationIndex int) DashContentProtectionElement {
	key := DeriveContentKeyForAssetAndRotationIndex(assetIdentifier, rotationIndex)
	keyIDHex := hex.EncodeToString(key[:])
	xml := fmt.Sprintf(
		`<ContentProtection schemeIdUri="urn:uuid:%s" value="PlayReady" cenc:default_KID="%s"><mspr:pro>base64-PRO-placeholder-for-%s</mspr:pro></ContentProtection>`,
		playReadyCommonSystemID, formatKeyIDAsUUID(keyIDHex), assetIdentifier,
	)
	return DashContentProtectionElement{XML: xml}
}

// formatKeyIDAsUUID reformats a 32-hex-character key identifier into the
// canonical 8-4-4-4-12 UUID string form DASH's cenc:default_KID attribute
// and CENC PSSH boxes conventionally use.
func formatKeyIDAsUUID(keyIDHex string) string {
	if len(keyIDHex) != 32 {
		return keyIDHex
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", keyIDHex[0:8], keyIDHex[8:12], keyIDHex[12:16], keyIDHex[16:20], keyIDHex[20:32])
}
