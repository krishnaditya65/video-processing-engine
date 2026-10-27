package drm

import (
	"bytes"
	"testing"
)

// TestDeriveContentInitializationVectorIsDeterministicAndDistinctFromKey
// verifies the IV derivation is deterministic (same inputs -> same IV,
// the property the whole edge scheme depends on) and that the derived IV
// is not simply a copy of the content key for the same inputs (they must
// be independent outputs of the derivation tree, see encryption.go doc).
func TestDeriveContentInitializationVectorIsDeterministicAndDistinctFromKey(t *testing.T) {
	iv1 := DeriveContentInitializationVectorForAssetAndRotationIndex("asset-42", 3)
	iv2 := DeriveContentInitializationVectorForAssetAndRotationIndex("asset-42", 3)
	if iv1 != iv2 {
		t.Errorf("expected deterministic IV derivation, got %x != %x", iv1, iv2)
	}

	key := DeriveContentKeyForAssetAndRotationIndex("asset-42", 3)
	if bytes.Equal(key[:], iv1[:]) {
		t.Errorf("expected the derived IV to differ from the derived content key, both were %x", key)
	}
}

// TestEncryptDecryptMediaPayloadRoundTrip is the mandatory AES round-trip
// test: encrypting then decrypting with the same content key/IV must
// recover the original plaintext bytes bit-for-bit.
func TestEncryptDecryptMediaPayloadRoundTrip(t *testing.T) {
	plaintext := []byte("this simulates raw H.264/AAC elementary-stream sample bytes fetched from mock object storage for one ~2s chunk")
	key := DeriveContentKeyForAssetAndRotationIndex("stranger-things-s5e1", 2)
	iv := DeriveContentInitializationVectorForAssetAndRotationIndex("stranger-things-s5e1", 2)

	ciphertext, err := EncryptMediaPayloadWithContentKey(plaintext, key, iv)
	if err != nil {
		t.Fatalf("EncryptMediaPayloadWithContentKey returned error: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("expected ciphertext to differ from plaintext")
	}
	if len(ciphertext) != len(plaintext) {
		t.Fatalf("expected AES-CTR to preserve length: plaintext=%d ciphertext=%d", len(plaintext), len(ciphertext))
	}

	recovered, err := DecryptMediaPayloadWithContentKey(ciphertext, key, iv)
	if err != nil {
		t.Fatalf("DecryptMediaPayloadWithContentKey returned error: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Fatalf("round trip failed:\n  original:  %q\n  recovered: %q", plaintext, recovered)
	}
}

// TestEncryptWithWrongKeyDoesNotRecoverPlaintext verifies decryption with a
// different rotation epoch's key/IV does not (with overwhelming
// probability) recover the original plaintext, demonstrating the
// encryption is actually keyed rather than a no-op/identity transform.
func TestEncryptWithWrongKeyDoesNotRecoverPlaintext(t *testing.T) {
	plaintext := bytes.Repeat([]byte("sample-payload-bytes"), 10)
	key := DeriveContentKeyForAssetAndRotationIndex("asset-1", 0)
	iv := DeriveContentInitializationVectorForAssetAndRotationIndex("asset-1", 0)

	ciphertext, err := EncryptMediaPayloadWithContentKey(plaintext, key, iv)
	if err != nil {
		t.Fatalf("encrypt error: %v", err)
	}

	wrongKey := DeriveContentKeyForAssetAndRotationIndex("asset-1", 1)
	wrongIV := DeriveContentInitializationVectorForAssetAndRotationIndex("asset-1", 1)
	recoveredWithWrongKey, err := DecryptMediaPayloadWithContentKey(ciphertext, wrongKey, wrongIV)
	if err != nil {
		t.Fatalf("decrypt error: %v", err)
	}
	if bytes.Equal(recoveredWithWrongKey, plaintext) {
		t.Fatal("expected decrypting with the wrong rotation epoch's key/IV to not recover the original plaintext")
	}
}

// TestEncryptionIsDeterministicGivenSameKeyAndIV verifies encrypting the
// same plaintext twice with the same key/IV produces identical ciphertext,
// consistent with AES-CTR being a deterministic keystream cipher (no random
// nonce is introduced beyond the deterministically-derived IV) — this is
// intentional in this scheme, since any edge node must be able to encrypt
// or decrypt a given chunk identically without coordination.
func TestEncryptionIsDeterministicGivenSameKeyAndIV(t *testing.T) {
	plaintext := []byte("deterministic-edge-encryption-payload")
	key := DeriveContentKeyForAssetAndRotationIndex("asset-7", 5)
	iv := DeriveContentInitializationVectorForAssetAndRotationIndex("asset-7", 5)

	ciphertext1, _ := EncryptMediaPayloadWithContentKey(plaintext, key, iv)
	ciphertext2, _ := EncryptMediaPayloadWithContentKey(plaintext, key, iv)
	if !bytes.Equal(ciphertext1, ciphertext2) {
		t.Fatal("expected identical ciphertext given identical key/IV/plaintext")
	}
}
