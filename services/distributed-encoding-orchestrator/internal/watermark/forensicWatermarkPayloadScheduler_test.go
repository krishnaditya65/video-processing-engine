package watermark

import "testing"

// TestGenerateDeterministicVariantSequenceForSession_IsDeterministicAcrossCalls
// verifies the core determinism requirement: identical inputs always
// produce an identical output sequence.
func TestGenerateDeterministicVariantSequenceForSession_IsDeterministicAcrossCalls(t *testing.T) {
	first := GenerateDeterministicVariantSequenceForSession("user-1", "session-a", 500)
	second := GenerateDeterministicVariantSequenceForSession("user-1", "session-a", 500)

	if len(first) != 500 || len(second) != 500 {
		t.Fatalf("expected sequences of length 500, got %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("sequence differs at index %d: %v vs %v", i, first[i], second[i])
		}
	}
}

// TestGenerateDeterministicVariantSequenceForSession_DifferentUsersDiffer
// verifies that distinct (userId, sessionId) pairs produce (with
// overwhelming probability) distinguishable sequences — the essential
// property that makes the scheme useful as a per-user fingerprint. We
// assert the two sequences are not IDENTICAL over a reasonably long
// span, which for a well-diffused hash chain should never coincidentally
// collide.
func TestGenerateDeterministicVariantSequenceForSession_DifferentUsersDiffer(t *testing.T) {
	sequenceA := GenerateDeterministicVariantSequenceForSession("user-alice", "session-1", 256)
	sequenceB := GenerateDeterministicVariantSequenceForSession("user-bob", "session-1", 256)

	identical := true
	for i := range sequenceA {
		if sequenceA[i] != sequenceB[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("expected different users to produce different variant sequences, got identical sequences")
	}
}

// TestGenerateDeterministicVariantSequenceForSession_DifferentSessionsForSameUserDiffer
// verifies session-mixing: the same user with two different sessionIds
// must not produce the same sequence (so a single compromised session
// cannot be replayed to infer another session's marking).
func TestGenerateDeterministicVariantSequenceForSession_DifferentSessionsForSameUserDiffer(t *testing.T) {
	sequenceSession1 := GenerateDeterministicVariantSequenceForSession("user-carol", "session-1", 256)
	sequenceSession2 := GenerateDeterministicVariantSequenceForSession("user-carol", "session-2", 256)

	identical := true
	for i := range sequenceSession1 {
		if sequenceSession1[i] != sequenceSession2[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("expected different sessionIds for the same user to produce different sequences")
	}
}

// TestGenerateDeterministicVariantSequenceForSession_ZeroAndNegativeChunkCounts
// verifies the documented degenerate-input contract.
func TestGenerateDeterministicVariantSequenceForSession_ZeroAndNegativeChunkCounts(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		sequence := GenerateDeterministicVariantSequenceForSession("user-x", "session-x", n)
		if sequence == nil {
			t.Fatalf("expected non-nil empty slice for totalChunkCount=%d", n)
		}
		if len(sequence) != 0 {
			t.Fatalf("expected empty slice for totalChunkCount=%d, got %v", n, sequence)
		}
	}
}

// TestVariantSelection_StringRendersAOrB verifies the String() method.
func TestVariantSelection_StringRendersAOrB(t *testing.T) {
	if VariantA.String() != "A" {
		t.Errorf("expected VariantA.String() == \"A\", got %q", VariantA.String())
	}
	if VariantB.String() != "B" {
		t.Errorf("expected VariantB.String() == \"B\", got %q", VariantB.String())
	}
}

// TestVerifyRecoveredVariantSequenceMatchesUserSession_RoundTripCorrectness
// is the primary encode/decode round-trip proof: generate a full
// sequence, then verify it against the same (user, session), including
// verifying arbitrary-length PREFIXES (simulating partial forensic
// recovery of leaked content).
func TestVerifyRecoveredVariantSequenceMatchesUserSession_RoundTripCorrectness(t *testing.T) {
	const totalChunks = 1000
	fullSequence := GenerateDeterministicVariantSequenceForSession("user-dave", "session-xyz", totalChunks)

	if !VerifyRecoveredVariantSequenceMatchesUserSession("user-dave", "session-xyz", fullSequence) {
		t.Fatal("expected full sequence to verify against its own (user, session)")
	}

	for _, prefixLen := range []int{1, 2, 17, 100, 500, 999, 1000} {
		prefix := fullSequence[:prefixLen]
		if !VerifyRecoveredVariantSequenceMatchesUserSession("user-dave", "session-xyz", prefix) {
			t.Errorf("expected prefix of length %d to verify successfully", prefixLen)
		}
	}
}

// TestVerifyRecoveredVariantSequenceMatchesUserSession_RejectsWrongSession
// verifies the negative case: a sequence generated for one session must
// not verify against a different (user, session) pair, except with
// negligible probability of accidental prefix collision (which we avoid
// by using a reasonably long prefix).
func TestVerifyRecoveredVariantSequenceMatchesUserSession_RejectsWrongSession(t *testing.T) {
	sequenceForEve := GenerateDeterministicVariantSequenceForSession("user-eve", "session-real", 64)

	if VerifyRecoveredVariantSequenceMatchesUserSession("user-eve", "session-imposter", sequenceForEve) {
		t.Fatal("expected verification to fail against a different sessionId")
	}
	if VerifyRecoveredVariantSequenceMatchesUserSession("user-mallory", "session-real", sequenceForEve) {
		t.Fatal("expected verification to fail against a different userId")
	}
}

// TestVerifyRecoveredVariantSequenceMatchesUserSession_EmptyRecoveredSequenceNeverVerifies
// verifies the documented "no evidence" contract for an empty input.
func TestVerifyRecoveredVariantSequenceMatchesUserSession_EmptyRecoveredSequenceNeverVerifies(t *testing.T) {
	if VerifyRecoveredVariantSequenceMatchesUserSession("anyone", "anything", nil) {
		t.Fatal("expected empty recovered sequence to never verify")
	}
	if VerifyRecoveredVariantSequenceMatchesUserSession("anyone", "anything", []VariantSelection{}) {
		t.Fatal("expected empty recovered sequence to never verify")
	}
}

// TestVerifyRecoveredVariantSequenceMatchesUserSession_DetectsSingleBitTampering
// verifies that flipping even one bit in a recovered sequence causes
// verification to fail, demonstrating the scheme's sensitivity (it is
// not merely checking length or a checksum).
func TestVerifyRecoveredVariantSequenceMatchesUserSession_DetectsSingleBitTampering(t *testing.T) {
	sequence := GenerateDeterministicVariantSequenceForSession("user-frank", "session-42", 128)

	tampered := make([]VariantSelection, len(sequence))
	copy(tampered, sequence)
	flipIndex := 50
	if tampered[flipIndex] == VariantA {
		tampered[flipIndex] = VariantB
	} else {
		tampered[flipIndex] = VariantA
	}

	if VerifyRecoveredVariantSequenceMatchesUserSession("user-frank", "session-42", tampered) {
		t.Fatal("expected tampered sequence (single bit flipped before the check window) to fail verification")
	}
}

// TestIdentifyMatchingSessionAmongCandidates_UniqueMatchFound verifies the
// anti-piracy "narrow down suspects" convenience wrapper picks out the
// single correct candidate from a list.
func TestIdentifyMatchingSessionAmongCandidates_UniqueMatchFound(t *testing.T) {
	const chunkCount = 100
	genuineSequence := GenerateDeterministicVariantSequenceForSession("user-guilty", "session-leak", chunkCount)

	candidates := []CandidateSession{
		{UserIdentifier: "user-innocent-1", SessionIdentifier: "session-1"},
		{UserIdentifier: "user-innocent-2", SessionIdentifier: "session-2"},
		{UserIdentifier: "user-guilty", SessionIdentifier: "session-leak"},
		{UserIdentifier: "user-innocent-3", SessionIdentifier: "session-3"},
	}

	matchedIndex, description := IdentifyMatchingSessionAmongCandidates(candidates, genuineSequence)
	if matchedIndex != 2 {
		t.Fatalf("expected matched index 2 (the guilty candidate), got %d (%s)", matchedIndex, description)
	}
}

// TestIdentifyMatchingSessionAmongCandidates_NoMatchReturnsNegativeOne
// verifies the "no candidate matches" outcome.
func TestIdentifyMatchingSessionAmongCandidates_NoMatchReturnsNegativeOne(t *testing.T) {
	recoveredSequence := GenerateDeterministicVariantSequenceForSession("user-nobody-here", "session-nowhere", 50)

	candidates := []CandidateSession{
		{UserIdentifier: "user-a", SessionIdentifier: "session-a"},
		{UserIdentifier: "user-b", SessionIdentifier: "session-b"},
	}

	matchedIndex, _ := IdentifyMatchingSessionAmongCandidates(candidates, recoveredSequence)
	if matchedIndex != -1 {
		t.Fatalf("expected no match (-1), got %d", matchedIndex)
	}
}
