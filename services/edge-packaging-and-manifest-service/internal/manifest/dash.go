// dash.go implements the DASH (.mpd) ManifestFormatGenerator strategy.
// Unlike HLS's flat, discontinuity-marked media playlist, DASH's
// idiomatic SSAI representation is Multi-Period: the main content and each
// ad break each get their own <Period>, which is the model implemented
// here (matching how real DASH SSAI systems - e.g. DASH-IF's SAND/ad
// insertion guidelines - structure ad breaks).
package manifest

import (
	"fmt"
	"strings"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/ssai"
)

// dashManifestFormatGenerator is the DASH implementation of
// ManifestFormatGenerator.
type dashManifestFormatGenerator struct{}

// GenerateManifestBody renders the DASH MPD for the given generation
// input by delegating to generatePersonalizedDashManifestForPlaybackContext.
func (dashManifestFormatGenerator) GenerateManifestBody(input ManifestGenerationInput) (GeneratedManifestOutput, error) {
	body := generatePersonalizedDashManifestForPlaybackContext(input)
	return GeneratedManifestOutput{
		Body:        body,
		ContentType: "application/dash+xml",
	}, nil
}

// dashPeriodTimelineSegment is one <S> entry destined for a
// SegmentTimeline: a duration in timescale units, exposed here rather than
// computed inline so period-splitting logic (splitTimelineIntoDashPeriods)
// and rendering logic (renderDashSegmentTimeline) stay decoupled.
type dashPeriodTimelineSegment struct {
	// uriOrChunkKey is the same content-chunk-index/ad-object-key
	// identifier ssai.TimelineEntry carries.
	uriOrChunkKey string
	// durationSeconds is this segment's playback duration.
	durationSeconds float64
}

// dashPeriod is one <Period> worth of spliced timeline: either the main
// content (isAdPeriod=false) or a single ad break's creatives
// (isAdPeriod=true).
type dashPeriod struct {
	// isAdPeriod distinguishes an ad-break Period from a content Period.
	isAdPeriod bool
	// segments lists this Period's timeline entries in order.
	segments []dashPeriodTimelineSegment
	// adBreakCueIdentifier is set only when isAdPeriod is true.
	adBreakCueIdentifier string
}

// splitTimelineIntoDashPeriods groups a flat SSAI-spliced TimelineEntry
// sequence (as produced by package ssai for one rendition) into ordered
// dashPeriod groups: every run of consecutive content entries becomes one
// content Period, and every run of consecutive ad entries belonging to the
// same ad break becomes one ad Period, matching DASH's Multi-Period SSAI
// convention referenced in this file's package doc.
func splitTimelineIntoDashPeriods(timeline []ssai.TimelineEntry) []dashPeriod {
	var periods []dashPeriod
	for _, entry := range timeline {
		startsNewAdPeriod := entry.Kind == ssai.TimelineEntryKindAdCreative && entry.IsFirstEntryOfAdBreak
		startsNewContentPeriod := entry.Kind == ssai.TimelineEntryKindContent && (entry.IsFirstContentEntryAfterAdBreak || len(periods) == 0)

		needsNewPeriod := len(periods) == 0 || startsNewAdPeriod || startsNewContentPeriod
		if needsNewPeriod {
			periods = append(periods, dashPeriod{
				isAdPeriod:           entry.Kind == ssai.TimelineEntryKindAdCreative,
				adBreakCueIdentifier: entry.BelongsToAdBreakCueIdentifier,
			})
		}
		current := &periods[len(periods)-1]
		current.segments = append(current.segments, dashPeriodTimelineSegment{
			uriOrChunkKey:   entry.SourceObjectStorageKeyOrChunkIndex,
			durationSeconds: entry.DurationSeconds,
		})
	}
	return periods
}

// generatePersonalizedDashManifestForPlaybackContext builds the DASH MPD:
// a top-level <MPD> with one <Period> per contiguous content/ad-break run
// (see splitTimelineIntoDashPeriods), each Period containing one
// <AdaptationSet> per surviving rendition group... simplified here to one
// AdaptationSet per rendition (real ladders group by codec/language into
// shared AdaptationSets; this simulation keeps a 1:1 rendition:AdaptationSet
// mapping for clarity) with a <ContentProtection> element when a DRM
// provider is active.
func generatePersonalizedDashManifestForPlaybackContext(input ManifestGenerationInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&b, "<MPD xmlns=\"urn:mpeg:dash:schema:mpd:2011\" xmlns:cenc=\"urn:mpeg:cenc:2013\" profiles=\"urn:mpeg:dash:profile:isoff-live:2011\" type=\"static\" mediaPresentationDuration=\"PT%.3fS\">\n",
		totalDurationSeconds(input))

	// Use the first selected rendition's spliced timeline to determine
	// Period boundaries; ad-break placement is identical across
	// renditions since splicing operates on cue-time offsets shared by
	// the whole asset, not per-rendition content.
	referenceRendition := input.SelectedRenditions[0]
	referencePeriods := splitTimelineIntoDashPeriods(input.SplicedTimelinesByRendition[referenceRendition.RenditionIdentifier])

	for periodIndex, referencePeriod := range referencePeriods {
		if referencePeriod.isAdPeriod {
			renderDashAdPeriod(&b, periodIndex, referencePeriod)
			continue
		}
		renderDashContentPeriod(&b, periodIndex, input)
	}

	fmt.Fprintf(&b, "</MPD>\n")
	return b.String()
}

// renderDashContentPeriod writes one main-content <Period>, containing one
// <AdaptationSet> per selected rendition with that rendition's own spliced
// content-only segments for this period's position in the timeline.
func renderDashContentPeriod(b *strings.Builder, periodIndex int, input ManifestGenerationInput) {
	fmt.Fprintf(b, "  <Period id=\"content-%d\">\n", periodIndex)
	for _, rendition := range input.SelectedRenditions {
		periods := splitTimelineIntoDashPeriods(input.SplicedTimelinesByRendition[rendition.RenditionIdentifier])
		if periodIndex >= len(periods) {
			continue
		}
		renderDashAdaptationSetForRendition(b, input.PlaybackContext.AssetIdentifier, rendition, periods[periodIndex], input.DrmProvider)
	}
	fmt.Fprintf(b, "  </Period>\n")
}

// renderDashAdPeriod writes one ad-break <Period> containing a single
// <AdaptationSet> for the ad creative's chunk sequence (ad creatives are
// not personalized per device-capability rendition in this simulation, so
// only one AdaptationSet is emitted here, unlike the per-rendition content
// Periods).
func renderDashAdPeriod(b *strings.Builder, periodIndex int, period dashPeriod) {
	fmt.Fprintf(b, "  <Period id=\"ad-%s\">\n", period.adBreakCueIdentifier)
	fmt.Fprintf(b, "    <AdaptationSet mimeType=\"video/mp4\" contentType=\"video\">\n")
	fmt.Fprintf(b, "      <Representation id=\"ad-creative\" bandwidth=\"0\">\n")
	renderDashSegmentTimeline(b, period.segments)
	fmt.Fprintf(b, "      </Representation>\n")
	fmt.Fprintf(b, "    </AdaptationSet>\n")
	fmt.Fprintf(b, "  </Period>\n")
}

// renderDashAdaptationSetForRendition writes one <AdaptationSet> +
// <Representation> pair for a single content rendition within a single
// Period, attaching a <ContentProtection> element (see package drm) when
// DRM is active. The ContentProtection's key identifier is resolved from
// this period's first segment's chunk-sequence index, so it reflects the
// correct rotation epoch for content beginning at this Period.
func renderDashAdaptationSetForRendition(
	b *strings.Builder,
	assetIdentifier string,
	rendition domain.VariantRendition,
	period dashPeriod,
	drmProvider drm.DrmKeyProvider,
) {
	fmt.Fprintf(b, "    <AdaptationSet mimeType=\"video/mp4\" contentType=\"video\" codecs=\"%s\">\n", buildRfc6381CodecString(rendition))
	if drmProvider != nil {
		rotationIndex := firstSegmentRotationIndex(period)
		protection := drmProvider.GenerateDashContentProtectionElementForAsset(assetIdentifier, rotationIndex)
		fmt.Fprintf(b, "      %s\n", protection.XML)
	}
	fmt.Fprintf(b, "      <Representation id=\"%s\" bandwidth=\"%d\" width=\"%d\" height=\"%d\">\n",
		rendition.RenditionIdentifier, rendition.PeakBitsPerSecond, rendition.FrameWidthPixels, rendition.FrameHeightPixels)
	renderDashSegmentTimeline(b, period.segments)
	fmt.Fprintf(b, "      </Representation>\n")
	fmt.Fprintf(b, "    </AdaptationSet>\n")
}

// firstSegmentRotationIndex parses a dashPeriod's first segment's
// chunk-sequence identifier back into an integer and resolves its DRM
// rotation index. Ad-break periods (whose segment keys are object-storage
// strings, not integers) resolve to rotation index 0 as a harmless
// default since ad creative in this simulation is not itself encrypted
// with rotating content keys.
func firstSegmentRotationIndex(period dashPeriod) int {
	if len(period.segments) == 0 {
		return 0
	}
	var chunkSequenceIndex int
	if _, err := fmt.Sscanf(period.segments[0].uriOrChunkKey, "%d", &chunkSequenceIndex); err != nil {
		return 0
	}
	return drm.RotationIndexForChunkSequence(chunkSequenceIndex)
}

// renderDashSegmentTimeline writes a <SegmentTemplate>/<SegmentTimeline>
// pair covering the given segments, using a fixed timescale of 1000 (i.e.
// durations are expressed directly in milliseconds) for simplicity.
func renderDashSegmentTimeline(b *strings.Builder, segments []dashPeriodTimelineSegment) {
	const timelineTimescale = 1000
	fmt.Fprintf(b, "        <SegmentTemplate timescale=\"%d\" media=\"$Number$.m4s\" initialization=\"init.mp4\" startNumber=\"0\">\n", timelineTimescale)
	fmt.Fprintf(b, "          <SegmentTimeline>\n")
	for _, seg := range segments {
		fmt.Fprintf(b, "            <S d=\"%d\" /> <!-- chunk %s -->\n", int64(seg.durationSeconds*timelineTimescale), seg.uriOrChunkKey)
	}
	fmt.Fprintf(b, "          </SegmentTimeline>\n")
	fmt.Fprintf(b, "        </SegmentTemplate>\n")
}

// totalDurationSeconds sums the first selected rendition's spliced
// timeline durations to compute the MPD's overall
// mediaPresentationDuration.
func totalDurationSeconds(input ManifestGenerationInput) float64 {
	if len(input.SelectedRenditions) == 0 {
		return 0
	}
	timeline := input.SplicedTimelinesByRendition[input.SelectedRenditions[0].RenditionIdentifier]
	total := 0.0
	for _, entry := range timeline {
		total += entry.DurationSeconds
	}
	return total
}
