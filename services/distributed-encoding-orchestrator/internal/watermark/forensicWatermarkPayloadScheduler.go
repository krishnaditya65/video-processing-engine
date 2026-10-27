// Package watermark implements a deterministic, per-user A/B forensic
// watermarking payload scheduler.
//
// # Concept
//
// Forensic (session-based) watermarking works by producing two
// perceptually-near-identical encodes of every chunk of a video, "Variant
// A" and "Variant B", each carrying an imperceptible but robust marker
// baked in at encode time (that part — the actual pixel-level marking —
// is out of scope for this package and belongs to the encode pipeline).
// For a given playback session, the CDN/packager is instructed, chunk by
// chunk, to splice in either the A or the B variant. The resulting
// sequence of A/B choices across all chunks of the asset is itself a
// binary signal: read the sequence back after leaked content is
// recovered, and it decodes to a payload — typically a userId, sessionId,
// or account fingerprint — identifying exactly who redistributed the
// content.
//
// This package deterministically derives that A/B sequence from a
// (userId, sessionId) pair using a documented, seedable hash-based
// pseudo-random number generator so that:
//
//  1. The SAME (userId, sessionId) always produces the SAME A/B sequence
//     — required so that re-requesting the manifest, or resuming
//     playback, doesn't scramble the embedded signature.
//  2. Different users are (with overwhelming probability) assigned
//     different sequences, so the sequence is a usable fingerprint.
//  3. The mapping is invertible: given a recovered bit sequence, this
//     package can reconstruct/verify the payload that produced it.
//
// # PRNG construction (documented, not "magic")
//
// The generator is a keyed hash-chain PRNG built from SHA-256:
//
//	state_0        = SHA256(userId || 0x1F || sessionId || 0x1F || "dewm-v1")
//	state_{i+1}     = SHA256(state_i)
//	bit_i (i>=0)   = LSB of first 8 bytes of state_i, taken modulo 2
//
// i.e. a standard hash-chain / counter-mode-style deterministic stream:
// each 32-byte state block deterministically produces the next state
// block and yields one selection bit, decided by parity of a
// SHA-256-derived 64-bit integer. SHA-256 is used purely as a
// deterministic, well-diffused pseudo-random function here — NOT for any
// cryptographic secrecy property — so this scheduler is NOT intended to
// resist an adversary who can observe many (userId, sessionId, sequence)
// triples and attempt to correlate them; it exists to make forensic
// tracing deterministic and independently recomputable by the
// orchestrator without persisting the full per-chunk decision table.
package watermark

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// VariantSelection identifies which of the two forensically-marked
// encodes should be spliced into a manifest at a given chunk index.
type VariantSelection int

const (
	// VariantA denotes the first forensic encode variant, conventionally
	// interpreted as the binary digit 0 when the sequence is later
	// decoded as a payload.
	VariantA VariantSelection = iota
	// VariantB denotes the second forensic encode variant, conventionally
	// interpreted as the binary digit 1.
	VariantB
)

// String renders a VariantSelection as "A" or "B" for logs and manifests.
func (selection VariantSelection) String() string {
	if selection == VariantA {
		return "A"
	}
	return "B"
}

// hashChainDomainSeparationSuffix is appended to the seed material so
// that this PRNG's output space is domain-separated from any other use
// of SHA-256 elsewhere in the system that might otherwise coincidentally
// share the same (userId, sessionId) input bytes.
const hashChainDomainSeparationSuffix = "dewm-v1"

// deterministicHashChainPseudoRandomBitGenerator produces an unbounded,
// deterministic stream of pseudo-random bits derived from a fixed seed by
// repeatedly re-hashing a running 32-byte state with SHA-256 (see package
// doc for the exact construction). Two generators constructed with the
// same seed always produce the same bit at the same index.
//
// Concurrency/safety: NOT safe for concurrent use — callers must not call
// nextPseudoRandomBit concurrently on the same instance, since it mutates
// internal state. Create one instance per goroutine/sequence-generation
// call; instances are cheap (32 bytes of state) to construct.
type deterministicHashChainPseudoRandomBitGenerator struct {
	currentStateDigest [sha256.Size]byte
	hasEmittedFirstBit bool
}

// newDeterministicHashChainPseudoRandomBitGenerator constructs a
// generator seeded from userIdentifier and sessionIdentifier as described
// in the package documentation's PRNG construction section.
func newDeterministicHashChainPseudoRandomBitGenerator(userIdentifier string, sessionIdentifier string) *deterministicHashChainPseudoRandomBitGenerator {
	seedMaterial := make([]byte, 0, len(userIdentifier)+len(sessionIdentifier)+len(hashChainDomainSeparationSuffix)+2)
	seedMaterial = append(seedMaterial, userIdentifier...)
	seedMaterial = append(seedMaterial, 0x1F)
	seedMaterial = append(seedMaterial, sessionIdentifier...)
	seedMaterial = append(seedMaterial, 0x1F)
	seedMaterial = append(seedMaterial, hashChainDomainSeparationSuffix...)

	return &deterministicHashChainPseudoRandomBitGenerator{
		currentStateDigest: sha256.Sum256(seedMaterial),
	}
}

// nextPseudoRandomBit advances the hash chain by one step (except on the
// very first call, which consumes the initial seed state directly) and
// returns the next deterministic pseudo-random bit, derived from the
// parity of the first 8 bytes of the current state digest interpreted as
// a big-endian uint64.
func (generator *deterministicHashChainPseudoRandomBitGenerator) nextPseudoRandomBit() int {
	if generator.hasEmittedFirstBit {
		generator.currentStateDigest = sha256.Sum256(generator.currentStateDigest[:])
	}
	generator.hasEmittedFirstBit = true

	first8Bytes := generator.currentStateDigest[:8]
	asUint64 := binary.BigEndian.Uint64(first8Bytes)
	return int(asUint64 & 1)
}

// GenerateDeterministicVariantSequenceForSession computes the ordered
// per-chunk A/B variant assignment for a single playback session, one
// entry per chunk from index 0 to totalChunkCount-1 inclusive.
//
// Params:
//   - userIdentifier: the platform userId the forensic mark should trace
//     back to (must be non-empty for a meaningful fingerprint, but this
//     function does not itself enforce that).
//   - sessionIdentifier: the specific playback sessionId, mixed into the
//     seed so that a compromised single session cannot be used to derive
//     the sequence used by that user's other sessions.
//   - totalChunkCount: number of chunks in the asset; must be >= 0. A
//     value of 0 returns an empty, non-nil slice.
//
// Returns the deterministic sequence of VariantSelection values, index i
// corresponding to chunk i.
//
// Determinism: calling this function twice with identical arguments
// always returns an identical sequence — this is a pure function with no
// shared state, so it is safe to call concurrently from multiple
// goroutines (each call constructs its own private generator instance).
func GenerateDeterministicVariantSequenceForSession(userIdentifier string, sessionIdentifier string, totalChunkCount int) []VariantSelection {
	if totalChunkCount <= 0 {
		return []VariantSelection{}
	}

	generator := newDeterministicHashChainPseudoRandomBitGenerator(userIdentifier, sessionIdentifier)
	sequence := make([]VariantSelection, totalChunkCount)
	for chunkIndex := 0; chunkIndex < totalChunkCount; chunkIndex++ {
		if generator.nextPseudoRandomBit() == 0 {
			sequence[chunkIndex] = VariantA
		} else {
			sequence[chunkIndex] = VariantB
		}
	}
	return sequence
}

// VerifyRecoveredVariantSequenceMatchesUserSession recomputes the
// expected deterministic A/B sequence for the supplied
// (userIdentifier, sessionIdentifier) pair and reports whether it
// matches recoveredSequence bit-for-bit. This is the "decode/verify"
// counterpart used once pirated content has been recovered and its
// embedded A/B pattern extracted: the anti-piracy team tests candidate
// (userId, sessionId) pairs (typically narrowed down via other logs, such
// as which sessions streamed the asset in the relevant time window)
// against the recovered sequence to confirm which session leaked it.
//
// Params:
//   - userIdentifier, sessionIdentifier: the candidate session to test.
//   - recoveredSequence: the A/B pattern extracted from the leaked
//     content, one entry per chunk starting at chunk 0. May be shorter
//     than the asset's full chunk count (e.g. only a fragment was
//     recovered) — this function only requires a matching PREFIX, since a
//     forensic investigator rarely recovers every chunk.
//
// Returns true if recoveredSequence is a prefix-match against the
// deterministic sequence this (userIdentifier, sessionIdentifier) pair
// would have produced; false otherwise, including when recoveredSequence
// is empty (an empty recovered sequence carries no evidence and cannot
// verify anything) or longer than would be meaningful.
//
// This function is pure and safe for concurrent use.
func VerifyRecoveredVariantSequenceMatchesUserSession(userIdentifier string, sessionIdentifier string, recoveredSequence []VariantSelection) bool {
	if len(recoveredSequence) == 0 {
		return false
	}

	generator := newDeterministicHashChainPseudoRandomBitGenerator(userIdentifier, sessionIdentifier)
	for chunkIndex := 0; chunkIndex < len(recoveredSequence); chunkIndex++ {
		expectedBit := generator.nextPseudoRandomBit()
		expectedVariant := VariantA
		if expectedBit == 1 {
			expectedVariant = VariantB
		}
		if recoveredSequence[chunkIndex] != expectedVariant {
			return false
		}
	}
	return true
}

// IdentifyMatchingSessionAmongCandidates searches candidateSessions for
// the (userIdentifier, sessionIdentifier) pair whose deterministic
// sequence matches recoveredSequence as a prefix, returning its index in
// candidateSessions, or -1 if no candidate matches (or more than one
// matches, which — for a well-formed candidate set drawn from distinct
// real sessions — should occur only with astronomically low probability
// and is treated as "inconclusive" here rather than silently picking
// one).
//
// This is a convenience wrapper around
// VerifyRecoveredVariantSequenceMatchesUserSession for the common
// "narrow a short list of suspects down to one" anti-piracy workflow.
// candidateSessions elements are (userIdentifier, sessionIdentifier)
// pairs represented as the CandidateSession struct.
func IdentifyMatchingSessionAmongCandidates(candidateSessions []CandidateSession, recoveredSequence []VariantSelection) (matchedIndex int, matchDescription string) {
	foundIndex := -1
	matchCount := 0
	for index, candidate := range candidateSessions {
		if VerifyRecoveredVariantSequenceMatchesUserSession(candidate.UserIdentifier, candidate.SessionIdentifier, recoveredSequence) {
			matchCount++
			foundIndex = index
		}
	}
	switch matchCount {
	case 0:
		return -1, "no candidate session matched the recovered sequence"
	case 1:
		return foundIndex, fmt.Sprintf("unique match: candidate index %d", foundIndex)
	default:
		return -1, fmt.Sprintf("inconclusive: %d candidate sessions matched", matchCount)
	}
}

// CandidateSession is a (userIdentifier, sessionIdentifier) pair under
// investigation by IdentifyMatchingSessionAmongCandidates.
type CandidateSession struct {
	// UserIdentifier is the platform userId of the candidate session.
	UserIdentifier string
	// SessionIdentifier is the playback sessionId of the candidate session.
	SessionIdentifier string
}
