package ssai

import (
	"testing"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
)

// buildContentChunks constructs count SourceMediaChunkReference entries of
// durationSeconds each, for use across the tests in this file.
func buildContentChunks(count int, durationSeconds float64) []domain.SourceMediaChunkReference {
	chunks := make([]domain.SourceMediaChunkReference, count)
	for i := 0; i < count; i++ {
		chunks[i] = domain.SourceMediaChunkReference{
			ChunkSequenceIndex: i,
			DurationSeconds:    durationSeconds,
		}
	}
	return chunks
}

// TestSpliceAdBreakSegmentsIntoManifestTimelineOrdersMidrollCorrectly
// verifies the headline SSAI splicing behavior: a mid-roll ad break lands
// between the correct pair of content chunks, in the correct order, and
// every subsequent content chunk's spliced-timeline start offset is
// shifted forward by exactly the ad break's total duration.
func TestSpliceAdBreakSegmentsIntoManifestTimelineOrdersMidrollCorrectly(t *testing.T) {
	// 10 chunks of 2s each = 20s of content; a cue at 10s should land
	// exactly between chunk 4 (ends at 10s) and chunk 5 (starts at 10s).
	chunks := buildContentChunks(10, 2.0)
	adBreaks := []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "midroll",
			CueTimeOffsetSeconds: 10.0,
			TotalDurationSeconds: 6.0,
			AdCreativeChunks: []domain.AdCreativeChunkReference{
				{ObjectStorageKey: "ad-1", DurationSeconds: 3.0},
				{ObjectStorageKey: "ad-2", DurationSeconds: 3.0},
			},
		},
	}

	timeline, err := SpliceAdBreakSegmentsIntoManifestTimeline(chunks, adBreaks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect: content 0..4 (5 entries), ad-1, ad-2, content 5..9 (5 entries) = 12 total.
	if len(timeline) != 12 {
		t.Fatalf("expected 12 timeline entries, got %d", len(timeline))
	}

	for i := 0; i < 5; i++ {
		if timeline[i].Kind != TimelineEntryKindContent {
			t.Errorf("entry %d: expected content, got %s", i, timeline[i].Kind)
		}
		if timeline[i].SourceContentChunkSequenceIndex != i {
			t.Errorf("entry %d: expected content chunk index %d, got %d", i, i, timeline[i].SourceContentChunkSequenceIndex)
		}
	}

	if timeline[5].Kind != TimelineEntryKindAdCreative || timeline[5].SourceObjectStorageKeyOrChunkIndex != "ad-1" {
		t.Errorf("entry 5: expected first ad creative 'ad-1', got %+v", timeline[5])
	}
	if !timeline[5].IsFirstEntryOfAdBreak {
		t.Error("entry 5: expected IsFirstEntryOfAdBreak=true")
	}
	if timeline[6].Kind != TimelineEntryKindAdCreative || timeline[6].SourceObjectStorageKeyOrChunkIndex != "ad-2" {
		t.Errorf("entry 6: expected second ad creative 'ad-2', got %+v", timeline[6])
	}
	if timeline[6].IsFirstEntryOfAdBreak {
		t.Error("entry 6: expected IsFirstEntryOfAdBreak=false for the second ad chunk")
	}

	if timeline[7].Kind != TimelineEntryKindContent || timeline[7].SourceContentChunkSequenceIndex != 5 {
		t.Errorf("entry 7: expected content chunk 5 resuming after the ad break, got %+v", timeline[7])
	}
	if !timeline[7].IsFirstContentEntryAfterAdBreak {
		t.Error("entry 7: expected IsFirstContentEntryAfterAdBreak=true")
	}

	for i := 8; i < 12; i++ {
		if timeline[i].IsFirstContentEntryAfterAdBreak {
			t.Errorf("entry %d: did not expect IsFirstContentEntryAfterAdBreak=true", i)
		}
	}

	// Verify spliced timeline start offsets: content chunk 5 (index 7 in
	// timeline) should start at 10 (pre-ad content) + 6 (ad break) = 16s.
	if got, want := timeline[7].SplicedTimelineStartOffsetSeconds, 16.0; got != want {
		t.Errorf("content chunk 5's spliced start offset = %.3f, want %.3f", got, want)
	}
	// The last content chunk (index 11 in timeline, original chunk 9)
	// should start at 16 + 4*2 = 24s.
	if got, want := timeline[11].SplicedTimelineStartOffsetSeconds, 24.0; got != want {
		t.Errorf("last content chunk's spliced start offset = %.3f, want %.3f", got, want)
	}
}

// TestSpliceAdBreakSegmentsIntoManifestTimelineHandlesMultipleAdBreaksInOrder
// verifies two ad breaks at different cue points are both spliced in, in
// timeline order, even when passed to the splicer in reverse
// chronological order (proving the splicer sorts, rather than trusting
// caller order).
func TestSpliceAdBreakSegmentsIntoManifestTimelineHandlesMultipleAdBreaksInOrder(t *testing.T) {
	chunks := buildContentChunks(10, 2.0) // 20s of content
	adBreaks := []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "second-break-later-cue",
			CueTimeOffsetSeconds: 16.0,
			TotalDurationSeconds: 2.0,
			AdCreativeChunks:     []domain.AdCreativeChunkReference{{ObjectStorageKey: "ad-late", DurationSeconds: 2.0}},
		},
		{
			CueIdentifier:        "first-break-earlier-cue",
			CueTimeOffsetSeconds: 4.0,
			TotalDurationSeconds: 2.0,
			AdCreativeChunks:     []domain.AdCreativeChunkReference{{ObjectStorageKey: "ad-early", DurationSeconds: 2.0}},
		},
	}

	timeline, err := SpliceAdBreakSegmentsIntoManifestTimeline(chunks, adBreaks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var adOrder []string
	for _, entry := range timeline {
		if entry.Kind == TimelineEntryKindAdCreative {
			adOrder = append(adOrder, entry.SourceObjectStorageKeyOrChunkIndex)
		}
	}
	if len(adOrder) != 2 || adOrder[0] != "ad-early" || adOrder[1] != "ad-late" {
		t.Errorf("expected ad creatives in chronological order [ad-early, ad-late], got %v", adOrder)
	}
}

// TestSpliceAdBreakSegmentsIntoManifestTimelineHandlesPostRollCue verifies
// an ad break whose cue point falls at or beyond the end of the content
// timeline is appended after all content (post-roll semantics).
func TestSpliceAdBreakSegmentsIntoManifestTimelineHandlesPostRollCue(t *testing.T) {
	chunks := buildContentChunks(3, 2.0) // 6s of content
	adBreaks := []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "postroll",
			CueTimeOffsetSeconds: 6.0,
			TotalDurationSeconds: 2.0,
			AdCreativeChunks:     []domain.AdCreativeChunkReference{{ObjectStorageKey: "ad-post", DurationSeconds: 2.0}},
		},
	}
	timeline, err := SpliceAdBreakSegmentsIntoManifestTimeline(chunks, adBreaks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(timeline) != 4 {
		t.Fatalf("expected 4 entries (3 content + 1 ad), got %d", len(timeline))
	}
	last := timeline[len(timeline)-1]
	if last.Kind != TimelineEntryKindAdCreative || last.SourceObjectStorageKeyOrChunkIndex != "ad-post" {
		t.Errorf("expected the post-roll ad to be the last entry, got %+v", last)
	}
}

// TestSpliceAdBreakSegmentsIntoManifestTimelineRejectsDurationMismatch
// verifies the splicer refuses an ad break whose declared
// TotalDurationSeconds does not match the sum of its creative chunks'
// durations, since that mismatch would desynchronize advertised segment
// timing from actual payload duration.
func TestSpliceAdBreakSegmentsIntoManifestTimelineRejectsDurationMismatch(t *testing.T) {
	chunks := buildContentChunks(5, 2.0)
	adBreaks := []domain.AdBreakInsertionPoint{
		{
			CueIdentifier:        "bad-break",
			CueTimeOffsetSeconds: 2.0,
			TotalDurationSeconds: 10.0,                                                                                // declared 10s...
			AdCreativeChunks:     []domain.AdCreativeChunkReference{{ObjectStorageKey: "ad-1", DurationSeconds: 4.0}}, // ...but chunks only sum to 4s
		},
	}
	_, err := SpliceAdBreakSegmentsIntoManifestTimeline(chunks, adBreaks)
	if err == nil {
		t.Fatal("expected an error for a duration-mismatched ad break, got nil")
	}
}

// TestSpliceAdBreakSegmentsIntoManifestTimelineWithNoAdBreaksPassesContentThrough
// verifies that with zero ad breaks, the splicer returns the content
// chunks unchanged (aside from being wrapped as TimelineEntry values).
func TestSpliceAdBreakSegmentsIntoManifestTimelineWithNoAdBreaksPassesContentThrough(t *testing.T) {
	chunks := buildContentChunks(4, 2.0)
	timeline, err := SpliceAdBreakSegmentsIntoManifestTimeline(chunks, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(timeline) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(timeline))
	}
	for i, entry := range timeline {
		if entry.Kind != TimelineEntryKindContent {
			t.Errorf("entry %d: expected content, got %s", i, entry.Kind)
		}
		if entry.SplicedTimelineStartOffsetSeconds != float64(i)*2.0 {
			t.Errorf("entry %d: expected start offset %.1f, got %.1f", i, float64(i)*2.0, entry.SplicedTimelineStartOffsetSeconds)
		}
	}
}
