// Package filtering implements the device-capability and network-quality
// variant-ladder narrowing pipeline as a classic Chain of Responsibility:
// each VariantLadderFilterStage receives the ladder produced by the
// previous stage, applies exactly one narrowing rule, and hands the result
// to the next stage. Composing single-responsibility stages this way keeps
// each rule independently testable and lets the edge add/remove/reorder
// rules (e.g. "drop AV1 for low-end devices" vs "cap resolution for poor
// network") without touching unrelated logic.
package filtering

import "edgepackaging.gormalone.io/edge-packaging-and-manifest-service/internal/domain"

// VariantLadderFilterStage is one link in the Chain of Responsibility that
// narrows a VariantLadder's Renditions down for a given PlaybackContext. A
// stage must be side-effect-free and must not mutate the slice it is
// given; it returns a new, possibly-shorter slice of surviving renditions.
type VariantLadderFilterStage interface {
	// ApplyFilterStageToRenditions returns the subset of candidateRenditions
	// that survive this stage's rule for the given playback context.
	ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition
	// StageName identifies this stage for logging/diagnostics and for
	// the per-stage assertions in the test suite.
	StageName() string
}

// DeviceCapabilityFilterPipeline is the ordered Chain of Responsibility of
// VariantLadderFilterStage links that together implement
// "remove unsupported tracks for low-end devices" and the related
// network/codec narrowing rules.
type DeviceCapabilityFilterPipeline struct {
	// stages is the ordered list of filter links to run in sequence.
	stages []VariantLadderFilterStage
}

// NewDefaultDeviceCapabilityFilterPipeline constructs the pipeline this
// service runs in production: low-end-device simplification, then codec
// support, then HDR support, then resolution cap, then network-quality
// bitrate cap, then QoE-driven ceiling downgrade. Order matters only for the
// low-end-device stage, which is intentionally run first since it can
// short-circuit later stages by already reducing the ladder to a single
// safe rendition, and for the QoE stage, which is intentionally run last so
// it always has the final say — it can only ever further tighten the
// ceiling the network-quality stage already computed, never loosen it.
func NewDefaultDeviceCapabilityFilterPipeline() *DeviceCapabilityFilterPipeline {
	return &DeviceCapabilityFilterPipeline{
		stages: []VariantLadderFilterStage{
			lowEndDeviceSimplificationFilterStage{},
			videoCodecSupportFilterStage{},
			audioCodecSupportFilterStage{},
			highDynamicRangeSupportFilterStage{},
			maximumResolutionCapFilterStage{},
			networkQualityBitrateCapFilterStage{},
			qoeDrivenVariantCeilingFilterStage{},
		},
	}
}

// RunFilterPipelineOverVariantLadder pushes ladder.Renditions through every
// stage of the chain in order and returns the final, personalized subset.
// If every stage's output would be empty, the pipeline falls back to the
// single lowest-bitrate rendition of the original ladder so a client is
// never left with zero playable renditions (a real edge would prefer
// "degraded playback" over "no playback").
func (pipeline *DeviceCapabilityFilterPipeline) RunFilterPipelineOverVariantLadder(ladder domain.VariantLadder, context domain.PlaybackContext) []domain.VariantRendition {
	surviving := append([]domain.VariantRendition(nil), ladder.Renditions...)
	for _, stage := range pipeline.stages {
		surviving = stage.ApplyFilterStageToRenditions(surviving, context)
	}
	if len(surviving) == 0 && len(ladder.Renditions) > 0 {
		return []domain.VariantRendition{lowestBitrateRendition(ladder.Renditions)}
	}
	return surviving
}

// lowestBitrateRendition returns the rendition with the smallest
// PeakBitsPerSecond, the safest single fallback for a client every stage
// would otherwise reject entirely.
func lowestBitrateRendition(renditions []domain.VariantRendition) domain.VariantRendition {
	lowest := renditions[0]
	for _, candidate := range renditions[1:] {
		if candidate.PeakBitsPerSecond < lowest.PeakBitsPerSecond {
			lowest = candidate
		}
	}
	return lowest
}

// lowEndDeviceSimplificationFilterStage implements the headline
// "remove unsupported tracks for low-end devices" rule: when the playback
// context reports a low-end/legacy device, it drops every rendition above
// a conservative resolution/bitrate ceiling and drops HDR and non-H.264
// renditions outright, regardless of what the device profile's individual
// codec flags claim, on the assumption that low-end device profiles are
// frequently under-reported or stale.
type lowEndDeviceSimplificationFilterStage struct{}

// lowEndDeviceMaximumVerticalResolutionPixels is the resolution ceiling
// applied to low-end/legacy devices regardless of their reported cap.
const lowEndDeviceMaximumVerticalResolutionPixels = 480

// lowEndDeviceMaximumBitsPerSecond is the bitrate ceiling applied to
// low-end/legacy devices regardless of network quality hint.
const lowEndDeviceMaximumBitsPerSecond = 1_500_000

func (lowEndDeviceSimplificationFilterStage) StageName() string {
	return "low-end-device-simplification"
}

func (lowEndDeviceSimplificationFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	if !context.DeviceCapabilities.IsLowEndOrLegacyDevice {
		return candidateRenditions
	}
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.VideoCodec != domain.VideoCodecH264 {
			continue
		}
		if rendition.IsHighDynamicRange {
			continue
		}
		if rendition.FrameHeightPixels > lowEndDeviceMaximumVerticalResolutionPixels {
			continue
		}
		if rendition.PeakBitsPerSecond > lowEndDeviceMaximumBitsPerSecond {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// videoCodecSupportFilterStage drops renditions encoded with a video codec
// the requesting device does not report support for. H.264 is always
// treated as supported, matching universal hardware/software decode
// availability.
type videoCodecSupportFilterStage struct{}

func (videoCodecSupportFilterStage) StageName() string { return "video-codec-support" }

func (videoCodecSupportFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		switch rendition.VideoCodec {
		case domain.VideoCodecHevc:
			if !context.DeviceCapabilities.SupportsHevcVideoCodec {
				continue
			}
		case domain.VideoCodecAv1:
			if !context.DeviceCapabilities.SupportsAv1VideoCodec {
				continue
			}
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// audioCodecSupportFilterStage drops renditions whose audio track uses a
// codec the requesting device does not report support for. AAC is always
// treated as supported.
type audioCodecSupportFilterStage struct{}

func (audioCodecSupportFilterStage) StageName() string { return "audio-codec-support" }

func (audioCodecSupportFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.AudioCodec == domain.AudioCodecEac3 && !context.DeviceCapabilities.SupportsDolbyDigitalPlusAudio {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// highDynamicRangeSupportFilterStage drops HDR renditions when the device
// cannot render HDR without edge-side tone-mapping (which this service
// does not perform).
type highDynamicRangeSupportFilterStage struct{}

func (highDynamicRangeSupportFilterStage) StageName() string { return "hdr-support" }

func (highDynamicRangeSupportFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.IsHighDynamicRange && !context.DeviceCapabilities.SupportsHighDynamicRangeVideo {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// maximumResolutionCapFilterStage drops renditions above the device's
// reported maximum decodable vertical resolution, when one was reported.
type maximumResolutionCapFilterStage struct{}

func (maximumResolutionCapFilterStage) StageName() string { return "maximum-resolution-cap" }

func (maximumResolutionCapFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	cap := context.DeviceCapabilities.MaximumDecodableVerticalResolutionPixels
	if cap <= 0 {
		return candidateRenditions
	}
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.FrameHeightPixels > cap {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// networkQualityBitrateCapFilterStage drops renditions whose peak bitrate
// exceeds the sustainable ceiling implied by the client's reported
// NetworkQualityHint, so the personalized manifest never advertises a
// rung the client is expected to stall on.
type networkQualityBitrateCapFilterStage struct{}

func (networkQualityBitrateCapFilterStage) StageName() string { return "network-quality-bitrate-cap" }

func (networkQualityBitrateCapFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	ceiling := context.NetworkQualityHint.SustainableBitsPerSecondCeiling()
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.PeakBitsPerSecond > ceiling {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}

// qoeDrivenVariantCeilingFilterStage is the QoE-personalization hook point:
// when the request carries a domain.RecentQoeSignal indicating the session
// is struggling (elevated recent rebuffer ratio, and/or a directly measured
// throughput below what the coarse NetworkQualityHint assumes), this stage
// further downgrades the surviving variant ladder's bitrate ceiling below
// what networkQualityBitrateCapFilterStage already computed. This is where
// a real deployment would plug in predictive-qoe-ml-service's output — a
// model that predicts near-term rebuffer risk from recent playback
// telemetry — rather than only reacting to network conditions already
// observed by the coarse NetworkQualityHint bucket.
//
// When context.RecentQoeSignal is nil, or the signal does not indicate a
// struggling session, this stage is a no-op and returns candidateRenditions
// unchanged: it only ever tightens the ladder, never loosens or replaces
// what earlier stages already decided.
type qoeDrivenVariantCeilingFilterStage struct{}

func (qoeDrivenVariantCeilingFilterStage) StageName() string { return "qoe-driven-variant-ceiling" }

func (qoeDrivenVariantCeilingFilterStage) ApplyFilterStageToRenditions(candidateRenditions []domain.VariantRendition, context domain.PlaybackContext) []domain.VariantRendition {
	signal := context.RecentQoeSignal
	if signal == nil || !signal.IndicatesStrugglingSession() {
		return candidateRenditions
	}
	ceiling := signal.RecommendedBitrateCeilingGivenNetworkQualityHint(context.NetworkQualityHint)
	var surviving []domain.VariantRendition
	for _, rendition := range candidateRenditions {
		if rendition.PeakBitsPerSecond > ceiling {
			continue
		}
		surviving = append(surviving, rendition)
	}
	return surviving
}
