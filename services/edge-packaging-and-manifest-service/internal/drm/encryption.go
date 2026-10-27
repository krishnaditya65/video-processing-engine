// This file implements real AES-128 content encryption of packaged fMP4
// media-data bytes, layered on top of the deterministic key-rotation scheme
// documented at the top of drm.go.
//
// # What this adds beyond manifest-layer DRM signaling
//
// Everything else in this package (drm.go) only produces manifest
// *signaling* — #EXT-X-KEY tags and <ContentProtection> elements telling a
// player which license server/key ID to use. It never touches a single
// media byte. This file closes that gap: it actually encrypts (and
// decrypts) the raw sample payload bytes that package fmp4 packs into an
// mdat box, using the same deterministic per-(asset, rotation-index)
// content key ResolveKeyRotationPlanForChunk already derives for manifest
// signaling, so a key ID rendered in a manifest and the key that actually
// protects that chunk's bytes are always the same key by construction.
//
// # Cipher mode: AES-128-CTR, not AES-128-CBC
//
// This package uses AES in CTR (Counter) mode rather than CBC for one
// deciding reason: CTR is a stream cipher (XOR against a keystream), so
// ciphertext length always exactly equals plaintext length. CBC requires
// padding to the 16-byte AES block size, which would change the byte length
// of the mdat payload and therefore invalidate the mdat box's already-set
// 32-bit size field and every trun.data_offset computed relative to it
// (see package fmp4's box-layout invariants, verified byte-exact by
// internal/fmp4/fmp4_test.go). CTR mode preserves those invariants for
// free. This mirrors real-world practice: HLS SAMPLE-AES-CTR and CENC
// 'cenc' scheme (as opposed to 'cbcs') both standardize on AES-CTR for
// exactly this reason, and this package's DRM signaling already advertises
// METHOD=SAMPLE-AES-CTR (see mockWidevineKeyProvider) even though, before
// this file, no CTR encryption was actually performed on the packaged
// bytes.
//
// # Initialization vector derivation
//
// The IV is derived with the same HMAC-SHA256-over-a-master-secret
// construction as the content key, using a distinct label suffix ("iv"
// instead of no suffix / the bare "asset:rotationIndex" string used for the
// key) so the key and IV are cryptographically independent outputs of the
// same deterministic derivation tree, not the same 16 bytes reused for two
// purposes. Determinism is preserved end to end: any edge node can
// independently recompute the identical (key, IV) pair for a given
// (asset, rotation index) without any additional key-exchange round trip,
// exactly like ContentKey derivation already does.
package drm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
)

// DeriveContentInitializationVectorForAssetAndRotationIndex deterministically
// derives the 128-bit AES-CTR initialization vector for
// (assetIdentifier, rotationIndex), using the same master-secret-keyed HMAC
// construction as DeriveContentKeyForAssetAndRotationIndex but with an
// "iv"-suffixed label, so the derived key and IV are independent outputs
// rather than the same bytes reused for two purposes.
func DeriveContentInitializationVectorForAssetAndRotationIndex(assetIdentifier string, rotationIndex int) [16]byte {
	mac := hmac.New(sha256.New, mockMasterDerivationSecret)
	mac.Write([]byte(fmt.Sprintf("%s:%d:iv", assetIdentifier, rotationIndex)))
	digest := mac.Sum(nil)
	var iv [16]byte
	copy(iv[:], digest[:16])
	return iv
}

// TransformMediaPayloadWithAesCtr encrypts or decrypts payload in place
// against the AES-128-CTR keystream derived from contentKey and
// initializationVector, returning a newly-allocated byte slice (payload
// itself is never mutated). Because AES-CTR is a symmetric XOR-keystream
// cipher, calling this function twice in a row with the same key/IV on its
// own output recovers the original bytes exactly — encryption and
// decryption are literally the same operation. EncryptMediaPayloadWithContentKey
// and DecryptMediaPayloadWithContentKey below are distinctly-named wrappers
// around this function purely so call sites read naturally; there is no
// functional difference between them.
func TransformMediaPayloadWithAesCtr(payload []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error) {
	block, err := aes.NewCipher(contentKey[:])
	if err != nil {
		return nil, fmt.Errorf("drm: constructing AES cipher: %w", err)
	}
	stream := cipher.NewCTR(block, initializationVector[:])
	out := make([]byte, len(payload))
	stream.XORKeyStream(out, payload)
	return out, nil
}

// EncryptMediaPayloadWithContentKey encrypts plaintext media payload bytes
// (e.g. the sample bytes package fmp4 packs into an mdat box) with
// AES-128-CTR under the given content key/IV, per the mode-choice rationale
// documented at the top of this file.
func EncryptMediaPayloadWithContentKey(plaintext []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error) {
	return TransformMediaPayloadWithAesCtr(plaintext, contentKey, initializationVector)
}

// DecryptMediaPayloadWithContentKey decrypts ciphertext media payload bytes
// previously produced by EncryptMediaPayloadWithContentKey with the same
// content key/IV, recovering the original plaintext bytes exactly (see
// TransformMediaPayloadWithAesCtr: AES-CTR decryption is bit-identical to
// encryption).
func DecryptMediaPayloadWithContentKey(ciphertext []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error) {
	return TransformMediaPayloadWithAesCtr(ciphertext, contentKey, initializationVector)
}
