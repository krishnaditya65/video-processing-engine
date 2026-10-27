// This file wires package drm's real AES-128-CTR media encryption
// (internal/drm/encryption.go) into the actual packaged mdat box bytes this
// package produces, closing the gap the package doc's "simplified/omitted"
// list calls out: "DRM signaling in this service lives entirely in the
// manifest layer... not in the packaged bytes." EncryptMovieFragmentMediaData
// and DecryptMovieFragmentMediaData below make that no longer true for the
// media bytes themselves (CENC auxiliary-info boxes 'senc'/'saiz'/'saio'
// that would let a real player locate/decrypt per-sample encryption ranges
// are still not implemented — see package doc — but the mdat bytes
// underneath are genuinely AES-encrypted, not merely signaled as such).
package fmp4

import (
	"fmt"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
)

// EncryptMovieFragmentMediaData locates the single top-level "mdat" box
// within a serialized moof+mdat movie fragment (as returned by
// BuildMovieFragmentAndMediaDataBoxes or found inside
// PackagedFmp4Chunk.MovieFragment) and encrypts its payload bytes in place
// with AES-128-CTR under the given content key/IV (see package drm's
// EncryptMediaPayloadWithContentKey for the cipher-mode rationale).
//
// Because AES-CTR is a length-preserving stream cipher, the mdat box's
// 32-bit size field and every trun.data_offset computed relative to it
// (both already set by BuildMovieFragmentAndMediaDataBoxes) remain valid
// after encryption — only the mdat payload bytes themselves change. The
// returned slice is a new allocation; movieFragment is not mutated.
func EncryptMovieFragmentMediaData(movieFragment []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error) {
	return transformMdatPayloadInMovieFragment(movieFragment, contentKey, initializationVector, drm.EncryptMediaPayloadWithContentKey)
}

// DecryptMovieFragmentMediaData reverses EncryptMovieFragmentMediaData,
// recovering the original plaintext mdat payload bytes exactly when given
// the same content key/IV used to encrypt. Provided as a distinctly-named
// function (rather than telling callers to just call Encrypt again) purely
// for call-site readability; AES-CTR makes the two operations bit-identical
// (see package drm's DecryptMediaPayloadWithContentKey doc).
func DecryptMovieFragmentMediaData(movieFragment []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error) {
	return transformMdatPayloadInMovieFragment(movieFragment, contentKey, initializationVector, drm.DecryptMediaPayloadWithContentKey)
}

// mdatPayloadTransformFunc abstracts over package drm's Encrypt/Decrypt
// entry points so transformMdatPayloadInMovieFragment has a single
// implementation shared by both directions.
type mdatPayloadTransformFunc func(payload []byte, contentKey [16]byte, initializationVector [16]byte) ([]byte, error)

// transformMdatPayloadInMovieFragment walks movieFragment's top-level boxes
// via WalkTopLevelBoxes, finds the "mdat" box, and replaces its payload
// bytes (leaving its 8-byte size+type header untouched) with the result of
// applying transform to the original payload bytes.
func transformMdatPayloadInMovieFragment(movieFragment []byte, contentKey [16]byte, initializationVector [16]byte, transform mdatPayloadTransformFunc) ([]byte, error) {
	headers, err := WalkTopLevelBoxes(movieFragment)
	if err != nil {
		return nil, fmt.Errorf("fmp4: parsing movie fragment for mdat encryption: %w", err)
	}

	out := append([]byte(nil), movieFragment...)
	foundMdat := false
	for _, header := range headers {
		if header.Type != "mdat" {
			continue
		}
		foundMdat = true
		payloadEnd := header.PayloadOffset + int(header.TotalSize) - isoBmffBoxHeaderSizeInBytes
		transformed, err := transform(out[header.PayloadOffset:payloadEnd], contentKey, initializationVector)
		if err != nil {
			return nil, fmt.Errorf("fmp4: transforming mdat payload: %w", err)
		}
		if len(transformed) != payloadEnd-header.PayloadOffset {
			return nil, fmt.Errorf("fmp4: encryption transform changed mdat payload length (%d -> %d), which would invalidate box size fields and trun.data_offset", payloadEnd-header.PayloadOffset, len(transformed))
		}
		copy(out[header.PayloadOffset:payloadEnd], transformed)
	}
	if !foundMdat {
		return nil, fmt.Errorf("fmp4: no mdat box found in movie fragment")
	}
	return out, nil
}
