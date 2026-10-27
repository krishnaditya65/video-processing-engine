// This file implements the Decorator design pattern over
// CodecArgumentBuilder: HdrMetadataPassthroughCodecArgumentBuilder wraps
// any existing CodecArgumentBuilder (software or hardware) and augments
// its argument slice with the ffmpeg flags needed to preserve HDR
// (high dynamic range) color/metadata information from source to output,
// without the wrapped builder needing any HDR-specific knowledge of its
// own. This mirrors how, e.g., io.Reader decorators (bufio.Reader,
// gzip.Reader) wrap another io.Reader in the standard library: the outer
// type implements the same interface as the thing it wraps and adds
// behavior around delegated calls.
//
// # What "HDR metadata" means for this pipeline
//
// The ingestion service's StructuralMap contract (upstream of this
// orchestrator) carries, per source asset, static and (for HDR10+/Dolby
// Vision content) dynamic HDR metadata describing how a decoder/display
// should map the encoded signal to light output. This orchestrator models
// that as HdrMetadataProfile and threads it through
// BuildFfmpegCommandLineArguments as additional CLI flags appended just
// before the output path.
//
// # An honest boundary: what this package does NOT do
//
// ffmpeg's own video filters/muxers can carry STATIC HDR metadata
// end-to-end via the flags this decorator emits (-color_primaries,
// -color_trc, -colorspace, -master-display, -max_cll): these describe a
// single, unchanging mastering-display characterization for the whole
// asset, and ffmpeg (via libx265's -x265-params for HEVC, in particular)
// can write them into the encoded bitstream directly.
//
// DYNAMIC per-frame HDR metadata — HDR10+'s frame-by-frame tone-mapping
// SEI messages, and Dolby Vision's RPU (Reference Processing Unit) data —
// is a fundamentally different problem: it is not a handful of static CLI
// flags but a per-frame side-channel that must be extracted from the
// source (or a vendor-supplied metadata file) and re-multiplexed onto the
// ENCODED bitstream, frame-for-frame, after ffmpeg's own encode pass.
// Stock ffmpeg builds do not implement that extraction/injection
// themselves; the real, production-grade tools for it are:
//
//   - hdr10plus_tool (https://github.com/quietvoid/hdr10plus_tool):
//     extracts a JSON per-frame HDR10+ metadata file from a source
//     bitstream, and injects it into a freshly-encoded HEVC bitstream via
//     x265's --dhdr10-info parameter (which this decorator surfaces as an
//     -x265-params dhdr10-info=<path> fragment, see
//     BuildHdr10PlusInjectionX265ParamsFragment below — but this package
//     does not itself run hdr10plus_tool or produce that JSON file).
//   - dovi_tool (https://github.com/quietvoid/dovi_tool): extracts the
//     Dolby Vision RPU from a source bitstream and re-injects it into a
//     freshly-encoded HEVC bitstream as a post-encode multiplexing step
//     (again, not an ffmpeg CLI flag at all).
//
// This package therefore documents, and emits placeholders/flags for, the
// STATIC portion of HDR passthrough that ffmpeg genuinely handles
// natively, and clearly labels the DYNAMIC portion
// (HdrDynamicMetadataKind) as requiring an external post-processing step
// this codebase does not implement — an explicit, honest scope boundary
// rather than a flag that silently does nothing. See the README's
// Limitations section for the same boundary stated for operators.
package ffmpeg

import "fmt"

// HdrTransferFunction identifies which electro-optical transfer function
// (EOTF) the mastering/display metadata uses, i.e. how encoded sample
// values map to light output. These correspond directly to ffmpeg's
// "-color_trc" values.
type HdrTransferFunction string

const (
	// HdrTransferFunctionNone means the source is not HDR (SDR content);
	// HdrMetadataPassthroughCodecArgumentBuilder emits no color-metadata
	// flags at all for this value, so wrapping a builder with a
	// zero-value HdrMetadataProfile is a documented, safe no-op.
	HdrTransferFunctionNone HdrTransferFunction = ""
	// HdrTransferFunctionSmpte2084Pq is SMPTE ST 2084 (PQ, Perceptual
	// Quantizer), used by HDR10, HDR10+, and Dolby Vision.
	HdrTransferFunctionSmpte2084Pq HdrTransferFunction = "smpte2084"
	// HdrTransferFunctionAribStdB67Hlg is ARIB STD-B67 (HLG, Hybrid
	// Log-Gamma), used by broadcast-oriented HDR delivery.
	HdrTransferFunctionAribStdB67Hlg HdrTransferFunction = "arib-std-b67"
)

// HdrDynamicMetadataKind identifies which (if any) dynamic, per-frame HDR
// metadata format accompanies the source, which determines whether an
// external post-processing tool step (see package doc comment) is
// required in addition to this builder's static-metadata flags for full
// fidelity passthrough.
type HdrDynamicMetadataKind int

const (
	// HdrDynamicMetadataNone means only static mastering-display metadata
	// applies (plain HDR10, or SDR) — no external tool step is needed.
	HdrDynamicMetadataNone HdrDynamicMetadataKind = iota
	// HdrDynamicMetadataHdr10Plus means the source carries HDR10+
	// per-frame dynamic tone-mapping SEI messages, requiring
	// hdr10plus_tool as a separate extraction/injection pipeline step
	// (see package doc comment) for full fidelity.
	HdrDynamicMetadataHdr10Plus
	// HdrDynamicMetadataDolbyVisionRpu means the source carries a Dolby
	// Vision RPU stream, requiring dovi_tool as a separate
	// extraction/injection pipeline step (see package doc comment) for
	// full fidelity.
	HdrDynamicMetadataDolbyVisionRpu
)

// String renders an HdrDynamicMetadataKind as a short label for logs.
func (kind HdrDynamicMetadataKind) String() string {
	switch kind {
	case HdrDynamicMetadataNone:
		return "none"
	case HdrDynamicMetadataHdr10Plus:
		return "hdr10plus"
	case HdrDynamicMetadataDolbyVisionRpu:
		return "dolby_vision_rpu"
	default:
		return "unknown"
	}
}

// HdrMetadataProfile describes one source asset's HDR color/mastering
// characterization, as carried by the ingestion service's StructuralMap
// contract. The zero value (HdrTransferFunctionNone, everything else
// empty/zero) represents "not HDR content" and causes
// HdrMetadataPassthroughCodecArgumentBuilder to add no flags at all.
type HdrMetadataProfile struct {
	// ColorPrimaries is the ffmpeg "-color_primaries" value, e.g.
	// "bt2020" for Rec. 2020 (the primaries used by virtually all
	// consumer HDR content).
	ColorPrimaries string
	// TransferCharacteristic is the ffmpeg "-color_trc" value.
	TransferCharacteristic HdrTransferFunction
	// ColorSpace is the ffmpeg "-colorspace" (matrix coefficients) value,
	// e.g. "bt2020nc" (non-constant luminance Rec. 2020, the near-universal
	// choice for HDR streaming delivery).
	ColorSpace string
	// MasterDisplayMetadataValue, if non-empty, is passed verbatim as
	// ffmpeg's "-master-display" value: the SMPTE ST 2086 static mastering
	// display color volume (primaries + white point + luminance range)
	// describing the display the content was graded on, e.g.
	// "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)".
	MasterDisplayMetadataValue string
	// MaxContentLightLevel and MaxFrameAverageLightLevel, if
	// MaxContentLightLevel > 0, are combined into ffmpeg's "-max_cll"
	// value ("<MaxCLL>,<MaxFALL>") per CEA-861.3: the maximum single-pixel
	// and maximum frame-average light levels (in cd/m^2) present anywhere
	// in the content.
	MaxContentLightLevel      int
	MaxFrameAverageLightLevel int
	// DynamicMetadataKind identifies whether dynamic per-frame HDR
	// metadata (HDR10+/Dolby Vision) accompanies this asset — see the
	// package doc comment for the honest boundary around what this
	// package does and does not do for that case.
	DynamicMetadataKind HdrDynamicMetadataKind
	// Hdr10PlusMetadataJsonFilePath, when DynamicMetadataKind ==
	// HdrDynamicMetadataHdr10Plus, is the filesystem path to the
	// hdr10plus_tool-produced per-frame metadata JSON file that a real
	// production pipeline would already have extracted upstream of this
	// orchestrator (via a separate pipeline stage, NOT by this package).
	// BuildHdr10PlusInjectionX265ParamsFragment uses it to build the
	// libx265 "--dhdr10-info" parameter fragment that instructs x265 to
	// re-inject that metadata during encode.
	Hdr10PlusMetadataJsonFilePath string
}

// isHdrContent reports whether profile describes actual HDR content (as
// opposed to the zero-value "not HDR" sentinel).
func (profile HdrMetadataProfile) isHdrContent() bool {
	return profile.TransferCharacteristic != HdrTransferFunctionNone
}

// BuildStaticColorMetadataArguments returns the ffmpeg CLI argument
// fragment carrying profile's STATIC color/mastering metadata
// (-color_primaries, -color_trc, -colorspace, and, when present,
// -master-display/-max_cll). Returns an empty (non-nil) slice for the
// zero-value "not HDR" profile.
//
// This is the portion of HDR passthrough ffmpeg genuinely implements
// natively (see package doc comment for the dynamic-metadata boundary
// this function deliberately does NOT attempt to cross).
func (profile HdrMetadataProfile) BuildStaticColorMetadataArguments() []string {
	if !profile.isHdrContent() {
		return []string{}
	}

	args := []string{}
	if profile.ColorPrimaries != "" {
		args = append(args, "-color_primaries", profile.ColorPrimaries)
	}
	args = append(args, "-color_trc", string(profile.TransferCharacteristic))
	if profile.ColorSpace != "" {
		args = append(args, "-colorspace", profile.ColorSpace)
	}
	if profile.MasterDisplayMetadataValue != "" {
		args = append(args, "-master-display", profile.MasterDisplayMetadataValue)
	}
	if profile.MaxContentLightLevel > 0 {
		args = append(args, "-max_cll", fmt.Sprintf("%d,%d", profile.MaxContentLightLevel, profile.MaxFrameAverageLightLevel))
	}
	return args
}

// BuildHdr10PlusInjectionX265ParamsFragment returns the libx265
// "dhdr10-info=<path>" parameter fragment (suitable for appending into an
// "-x265-params" value, comma-joined with any other x265 parameters) that
// instructs x265 to re-inject the given hdr10plus_tool-produced per-frame
// metadata JSON during encode. Returns "" if profile's
// DynamicMetadataKind is not HdrDynamicMetadataHdr10Plus or
// Hdr10PlusMetadataJsonFilePath is empty.
//
// Callers are responsible for having already produced that JSON file via
// a separate hdr10plus_tool extraction step upstream of this
// orchestrator — this function only builds the encoder parameter that
// consumes it; it never invokes hdr10plus_tool itself (see package doc
// comment).
func (profile HdrMetadataProfile) BuildHdr10PlusInjectionX265ParamsFragment() string {
	if profile.DynamicMetadataKind != HdrDynamicMetadataHdr10Plus || profile.Hdr10PlusMetadataJsonFilePath == "" {
		return ""
	}
	return "dhdr10-info=" + profile.Hdr10PlusMetadataJsonFilePath
}

// RequiresExternalDynamicMetadataToolStep reports whether achieving full
// HDR fidelity for this profile requires an external post-processing tool
// step (hdr10plus_tool or dovi_tool) that this codebase does not itself
// implement — see the package doc comment.
func (profile HdrMetadataProfile) RequiresExternalDynamicMetadataToolStep() bool {
	return profile.DynamicMetadataKind != HdrDynamicMetadataNone
}

// hdrMetadataPassthroughCodecArgumentBuilder is the Decorator
// implementation: it wraps an inner CodecArgumentBuilder and splices
// HdrMetadataProfile's static color-metadata flags into the inner
// builder's argument slice, immediately before the trailing output-file
// path argument (every CodecArgumentBuilder implementation in this
// package, software and hardware alike, appends
// spec.OutputMediaFilePath as the final argument, which is what this
// decorator relies on to know where to splice).
type hdrMetadataPassthroughCodecArgumentBuilder struct {
	inner   CodecArgumentBuilder
	profile HdrMetadataProfile
}

// NewHdrMetadataPassthroughCodecArgumentBuilder constructs the Decorator
// that wraps inner and augments every call to
// BuildFfmpegCommandLineArguments with profile's static HDR color
// metadata flags. TargetCodec() delegates unchanged to inner, since
// wrapping for HDR metadata does not change which codec is used.
//
// Wrapping with the zero-value HdrMetadataProfile (i.e. non-HDR content)
// is a documented safe no-op: BuildFfmpegCommandLineArguments then
// returns exactly inner's own output, unmodified.
func NewHdrMetadataPassthroughCodecArgumentBuilder(inner CodecArgumentBuilder, profile HdrMetadataProfile) CodecArgumentBuilder {
	return hdrMetadataPassthroughCodecArgumentBuilder{inner: inner, profile: profile}
}

func (decorator hdrMetadataPassthroughCodecArgumentBuilder) TargetCodec() SupportedVideoCodec {
	return decorator.inner.TargetCodec()
}

func (decorator hdrMetadataPassthroughCodecArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	innerArgs := decorator.inner.BuildFfmpegCommandLineArguments(spec)

	hdrArgs := decorator.profile.BuildStaticColorMetadataArguments()
	if len(hdrArgs) == 0 || len(innerArgs) == 0 {
		return innerArgs
	}

	// Every builder in this package appends the output path as the final
	// element; splice the HDR flags in immediately before it so ffmpeg
	// sees them as output-side options (matching where -color_primaries
	// et al. conventionally belong in a real ffmpeg invocation).
	outputPath := innerArgs[len(innerArgs)-1]
	result := make([]string, 0, len(innerArgs)+len(hdrArgs))
	result = append(result, innerArgs[:len(innerArgs)-1]...)
	result = append(result, hdrArgs...)
	result = append(result, outputPath)
	return result
}
