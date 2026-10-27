// Package domain holds the shared, dependency-free data model for the Edge
// Routing & Packaging Engine: the description of a playback request
// (PlaybackContext), the description of the encoded content that is
// available to serve (VariantLadder / VariantRendition / SourceSegment), and
// the description of server-side ad insertion breakpoints
// (AdBreakInsertionPoint). Every other package in this service (manifest,
// ssai, drm, filtering, fmp4) is built on top of these types, so they are
// kept intentionally small, serialization-friendly, and free of any
// networking or format-specific concerns.
package domain

import "time"

// NetworkQualityHint is a coarse, edge-observable classification of the
// requesting client's current network conditions. Real deployments would
// derive this from CDN-observed throughput, a client-reported bandwidth
// estimate (e.g. the CMCD "br" / "tb" keys), or RTT probing; here it is
// accepted directly as a request hint (query parameter or header) to keep
// the service self-contained.
type NetworkQualityHint string

const (
	// NetworkQualityPoor represents a severely bandwidth-constrained
	// client (e.g. sub-500kbps effective throughput, high packet loss).
	NetworkQualityPoor NetworkQualityHint = "poor"
	// NetworkQualityModerate represents a constrained-but-usable
	// broadband/cellular connection (roughly 500kbps-2.5Mbps).
	NetworkQualityModerate NetworkQualityHint = "moderate"
	// NetworkQualityGood represents a typical broadband or strong
	// cellular connection (roughly 2.5Mbps-8Mbps).
	NetworkQualityGood NetworkQualityHint = "good"
	// NetworkQualityExcellent represents an unconstrained connection
	// capable of sustaining the top of the variant ladder (>8Mbps).
	NetworkQualityExcellent NetworkQualityHint = "excellent"
)

// approximateSustainableBitsPerSecondByNetworkQualityHint maps each coarse
// network quality bucket to a conservative sustainable bitrate ceiling, in
// bits per second, used by the network-aware filtering stage to drop
// variants the client is unlikely to sustain without rebuffering.
var approximateSustainableBitsPerSecondByNetworkQualityHint = map[NetworkQualityHint]int64{
	NetworkQualityPoor:      600_000,
	NetworkQualityModerate:  2_500_000,
	NetworkQualityGood:      8_000_000,
	NetworkQualityExcellent: 50_000_000,
}

// SustainableBitsPerSecondCeiling returns the conservative bitrate ceiling,
// in bits per second, that a client reporting this NetworkQualityHint is
// expected to sustain. Unknown/empty hints are treated as
// NetworkQualityGood, a deliberately safe middle default so a missing
// header never silently locks a client out of every variant.
func (hint NetworkQualityHint) SustainableBitsPerSecondCeiling() int64 {
	if ceiling, known := approximateSustainableBitsPerSecondByNetworkQualityHint[hint]; known {
		return ceiling
	}
	return approximateSustainableBitsPerSecondByNetworkQualityHint[NetworkQualityGood]
}

// VideoCodec enumerates the video compression formats this engine knows how
// to signal in manifests and, where relevant, filter on for device support.
type VideoCodec string

const (
	// VideoCodecH264 is ISO/IEC 14496-10 AVC, the universally-supported
	// baseline codec that every device in the filtering pipeline is
	// assumed to decode.
	VideoCodecH264 VideoCodec = "h264"
	// VideoCodecHevc is ITU-T H.265/HEVC, requiring explicit device
	// support (hardware decode on most set-top and mobile silicon).
	VideoCodecHevc VideoCodec = "hevc"
	// VideoCodecAv1 is the AOMedia AV1 codec, requiring explicit device
	// support and generally reserved for newer/higher-end devices.
	VideoCodecAv1 VideoCodec = "av1"
)

// AudioCodec enumerates the audio compression formats referenced by a
// VariantRendition's separately-stored audio track.
type AudioCodec string

const (
	// AudioCodecAac is MPEG-4 AAC-LC, the universally-supported baseline
	// audio codec.
	AudioCodecAac AudioCodec = "aac"
	// AudioCodecEac3 is Dolby Digital Plus (E-AC-3), a premium surround
	// codec requiring explicit device support.
	AudioCodecEac3 AudioCodec = "eac3"
)

// DrmSystemIdentifier enumerates the multi-DRM systems this engine can
// stitch signaling for. An empty/DrmSystemNone value means the manifest is
// generated without any content-protection signaling (clear content).
type DrmSystemIdentifier string

const (
	// DrmSystemNone indicates the requested playback session is
	// unencrypted ("clear") and requires no DRM signaling at all.
	DrmSystemNone DrmSystemIdentifier = "none"
	// DrmSystemWidevine identifies Google Widevine, the DRM system used
	// by Chrome, Android, and most Android TV / smart TV platforms.
	DrmSystemWidevine DrmSystemIdentifier = "widevine"
	// DrmSystemFairPlay identifies Apple FairPlay Streaming, required
	// for HLS playback on Safari, iOS, tvOS, and macOS.
	DrmSystemFairPlay DrmSystemIdentifier = "fairplay"
	// DrmSystemPlayReady identifies Microsoft PlayReady, used by Xbox,
	// Edge, and many smart TV / set-top platforms.
	DrmSystemPlayReady DrmSystemIdentifier = "playready"
)

// DeviceCapabilityProfile describes what the requesting playback client is
// known (or assumed) to be capable of decoding and rendering. It is the
// primary input to the Chain of Responsibility device-capability filtering
// pipeline implemented in package filtering.
type DeviceCapabilityProfile struct {
	// IsLowEndOrLegacyDevice marks the request as originating from a
	// constrained device class (e.g. an entry-level set-top box or an
	// old smart TV firmware) that should be steered toward the most
	// conservative, safest-to-decode subset of the variant ladder
	// regardless of its individual codec/resolution flags below.
	IsLowEndOrLegacyDevice bool
	// SupportsHevcVideoCodec reports whether the client can decode
	// H.265/HEVC video.
	SupportsHevcVideoCodec bool
	// SupportsAv1VideoCodec reports whether the client can decode AV1
	// video.
	SupportsAv1VideoCodec bool
	// SupportsHighDynamicRangeVideo reports whether the client's display
	// pipeline can render HDR (HDR10/Dolby Vision class) video without
	// requiring SDR tone-mapping at the edge.
	SupportsHighDynamicRangeVideo bool
	// SupportsDolbyDigitalPlusAudio reports whether the client can
	// decode Dolby Digital Plus (E-AC-3) surround audio.
	SupportsDolbyDigitalPlusAudio bool
	// MaximumDecodableVerticalResolutionPixels caps the vertical
	// resolution (e.g. 1080 for a 1080p-only panel/decoder) the client
	// can usefully decode and display. A value of 0 means "no cap
	// reported", and the filtering stage will not restrict on
	// resolution.
	MaximumDecodableVerticalResolutionPixels int
	// SupportedContainerFormats lists the container formats
	// ("fmp4", "ts") the client's player can demux. An empty slice is
	// treated as "supports fmp4 only", the modern baseline.
	SupportedContainerFormats []string
}

// qoeStrugglingSessionRebufferRatioThreshold is the minimum
// RecentRebufferRatio (fraction of recent playback time spent rebuffering,
// 0.0-1.0) at which a session is judged "struggling" and eligible for
// QoE-driven variant-ceiling downgrade. 3% rebuffer ratio is a commonly-used
// real-world threshold above which viewers measurably start abandoning
// sessions.
const qoeStrugglingSessionRebufferRatioThreshold = 0.03

// qoeDowngradedCeilingMultiplier is applied to the network-quality-hint
// bitrate ceiling when a session is judged struggling, so the QoE stage
// tightens (never loosens) whatever ceiling the coarse NetworkQualityHint
// already computed.
const qoeDowngradedCeilingMultiplier = 0.5

// qoeMeasuredThroughputSafetyMarginMultiplier discounts a client's directly
// measured recent throughput before treating it as a bitrate ceiling, so the
// offered rendition leaves headroom for throughput variance rather than
// exactly matching the most recent measurement.
const qoeMeasuredThroughputSafetyMarginMultiplier = 0.8

// RecentQoeSignal carries recent, session-observed Quality-of-Experience
// telemetry for the requesting playback session — the hook point through
// which an external prediction service (predictive-qoe-ml-service) would
// feed its output into manifest personalization. It is intentionally a
// small, transport-agnostic struct: in production it would be populated
// either from client-reported CMCD-style telemetry on the manifest request
// itself, or from a side-channel call to predictive-qoe-ml-service keyed by
// RequestingSessionID; this service accepts it directly as an optional
// field on PlaybackContext so the personalization hook can be exercised and
// tested without either integration existing yet.
type RecentQoeSignal struct {
	// RecentRebufferRatio is the fraction (0.0-1.0) of the session's recent
	// playback window spent rebuffering. Higher values indicate a client
	// that is struggling to keep its buffer filled at the renditions it has
	// been playing.
	RecentRebufferRatio float64
	// MeasuredThroughputBitsPerSecond is the session's recently measured
	// actual achieved throughput, in bits per second, if known (0 means
	// "not reported" and is ignored). This is typically a more accurate,
	// session-specific signal than the coarse NetworkQualityHint bucket.
	MeasuredThroughputBitsPerSecond int64
}

// IndicatesStrugglingSession reports whether this QoE signal describes a
// session that is struggling badly enough to warrant tightening the offered
// variant ladder ceiling: either the recent rebuffer ratio has crossed
// qoeStrugglingSessionRebufferRatioThreshold, or a reported measured
// throughput implies the session cannot sustain even the network-quality
// hint's ceiling.
func (signal RecentQoeSignal) IndicatesStrugglingSession() bool {
	return signal.RecentRebufferRatio >= qoeStrugglingSessionRebufferRatioThreshold
}

// RecommendedBitrateCeilingGivenNetworkQualityHint computes the tightened
// bitrate ceiling, in bits per second, this QoE signal recommends for a
// struggling session given the request's coarse NetworkQualityHint. It
// never exceeds (only ever further restricts) hint.SustainableBitsPerSecondCeiling():
// it is the lesser of (a) that ceiling scaled down by
// qoeDowngradedCeilingMultiplier and (b) a safety-margined fraction of
// MeasuredThroughputBitsPerSecond, if a nonzero measurement was reported.
func (signal RecentQoeSignal) RecommendedBitrateCeilingGivenNetworkQualityHint(hint NetworkQualityHint) int64 {
	ceiling := int64(float64(hint.SustainableBitsPerSecondCeiling()) * qoeDowngradedCeilingMultiplier)
	if signal.MeasuredThroughputBitsPerSecond > 0 {
		measuredCeiling := int64(float64(signal.MeasuredThroughputBitsPerSecond) * qoeMeasuredThroughputSafetyMarginMultiplier)
		if measuredCeiling < ceiling {
			ceiling = measuredCeiling
		}
	}
	return ceiling
}

// PlaybackContext is the fully-resolved description of a single manifest or
// packaging request: who is asking (device capabilities), over what network
// (NetworkQualityHint), under what content-protection system (DrmSystem),
// and with what ad breaks must be spliced into the timeline
// (AdBreakInsertionPoints). Package manifest's Builder consumes a
// PlaybackContext to produce a personalized HLS or DASH manifest.
type PlaybackContext struct {
	// AssetIdentifier is the catalog/content identifier the manifest is
	// being generated for.
	AssetIdentifier string
	// RequestingSessionID is an opaque per-playback-session identifier,
	// used only for logging/tracing correlation in this simulation.
	RequestingSessionID string
	// DeviceCapabilities describes what the requesting client can
	// decode and render.
	DeviceCapabilities DeviceCapabilityProfile
	// NetworkQualityHint classifies the client's current network
	// conditions, used to additionally cap the variant ladder.
	NetworkQualityHint NetworkQualityHint
	// PreferredDrmSystem selects which multi-DRM key provider strategy
	// should stitch key signaling into the generated manifest.
	PreferredDrmSystem DrmSystemIdentifier
	// AdBreakInsertionPoints lists the SCTE-35-like ad break cue points
	// that must be server-side spliced into the personalized manifest's
	// segment/period timeline.
	AdBreakInsertionPoints []AdBreakInsertionPoint
	// RequestedContainerFormat optionally forces "fmp4" or "ts" JIT
	// packaging; an empty string lets the packager pick fmp4 (the
	// default modern container).
	RequestedContainerFormat string
	// RecentQoeSignal optionally carries recent Quality-of-Experience
	// telemetry for this session (rebuffer ratio, measured throughput),
	// used by the QoE-driven variant-ceiling filtering stage (package
	// filtering) to further downgrade the offered ladder when the session
	// is struggling. A nil value means no QoE signal is available, and
	// that stage becomes a no-op.
	RecentQoeSignal *RecentQoeSignal
}

// AdCreativeChunkReference identifies a single object-storage-resident
// media chunk belonging to an ad creative that must be interleaved into the
// content timeline during server-side ad insertion.
type AdCreativeChunkReference struct {
	// ObjectStorageKey is the backing-store key (e.g. an S3 object key)
	// for this ad chunk's payload bytes.
	ObjectStorageKey string
	// DurationSeconds is the wall-clock duration this ad chunk occupies
	// in the spliced timeline.
	DurationSeconds float64
}

// AdBreakInsertionPoint models a single SCTE-35-like ad break cue: a point
// in the content timeline (CueTimeOffsetSeconds) at which playback should
// divert, for TotalDurationSeconds, into the listed AdCreativeChunks before
// resuming content.
type AdBreakInsertionPoint struct {
	// CueIdentifier is a unique identifier for this ad break, mirroring
	// the role an SCTE-35 splice_insert() event_id plays in broadcast
	// signaling.
	CueIdentifier string
	// CueTimeOffsetSeconds is the offset, in seconds from the start of
	// the asset's main content timeline, at which the ad break begins.
	CueTimeOffsetSeconds float64
	// TotalDurationSeconds is the sum duration of the ad break, which
	// should equal the sum of AdCreativeChunks' durations; the splicer
	// validates this and tests assert on it.
	TotalDurationSeconds float64
	// AdCreativeChunks lists, in playback order, the ad creative chunks
	// to splice in at CueTimeOffsetSeconds.
	AdCreativeChunks []AdCreativeChunkReference
}

// SourceMediaChunkReference identifies one elementary-stream chunk
// (video-only or audio-only, per how this engine's upstream encoder stores
// tracks separately in object storage) that the JIT packager must fetch and
// wrap into a container response.
type SourceMediaChunkReference struct {
	// ChunkSequenceIndex is this chunk's zero-based position within the
	// rendition's chunk timeline; it also drives deterministic DRM key
	// rotation (see package drm).
	ChunkSequenceIndex int
	// VideoTrackObjectStorageKey is the backing-store key for this
	// chunk's raw elementary video track bytes.
	VideoTrackObjectStorageKey string
	// AudioTrackObjectStorageKey is the backing-store key for this
	// chunk's raw elementary audio track bytes.
	AudioTrackObjectStorageKey string
	// DurationSeconds is this chunk's wall-clock duration; the platform
	// standardizes on ~2s chunks, matching the JIT-packaging latency
	// target documented in the README.
	DurationSeconds float64
	// BaseMediaDecodeTimeInTimescaleUnits is the fMP4 tfdt-equivalent
	// decode timestamp for this chunk, expressed in the track's
	// timescale (see VariantRendition.TimescaleUnitsPerSecond).
	BaseMediaDecodeTimeInTimescaleUnits uint64
}

// VariantRendition describes one rung of the encoding ladder for an asset:
// a specific resolution/bitrate/codec combination and the ordered list of
// chunks that make it up.
type VariantRendition struct {
	// RenditionIdentifier is a short, stable identifier for this
	// variant (e.g. "v0-360p-h264").
	RenditionIdentifier string
	// PeakBitsPerSecond is the encoded peak bitrate, used both for
	// HLS BANDWIDTH / DASH @bandwidth signaling and for network-aware
	// filtering.
	PeakBitsPerSecond int64
	// FrameWidthPixels is the encoded frame width.
	FrameWidthPixels int
	// FrameHeightPixels is the encoded frame height.
	FrameHeightPixels int
	// VideoCodec is the video compression format this rendition was
	// encoded with.
	VideoCodec VideoCodec
	// AudioCodec is the audio compression format this rendition's audio
	// track was encoded with.
	AudioCodec AudioCodec
	// IsHighDynamicRange marks this rendition as carrying HDR (vs SDR)
	// video.
	IsHighDynamicRange bool
	// TimescaleUnitsPerSecond is the fMP4 media timescale (e.g. 90000)
	// this rendition's chunks' timestamps are expressed in.
	TimescaleUnitsPerSecond uint32
	// Chunks lists, in playback order, the source media chunks that
	// make up this rendition's timeline.
	Chunks []SourceMediaChunkReference
}

// VariantLadder is the full set of encoding renditions available for an
// asset, the input the Chain of Responsibility filtering pipeline narrows
// down into a PersonalizedVariantLadder for a specific PlaybackContext.
type VariantLadder struct {
	// AssetIdentifier is the catalog/content identifier this ladder
	// belongs to.
	AssetIdentifier string
	// Renditions lists every encoded rendition available, from lowest
	// to highest bitrate by convention (not enforced).
	Renditions []VariantRendition
	// LiveEdgeGeneratedAt records when this ladder snapshot was
	// generated; used only for observability/log correlation.
	LiveEdgeGeneratedAt time.Time
}
