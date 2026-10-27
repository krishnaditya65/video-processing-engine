// Package manifest generates personalized HLS (.m3u8) and DASH (.mpd)
// manifests from a domain.PlaybackContext, a domain.VariantLadder, and an
// asset's content chunk timeline, applying (in order): device-capability
// filtering (package filtering, a Chain of Responsibility), SSAI ad-break
// splicing (package ssai), and DRM key signaling (package drm, a
// Strategy). The two output formats are themselves selected via a Strategy
// interface (ManifestFormatGenerator), and the whole multi-step assembly
// is exposed through a Builder (PersonalizedManifestBuilder) so callers
// configure the pieces incrementally before a single Build() call performs
// the actual generation.
package manifest

import (
	"fmt"

	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/drm"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/filtering"
	"edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/ssai"
)

// ManifestOutputFormat selects which manifest markup
// ManifestFormatGenerator implementation to use.
type ManifestOutputFormat string

const (
	// ManifestOutputFormatHls selects HLS (.m3u8) output.
	ManifestOutputFormatHls ManifestOutputFormat = "hls"
	// ManifestOutputFormatDash selects DASH (.mpd) output.
	ManifestOutputFormatDash ManifestOutputFormat = "dash"
)

// GeneratedManifestOutput is the fully-rendered result of a manifest build:
// the markup text plus the HTTP content type it should be served with.
type GeneratedManifestOutput struct {
	// Body is the complete manifest document text.
	Body string
	// ContentType is the MIME type this Body should be served as.
	ContentType string
	// SelectedRenditionCount records how many renditions survived
	// device-capability filtering, surfaced for logging/observability.
	SelectedRenditionCount int
}

// ManifestFormatGenerator is the Strategy interface implemented once per
// output format (HLS, DASH). Package manifest's Builder selects a concrete
// implementation based on the caller-requested ManifestOutputFormat and
// delegates the actual markup assembly to it, keeping format-specific
// string/XML construction fully isolated from the shared
// filtering/splicing/DRM pipeline.
type ManifestFormatGenerator interface {
	// GenerateManifestBody renders the complete manifest document for
	// the given inputs, all of which have already been resolved by the
	// shared pipeline (filtered renditions, spliced timeline, resolved
	// DRM provider).
	GenerateManifestBody(input ManifestGenerationInput) (GeneratedManifestOutput, error)
}

// ManifestGenerationInput bundles everything a ManifestFormatGenerator
// needs after the shared filtering/splicing/DRM-resolution steps have run,
// so each format's generator is a pure function of this struct.
type ManifestGenerationInput struct {
	// PlaybackContext is the original request context (asset id, device
	// capabilities, etc.).
	PlaybackContext domain.PlaybackContext
	// SelectedRenditions is the personalized variant ladder subset that
	// survived the device-capability filtering pipeline.
	SelectedRenditions []domain.VariantRendition
	// SplicedTimelinesByRendition maps each selected rendition's
	// RenditionIdentifier to its SSAI-spliced content+ad timeline.
	SplicedTimelinesByRendition map[string][]ssai.TimelineEntry
	// DrmProvider is the resolved Strategy implementation for
	// PlaybackContext.PreferredDrmSystem, or nil if playback is clear
	// (domain.DrmSystemNone or unrecognized).
	DrmProvider drm.DrmKeyProvider
}

// manifestFormatGeneratorsByOutputFormat is the Strategy registry mapping
// each supported ManifestOutputFormat to its generator implementation.
var manifestFormatGeneratorsByOutputFormat = map[ManifestOutputFormat]ManifestFormatGenerator{
	ManifestOutputFormatHls:  hlsManifestFormatGenerator{},
	ManifestOutputFormatDash: dashManifestFormatGenerator{},
}

// PersonalizedManifestBuilder implements the Builder pattern for
// personalized manifest assembly: WithX methods incrementally configure
// the asset's variant ladder, its content chunk timeline, and the desired
// output format, and a single Build() call runs the full
// filter-then-splice-then-render pipeline exactly once, returning the
// GeneratedManifestOutput.
type PersonalizedManifestBuilder struct {
	playbackContext domain.PlaybackContext
	variantLadder   domain.VariantLadder
	outputFormat    ManifestOutputFormat
	filterPipeline  *filtering.DeviceCapabilityFilterPipeline
	buildErrors     []error
}

// NewPersonalizedManifestBuilder starts a new Builder for the given
// playback context, defaulting to the platform's standard device
// capability filter pipeline (see filtering.NewDefaultDeviceCapabilityFilterPipeline).
func NewPersonalizedManifestBuilder(playbackContext domain.PlaybackContext) *PersonalizedManifestBuilder {
	return &PersonalizedManifestBuilder{
		playbackContext: playbackContext,
		filterPipeline:  filtering.NewDefaultDeviceCapabilityFilterPipeline(),
	}
}

// WithVariantLadder attaches the asset's full encoding ladder (pre
// device-capability filtering) to the builder. Returns the builder for
// fluent chaining.
func (builder *PersonalizedManifestBuilder) WithVariantLadder(ladder domain.VariantLadder) *PersonalizedManifestBuilder {
	builder.variantLadder = ladder
	return builder
}

// WithOutputFormat selects which ManifestFormatGenerator strategy Build()
// will delegate to. Returns the builder for fluent chaining.
func (builder *PersonalizedManifestBuilder) WithOutputFormat(format ManifestOutputFormat) *PersonalizedManifestBuilder {
	builder.outputFormat = format
	return builder
}

// WithDeviceCapabilityFilterPipeline overrides the default filter
// pipeline, primarily for tests that need to exercise a custom stage
// composition. Returns the builder for fluent chaining.
func (builder *PersonalizedManifestBuilder) WithDeviceCapabilityFilterPipeline(pipeline *filtering.DeviceCapabilityFilterPipeline) *PersonalizedManifestBuilder {
	builder.filterPipeline = pipeline
	return builder
}

// Build runs the full pipeline - device-capability filtering, then
// per-rendition SSAI splicing, then DRM provider resolution, then
// delegating to the selected ManifestFormatGenerator strategy - and
// returns the final GeneratedManifestOutput.
func (builder *PersonalizedManifestBuilder) Build() (GeneratedManifestOutput, error) {
	if len(builder.buildErrors) > 0 {
		return GeneratedManifestOutput{}, builder.buildErrors[0]
	}

	generator, ok := manifestFormatGeneratorsByOutputFormat[builder.outputFormat]
	if !ok {
		return GeneratedManifestOutput{}, fmt.Errorf("manifest: unsupported output format %q", builder.outputFormat)
	}

	selectedRenditions := builder.filterPipeline.RunFilterPipelineOverVariantLadder(builder.variantLadder, builder.playbackContext)
	if len(selectedRenditions) == 0 {
		return GeneratedManifestOutput{}, fmt.Errorf("manifest: no renditions available for asset %q after filtering", builder.playbackContext.AssetIdentifier)
	}

	splicedTimelines := make(map[string][]ssai.TimelineEntry, len(selectedRenditions))
	for _, rendition := range selectedRenditions {
		timeline, err := ssai.SpliceAdBreakSegmentsIntoManifestTimeline(rendition.Chunks, builder.playbackContext.AdBreakInsertionPoints)
		if err != nil {
			return GeneratedManifestOutput{}, fmt.Errorf("manifest: splicing rendition %q: %w", rendition.RenditionIdentifier, err)
		}
		splicedTimelines[rendition.RenditionIdentifier] = timeline
	}

	var drmProvider drm.DrmKeyProvider
	if provider, ok := drm.NewDrmKeyProviderForSystem(builder.playbackContext.PreferredDrmSystem); ok {
		drmProvider = provider
	}

	input := ManifestGenerationInput{
		PlaybackContext:             builder.playbackContext,
		SelectedRenditions:          selectedRenditions,
		SplicedTimelinesByRendition: splicedTimelines,
		DrmProvider:                 drmProvider,
	}

	output, err := generator.GenerateManifestBody(input)
	if err != nil {
		return GeneratedManifestOutput{}, err
	}
	output.SelectedRenditionCount = len(selectedRenditions)
	return output, nil
}

// BuildHlsMediaPlaylistForRendition runs the shared SSAI-splicing and
// DRM-resolution steps for a single named rendition of ladder and renders
// that rendition's standalone HLS media playlist - the document a real
// player fetches after selecting a variant from the master playlist's
// #EXT-X-STREAM-INF URIs (see generatePersonalizedHlsMasterPlaylistForPlaybackContext).
// It returns an error if renditionIdentifier does not name a rendition
// present in ladder.
func BuildHlsMediaPlaylistForRendition(playbackContext domain.PlaybackContext, ladder domain.VariantLadder, renditionIdentifier string) (GeneratedManifestOutput, error) {
	var target *domain.VariantRendition
	for i := range ladder.Renditions {
		if ladder.Renditions[i].RenditionIdentifier == renditionIdentifier {
			target = &ladder.Renditions[i]
			break
		}
	}
	if target == nil {
		return GeneratedManifestOutput{}, fmt.Errorf("manifest: rendition %q not found in ladder for asset %q", renditionIdentifier, ladder.AssetIdentifier)
	}

	timeline, err := ssai.SpliceAdBreakSegmentsIntoManifestTimeline(target.Chunks, playbackContext.AdBreakInsertionPoints)
	if err != nil {
		return GeneratedManifestOutput{}, fmt.Errorf("manifest: splicing rendition %q: %w", renditionIdentifier, err)
	}

	var drmProvider drm.DrmKeyProvider
	if provider, ok := drm.NewDrmKeyProviderForSystem(playbackContext.PreferredDrmSystem); ok {
		drmProvider = provider
	}

	body := generateHlsMediaPlaylistForRendition(playbackContext.AssetIdentifier, *target, timeline, drmProvider)
	return GeneratedManifestOutput{
		Body:                   body,
		ContentType:            "application/vnd.apple.mpegurl",
		SelectedRenditionCount: 1,
	}, nil
}
