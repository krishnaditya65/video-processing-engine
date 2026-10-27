// This file implements the Strategy design pattern for building
// codec-specific ffmpeg CLI argument slices. Each supported codec
// (H.264, HEVC, AV1) gets its own CodecArgumentBuilder implementation
// encapsulating that codec's specific encoder name, rate-control flags,
// and preset conventions, while the calling code (FfmpegSubprocessInvoker
// in codecArgumentBuilder_invoker.go / worker package) depends only on
// the common CodecArgumentBuilder interface and never branches on codec
// type itself.
package ffmpeg

import (
	"fmt"
	"strconv"
)

// SupportedVideoCodec enumerates the video codecs this orchestrator knows
// how to build ffmpeg arguments for.
type SupportedVideoCodec int

const (
	// CodecH264 selects the H.264/AVC codec via libx264.
	CodecH264 SupportedVideoCodec = iota
	// CodecHevc selects the H.265/HEVC codec via libx265.
	CodecHevc
	// CodecAv1 selects the AV1 codec via libaom-av1.
	CodecAv1
	// CodecH264Nvenc selects H.264 hardware-accelerated encoding via
	// NVIDIA's NVENC engine (ffmpeg's h264_nvenc encoder), built by
	// NvencArgumentBuilder in hardwareCodecArgumentBuilder.go.
	CodecH264Nvenc
	// CodecH264QuickSync selects H.264 hardware-accelerated encoding via
	// Intel Quick Sync Video (ffmpeg's h264_qsv encoder), built by
	// QuickSyncArgumentBuilder in hardwareCodecArgumentBuilder.go.
	CodecH264QuickSync
)

// String renders a SupportedVideoCodec as its canonical short name.
func (codec SupportedVideoCodec) String() string {
	switch codec {
	case CodecH264:
		return "h264"
	case CodecHevc:
		return "hevc"
	case CodecAv1:
		return "av1"
	case CodecH264Nvenc:
		return "h264_nvenc"
	case CodecH264QuickSync:
		return "h264_qsv"
	default:
		return "unknown"
	}
}

// IsHardwareAccelerated reports whether codec is one of the GPU/fixed-
// function hardware encoder strategies (NVENC, Quick Sync) rather than a
// CPU/software encoder strategy (H.264/HEVC/AV1 via libx264/libx265/
// libaom-av1). internal/cost's CostAccountant uses this to decide whether
// a completed ChunkEncodingTask's compute time should be billed at the
// CPU-second or GPU-second rate.
func (codec SupportedVideoCodec) IsHardwareAccelerated() bool {
	switch codec {
	case CodecH264Nvenc, CodecH264QuickSync:
		return true
	default:
		return false
	}
}

// ChunkEncodeSpecification fully describes one scene-chunk encode job at
// the level of detail needed to build an ffmpeg command line: the input
// media, desired codec/resolution/bitrate rung, and output location. It
// is intentionally decoupled from the domain package's richer
// ChunkEncodingTask type so this package has no import-cycle dependency
// on domain and can be tested in complete isolation.
type ChunkEncodeSpecification struct {
	// InputMediaFilePath is the path (or, in a real deployment, a
	// pre-signed/streamable URI ffmpeg can read) to the source chunk.
	InputMediaFilePath string
	// OutputMediaFilePath is where ffmpeg should write the encoded chunk.
	OutputMediaFilePath string
	// TargetWidthPixels and TargetHeightPixels give the output
	// resolution rung, e.g. 1920x1080.
	TargetWidthPixels  int
	TargetHeightPixels int
	// TargetBitrateKilobitsPerSecond is the desired average video
	// bitrate in kbps, typically a Pareto-optimal rung selected by the
	// ladder package.
	TargetBitrateKilobitsPerSecond int
	// ConstantRateFactorQuality, if non-zero, requests
	// quality-constrained ("CRF") encoding instead of/alongside a
	// bitrate cap, following each codec's own CRF numeric range
	// conventions (0-51 for x264/x265, 0-63 for aom-av1). A value of 0
	// means "not requested"; callers that want CRF 0 (lossless, rarely
	// desired) should treat that as a documented limitation of this
	// struct rather than force it through this field.
	ConstantRateFactorQuality int
	// EncodePresetName selects the encoder's speed/efficiency preset,
	// e.g. "medium", "slow" for x264/x265, or a numeric "cpu-used"
	// equivalent handled internally for AV1.
	EncodePresetName string
}

// CodecArgumentBuilder is the Strategy interface implemented once per
// supported SupportedVideoCodec. Each implementation encapsulates
// exactly the encoder-specific knowledge needed to translate a
// codec-agnostic ChunkEncodeSpecification into the concrete ffmpeg CLI
// argument slice for that codec.
//
// Implementations must be pure/stateless functions of their input (no
// I/O, no shared mutable state) so a single implementation instance can
// be safely reused and called concurrently by many worker goroutines.
type CodecArgumentBuilder interface {
	// BuildFfmpegCommandLineArguments returns the full ordered argument
	// slice that should follow the "ffmpeg" binary name itself (i.e.
	// exec.Command("ffmpeg", result...)) to perform the encode described
	// by spec, including "-progress pipe:2" so progress lines are
	// interleavable on stderr for ProgressParser to consume.
	BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string

	// TargetCodec identifies which SupportedVideoCodec this builder
	// implements, used for logging and for the CodecArgumentBuilderFactory.
	TargetCodec() SupportedVideoCodec
}

// buildCommonInputAndScalingArguments returns the argument fragments
// shared by every codec strategy: input file, output resolution scaling
// filter, and the progress-reporting flag. Codec-specific builders
// prepend this to their own encoder-selection and rate-control flags.
func buildCommonInputAndScalingArguments(spec ChunkEncodeSpecification) []string {
	return []string{
		"-y",
		"-i", spec.InputMediaFilePath,
		"-vf", fmt.Sprintf("scale=%d:%d", spec.TargetWidthPixels, spec.TargetHeightPixels),
		"-progress", "pipe:2",
		"-nostats",
	}
}

// h264ArgumentBuilder implements CodecArgumentBuilder for H.264/AVC via
// libx264. Stateless; safe for concurrent use.
type h264ArgumentBuilder struct{}

// NewH264ArgumentBuilder constructs the CodecArgumentBuilder strategy for
// H.264/AVC encodes via libx264, using the "high" profile and "yuv420p"
// pixel format conventional for broad playback-device compatibility.
func NewH264ArgumentBuilder() CodecArgumentBuilder { return h264ArgumentBuilder{} }

func (h264ArgumentBuilder) TargetCodec() SupportedVideoCodec { return CodecH264 }

func (h264ArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	args := buildCommonInputAndScalingArguments(spec)
	args = append(args,
		"-c:v", "libx264",
		"-profile:v", "high",
		"-pix_fmt", "yuv420p",
	)
	preset := spec.EncodePresetName
	if preset == "" {
		preset = "medium"
	}
	args = append(args, "-preset", preset)

	if spec.ConstantRateFactorQuality > 0 {
		args = append(args, "-crf", strconv.Itoa(spec.ConstantRateFactorQuality))
	}
	if spec.TargetBitrateKilobitsPerSecond > 0 {
		bitrateArg := strconv.Itoa(spec.TargetBitrateKilobitsPerSecond) + "k"
		args = append(args,
			"-b:v", bitrateArg,
			"-maxrate", bitrateArg,
			"-bufsize", strconv.Itoa(spec.TargetBitrateKilobitsPerSecond*2)+"k",
		)
	}
	args = append(args, spec.OutputMediaFilePath)
	return args
}

// hevcArgumentBuilder implements CodecArgumentBuilder for H.265/HEVC via
// libx265. Stateless; safe for concurrent use.
type hevcArgumentBuilder struct{}

// NewHevcArgumentBuilder constructs the CodecArgumentBuilder strategy for
// H.265/HEVC encodes via libx265, emitting the "main" profile and 10-bit
// friendly tag handling conventions used by HDR-capable delivery
// pipelines (bit depth itself is controlled upstream by the source; this
// builder only sets container/tagging flags that matter regardless of
// bit depth).
func NewHevcArgumentBuilder() CodecArgumentBuilder { return hevcArgumentBuilder{} }

func (hevcArgumentBuilder) TargetCodec() SupportedVideoCodec { return CodecHevc }

func (hevcArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	args := buildCommonInputAndScalingArguments(spec)
	args = append(args,
		"-c:v", "libx265",
		"-pix_fmt", "yuv420p",
		"-tag:v", "hvc1",
	)
	preset := spec.EncodePresetName
	if preset == "" {
		preset = "medium"
	}
	args = append(args, "-preset", preset)

	if spec.ConstantRateFactorQuality > 0 {
		args = append(args, "-crf", strconv.Itoa(spec.ConstantRateFactorQuality))
	}
	if spec.TargetBitrateKilobitsPerSecond > 0 {
		bitrateArg := strconv.Itoa(spec.TargetBitrateKilobitsPerSecond) + "k"
		args = append(args,
			"-b:v", bitrateArg,
			"-maxrate", bitrateArg,
			"-bufsize", strconv.Itoa(spec.TargetBitrateKilobitsPerSecond*2)+"k",
		)
	}
	args = append(args, spec.OutputMediaFilePath)
	return args
}

// av1ArgumentBuilder implements CodecArgumentBuilder for AV1 via
// libaom-av1. Stateless; safe for concurrent use.
type av1ArgumentBuilder struct{}

// NewAv1ArgumentBuilder constructs the CodecArgumentBuilder strategy for
// AV1 encodes via libaom-av1. AV1 encoding via libaom is CPU-intensive,
// so this builder sets "-cpu-used" (aom's speed/efficiency trade-off
// knob, 0=slowest/best to 8=fastest) derived from EncodePresetName when
// it parses as an integer 0-8, defaulting to 4 (a balanced middle
// setting) otherwise, and enables row-based multithreading and tiling
// flags appropriate for fleet-scale parallel encoding.
func NewAv1ArgumentBuilder() CodecArgumentBuilder { return av1ArgumentBuilder{} }

func (av1ArgumentBuilder) TargetCodec() SupportedVideoCodec { return CodecAv1 }

func (av1ArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	args := buildCommonInputAndScalingArguments(spec)
	args = append(args,
		"-c:v", "libaom-av1",
		"-pix_fmt", "yuv420p",
		"-row-mt", "1",
		"-tiles", "2x2",
	)

	cpuUsedLevel := 4
	if parsed, err := strconv.Atoi(spec.EncodePresetName); err == nil && parsed >= 0 && parsed <= 8 {
		cpuUsedLevel = parsed
	}
	args = append(args, "-cpu-used", strconv.Itoa(cpuUsedLevel))

	if spec.ConstantRateFactorQuality > 0 {
		args = append(args, "-crf", strconv.Itoa(spec.ConstantRateFactorQuality), "-b:v", "0")
	} else if spec.TargetBitrateKilobitsPerSecond > 0 {
		bitrateArg := strconv.Itoa(spec.TargetBitrateKilobitsPerSecond) + "k"
		args = append(args,
			"-b:v", bitrateArg,
			"-maxrate", bitrateArg,
			"-bufsize", strconv.Itoa(spec.TargetBitrateKilobitsPerSecond*2)+"k",
		)
	}
	args = append(args, spec.OutputMediaFilePath)
	return args
}

// CodecArgumentBuilderFactory maps a SupportedVideoCodec to its
// CodecArgumentBuilder strategy implementation. This is the single
// switch-on-codec-type point in the codebase; every other consumer works
// exclusively through the CodecArgumentBuilder interface.
//
// Returns an error if targetCodec is not one of the known
// SupportedVideoCodec constants.
func CodecArgumentBuilderFactory(targetCodec SupportedVideoCodec) (CodecArgumentBuilder, error) {
	switch targetCodec {
	case CodecH264:
		return NewH264ArgumentBuilder(), nil
	case CodecHevc:
		return NewHevcArgumentBuilder(), nil
	case CodecAv1:
		return NewAv1ArgumentBuilder(), nil
	case CodecH264Nvenc:
		return NewNvencArgumentBuilder(), nil
	case CodecH264QuickSync:
		return NewQuickSyncArgumentBuilder(), nil
	default:
		return nil, fmt.Errorf("ffmpeg: unsupported video codec %v", targetCodec)
	}
}
