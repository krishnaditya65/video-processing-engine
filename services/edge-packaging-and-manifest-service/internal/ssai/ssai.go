// Package ssai implements Server-Side Ad Insertion manifest splicing: given
// an asset's ordered content chunk timeline and a list of SCTE-35-like
// AdBreakInsertionPoint cue points, it produces a single ordered
// TimelineEntry sequence interleaving ad-creative entries at the correct
// offsets, which package manifest then renders into format-specific
// markers (HLS #EXT-X-DISCONTINUITY / DASH ad Period).
//
// # SSAI timing model
//
// Each domain.AdBreakInsertionPoint carries a CueTimeOffsetSeconds, the
// point on the *content* timeline (i.e. ignoring any ads already spliced
// in) at which playback must divert into the ad break. The splicer walks
// the content chunk list in order, accumulating elapsed content duration,
// and inserts the ad break's TimelineEntry group immediately before the
// first content chunk whose start time would be >= CueTimeOffsetSeconds.
// This mirrors SCTE-35 splice_insert() semantics: the cue marks a splice
// point on the unmodified content timeline, not a position in the
// already-spliced output. Ad chunks are never allowed to further subdivide
// a content chunk - the splice point is always a content-chunk boundary,
// matching how real packagers require ad breaks aligned to segment
// boundaries.
package ssai

import (
	"fmt"
	"sort"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
)

// TimelineEntryKind distinguishes a content chunk entry from an ad chunk
// entry in a spliced TimelineEntry sequence.
type TimelineEntryKind string

const (
	// TimelineEntryKindContent marks an entry sourced from the asset's
	// main content chunk timeline.
	TimelineEntryKindContent TimelineEntryKind = "content"
	// TimelineEntryKindAdCreative marks an entry sourced from an ad
	// break's creative chunks.
	TimelineEntryKindAdCreative TimelineEntryKind = "ad"
)

// TimelineEntry is one segment-equivalent slot in the final spliced
// playback timeline: either a content chunk or an ad-creative chunk, along
// with its resolved start offset in the *spliced* (post-insertion) output
// timeline and the ad-break identifier it belongs to, if any.
type TimelineEntry struct {
	// Kind distinguishes content vs. ad-creative entries.
	Kind TimelineEntryKind
	// SourceObjectStorageKeyOrChunkIndex identifies the underlying
	// content chunk (by ChunkSequenceIndex, formatted as a string) or ad
	// creative chunk (by its ObjectStorageKey) this entry renders.
	SourceObjectStorageKeyOrChunkIndex string
	// DurationSeconds is this entry's playback duration.
	DurationSeconds float64
	// SplicedTimelineStartOffsetSeconds is this entry's start offset in
	// the final, post-splice output timeline (i.e. it already accounts
	// for the extra duration any earlier ad breaks inserted).
	SplicedTimelineStartOffsetSeconds float64
	// BelongsToAdBreakCueIdentifier is non-empty for ad entries,
	// carrying the originating AdBreakInsertionPoint.CueIdentifier; it
	// is used to emit a single discontinuity marker per ad break rather
	// than per ad chunk.
	BelongsToAdBreakCueIdentifier string
	// IsFirstEntryOfAdBreak marks the first TimelineEntry of an ad
	// break's creative chunks, the point at which an HLS
	// #EXT-X-DISCONTINUITY (or DASH new ad Period) must be emitted.
	IsFirstEntryOfAdBreak bool
	// IsFirstContentEntryAfterAdBreak marks the first content entry
	// immediately following an ad break's last chunk, the point at
	// which playback must emit a second #EXT-X-DISCONTINUITY (or DASH
	// return to the content Period).
	IsFirstContentEntryAfterAdBreak bool
	// SourceContentChunkSequenceIndex is set (and >= 0) only for
	// content entries, echoing the originating chunk's
	// ChunkSequenceIndex for callers that need it beyond the string
	// form above.
	SourceContentChunkSequenceIndex int
}

// SpliceAdBreakSegmentsIntoManifestTimeline is the core SSAI splicing
// operation: it merges contentChunks (assumed already sorted by
// ChunkSequenceIndex) with adBreaks (in any order; this function sorts a
// local copy by CueTimeOffsetSeconds) into one ordered TimelineEntry
// sequence, per the timing model documented at the package level.
//
// It returns an error if any AdBreakInsertionPoint's
// TotalDurationSeconds does not match the sum of its AdCreativeChunks'
// DurationSeconds, since a mismatch there would silently desynchronize
// the spliced manifest's advertised segment timing from the actual ad
// creative payload duration.
func SpliceAdBreakSegmentsIntoManifestTimeline(
	contentChunks []domain.SourceMediaChunkReference,
	adBreaks []domain.AdBreakInsertionPoint,
) ([]TimelineEntry, error) {
	for _, adBreak := range adBreaks {
		if err := validateAdBreakDurationMatchesCreativeChunkSum(adBreak); err != nil {
			return nil, err
		}
	}

	sortedAdBreaks := append([]domain.AdBreakInsertionPoint(nil), adBreaks...)
	sort.Slice(sortedAdBreaks, func(i, j int) bool {
		return sortedAdBreaks[i].CueTimeOffsetSeconds < sortedAdBreaks[j].CueTimeOffsetSeconds
	})

	var timeline []TimelineEntry
	splicedElapsedSeconds := 0.0
	contentElapsedSeconds := 0.0
	adBreakCursor := 0

	for _, chunk := range contentChunks {
		// Insert every ad break whose cue point falls at-or-before this
		// chunk's content-timeline start, before appending the chunk
		// itself, so cue points that land exactly on a chunk boundary
		// splice in ahead of that chunk (matches SCTE-35 splice_insert
		// "play out the ad, then resume content at the boundary").
		for adBreakCursor < len(sortedAdBreaks) && sortedAdBreaks[adBreakCursor].CueTimeOffsetSeconds <= contentElapsedSeconds {
			adBreak := sortedAdBreaks[adBreakCursor]
			for i, adChunk := range adBreak.AdCreativeChunks {
				timeline = append(timeline, TimelineEntry{
					Kind:                               TimelineEntryKindAdCreative,
					SourceObjectStorageKeyOrChunkIndex: adChunk.ObjectStorageKey,
					DurationSeconds:                    adChunk.DurationSeconds,
					SplicedTimelineStartOffsetSeconds:  splicedElapsedSeconds,
					BelongsToAdBreakCueIdentifier:      adBreak.CueIdentifier,
					IsFirstEntryOfAdBreak:              i == 0,
					SourceContentChunkSequenceIndex:    -1,
				})
				splicedElapsedSeconds += adChunk.DurationSeconds
			}
			adBreakCursor++
			// The content entry immediately following this ad break
			// (the current chunk about to be appended below, or a
			// later one if multiple ad breaks stack at the same cue
			// point) will be flagged after this loop.
		}

		isFirstContentAfterAdBreak := len(timeline) > 0 && timeline[len(timeline)-1].Kind == TimelineEntryKindAdCreative

		timeline = append(timeline, TimelineEntry{
			Kind:                               TimelineEntryKindContent,
			SourceObjectStorageKeyOrChunkIndex: fmt.Sprintf("%d", chunk.ChunkSequenceIndex),
			DurationSeconds:                    chunk.DurationSeconds,
			SplicedTimelineStartOffsetSeconds:  splicedElapsedSeconds,
			IsFirstContentEntryAfterAdBreak:    isFirstContentAfterAdBreak,
			SourceContentChunkSequenceIndex:    chunk.ChunkSequenceIndex,
		})
		splicedElapsedSeconds += chunk.DurationSeconds
		contentElapsedSeconds += chunk.DurationSeconds
	}

	// Any remaining ad breaks whose cue point is at or beyond the end of
	// the content timeline are appended at the very end (post-roll
	// semantics).
	for ; adBreakCursor < len(sortedAdBreaks); adBreakCursor++ {
		adBreak := sortedAdBreaks[adBreakCursor]
		for i, adChunk := range adBreak.AdCreativeChunks {
			timeline = append(timeline, TimelineEntry{
				Kind:                               TimelineEntryKindAdCreative,
				SourceObjectStorageKeyOrChunkIndex: adChunk.ObjectStorageKey,
				DurationSeconds:                    adChunk.DurationSeconds,
				SplicedTimelineStartOffsetSeconds:  splicedElapsedSeconds,
				BelongsToAdBreakCueIdentifier:      adBreak.CueIdentifier,
				IsFirstEntryOfAdBreak:              i == 0,
				SourceContentChunkSequenceIndex:    -1,
			})
			splicedElapsedSeconds += adChunk.DurationSeconds
		}
	}

	return timeline, nil
}

// validateAdBreakDurationMatchesCreativeChunkSum checks that an
// AdBreakInsertionPoint's declared TotalDurationSeconds matches the sum of
// its AdCreativeChunks' durations, within a small floating-point epsilon.
func validateAdBreakDurationMatchesCreativeChunkSum(adBreak domain.AdBreakInsertionPoint) error {
	const floatingPointComparisonEpsilonSeconds = 0.001
	sum := 0.0
	for _, chunk := range adBreak.AdCreativeChunks {
		sum += chunk.DurationSeconds
	}
	delta := sum - adBreak.TotalDurationSeconds
	if delta < 0 {
		delta = -delta
	}
	if delta > floatingPointComparisonEpsilonSeconds {
		return fmt.Errorf(
			"ssai: ad break %q declares TotalDurationSeconds=%.3f but its creative chunks sum to %.3f",
			adBreak.CueIdentifier, adBreak.TotalDurationSeconds, sum,
		)
	}
	return nil
}
