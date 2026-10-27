package fmp4

import (
	"bytes"
	"testing"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
)

// TestEncryptDecryptMovieFragmentMediaDataRoundTrip is the mandatory
// end-to-end round trip: package a chunk exactly as the JIT packaging path
// does, encrypt its movie fragment's mdat payload, decrypt it back, and
// verify the full packaged payload (including the untouched ftyp/moov/moof
// bytes) is recovered bit-for-bit identical to the original.
func TestEncryptDecryptMovieFragmentMediaDataRoundTrip(t *testing.T) {
	videoBytes := bytes.Repeat([]byte("VIDEO-ELEMENTARY-STREAM-BYTES--"), 50)
	audioBytes := bytes.Repeat([]byte("AUDIO-BYTES-"), 30)

	packaged, err := PackageElementaryStreamChunkAsFragmentedMp4(3, 90000, 270000, videoBytes, audioBytes)
	if err != nil {
		t.Fatalf("PackageElementaryStreamChunkAsFragmentedMp4 returned error: %v", err)
	}
	originalFullPayload := append([]byte(nil), packaged.FullPayload()...)

	key := drm.DeriveContentKeyForAssetAndRotationIndex("asset-encrypt-test", 0)
	iv := drm.DeriveContentInitializationVectorForAssetAndRotationIndex("asset-encrypt-test", 0)

	encryptedFragment, err := EncryptMovieFragmentMediaData(packaged.MovieFragment, key, iv)
	if err != nil {
		t.Fatalf("EncryptMovieFragmentMediaData returned error: %v", err)
	}
	if bytes.Equal(encryptedFragment, packaged.MovieFragment) {
		t.Fatal("expected the encrypted movie fragment to differ from the original")
	}
	if len(encryptedFragment) != len(packaged.MovieFragment) {
		t.Fatalf("expected encryption to preserve movie fragment length: original=%d encrypted=%d", len(packaged.MovieFragment), len(encryptedFragment))
	}

	// The moof box (everything before mdat) must be byte-identical:
	// encryption only ever touches mdat's payload.
	headers, err := WalkTopLevelBoxes(encryptedFragment)
	if err != nil {
		t.Fatalf("encrypted fragment failed to parse as valid boxes: %v", err)
	}
	if len(headers) != 2 || headers[0].Type != "moof" || headers[1].Type != "mdat" {
		t.Fatalf("expected [moof, mdat] top-level boxes in the encrypted fragment, got %+v", headers)
	}
	moofEnd := headers[0].PayloadOffset + int(headers[0].TotalSize) - isoBmffBoxHeaderSizeInBytes
	if !bytes.Equal(encryptedFragment[:moofEnd], packaged.MovieFragment[:moofEnd]) {
		t.Error("expected the moof box to be byte-identical before and after mdat encryption")
	}

	decryptedFragment, err := DecryptMovieFragmentMediaData(encryptedFragment, key, iv)
	if err != nil {
		t.Fatalf("DecryptMovieFragmentMediaData returned error: %v", err)
	}
	if !bytes.Equal(decryptedFragment, packaged.MovieFragment) {
		t.Fatal("expected decryption to recover the original movie fragment bytes exactly")
	}

	// Reassemble the full response payload (init segment + decrypted
	// fragment) and confirm it matches the original chunk exactly.
	reassembled := append(append([]byte(nil), packaged.InitializationSegment...), decryptedFragment...)
	if !bytes.Equal(reassembled, originalFullPayload) {
		t.Fatal("expected the reassembled full payload to match the original chunk's full payload exactly")
	}

	// Sanity: the plaintext video/audio bytes are actually still recoverable
	// from the decrypted mdat payload (proves the round trip is meaningful,
	// not just "same length").
	if !bytes.Contains(decryptedFragment, videoBytes) {
		t.Error("expected decrypted mdat to contain the original video bytes")
	}
	if !bytes.Contains(decryptedFragment, audioBytes) {
		t.Error("expected decrypted mdat to contain the original audio bytes")
	}
	if bytes.Contains(encryptedFragment, videoBytes) {
		t.Error("expected encrypted mdat to NOT contain the plaintext video bytes")
	}
}

// TestEncryptMovieFragmentMediaDataRejectsMissingMdat verifies a clear
// error (not a panic or silent no-op) when the input has no mdat box.
func TestEncryptMovieFragmentMediaDataRejectsMissingMdat(t *testing.T) {
	moofOnly := BuildFileTypeBox() // any non-mdat top-level box will do
	key := drm.DeriveContentKeyForAssetAndRotationIndex("asset-x", 0)
	iv := drm.DeriveContentInitializationVectorForAssetAndRotationIndex("asset-x", 0)

	if _, err := EncryptMovieFragmentMediaData(moofOnly, key, iv); err == nil {
		t.Error("expected an error when no mdat box is present")
	}
}
