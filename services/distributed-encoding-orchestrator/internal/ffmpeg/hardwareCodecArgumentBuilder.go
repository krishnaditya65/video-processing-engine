// This file extends the Strategy pattern documented in
// codecArgumentBuilder.go with two hardware-accelerated
// CodecArgumentBuilder implementations: NvencArgumentBuilder (NVIDIA
// NVENC, via ffmpeg's "-hwaccel cuda -c:v h264_nvenc") and
// QuickSyncArgumentBuilder (Intel Quick Sync Video, via ffmpeg's
// "-hwaccel qsv -c:v h264_qsv"). Both implement exactly the same
// CodecArgumentBuilder interface as the software strategies in
// codecArgumentBuilder.go, so worker.FfmpegBackedChunkEncodingTaskProcessor
// and CodecArgumentBuilderFactory never need to know or care whether a
// given ChunkEncodingTask will run on CPU or GPU — the only thing that
// changes is which ffmpeg.SupportedVideoCodec value the owning
// domain.EncodingJob was built with.
//
// # Why hardware encode matters here
//
// Fixed-function hardware encoders trade some compression efficiency
// (typically a modestly worse bitrate-for-equivalent-VMAF ratio than a
// well-tuned libx264/libx265 "slow"/"medium" preset) for dramatically
// higher throughput and lower CPU utilization per encode — often 10-50x
// faster wall-clock than a software encode of the same content. That
// throughput/quality/cost trade-off is exactly what internal/cost's
// CostAccountant is for: it lets an operator compare a title's actual
// GPU-second cost against what the same title would have cost in
// CPU-seconds on a software strategy.
package ffmpeg

import (
	"strconv"
)

// buildCommonHardwareInputAndScalingArguments mirrors
// buildCommonInputAndScalingArguments but additionally prepends the
// "-hwaccel <api>" flag, which real ffmpeg requires to appear BEFORE
// "-i" (unlike codec-selection flags, which apply to the output side and
// may appear after "-i"), so hardware and software builders cannot share
// a single common-arguments helper.
func buildCommonHardwareInputAndScalingArguments(hwaccelApiName string, spec ChunkEncodeSpecification) []string {
	args := []string{"-y"}
	if hwaccelApiName != "" {
		args = append(args, "-hwaccel", hwaccelApiName)
	}
	args = append(args,
		"-i", spec.InputMediaFilePath,
		"-vf", "scale="+strconv.Itoa(spec.TargetWidthPixels)+":"+strconv.Itoa(spec.TargetHeightPixels),
		"-progress", "pipe:2",
		"-nostats",
	)
	return args
}

// nvencArgumentBuilder implements CodecArgumentBuilder for H.264
// hardware-accelerated encoding via NVIDIA's NVENC engine. Stateless;
// safe for concurrent use.
type nvencArgumentBuilder struct{}

// NewNvencArgumentBuilder constructs the CodecArgumentBuilder strategy
// for H.264 encodes via ffmpeg's h264_nvenc encoder, requesting CUDA
// hardware-accelerated decode/scale ("-hwaccel cuda") to keep the whole
// pipeline off the CPU where possible.
//
// Rate control: NVENC's ffmpeg wrapper exposes "-rc" to select the
// rate-control mode; this builder selects "vbr" (variable bitrate,
// NVENC's throughput-friendly default for VOD encoding) whenever a
// target bitrate is supplied, or "constqp" (constant quantization
// parameter, NVENC's CRF-equivalent) via "-cq" when
// ChunkEncodeSpecification.ConstantRateFactorQuality is set — mirroring
// how the software builders treat ConstantRateFactorQuality as an
// independent, coexistable quality control alongside bitrate targeting.
func NewNvencArgumentBuilder() CodecArgumentBuilder { return nvencArgumentBuilder{} }

func (nvencArgumentBuilder) TargetCodec() SupportedVideoCodec { return CodecH264Nvenc }

func (nvencArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	args := buildCommonHardwareInputAndScalingArguments("cuda", spec)
	args = append(args,
		"-c:v", "h264_nvenc",
		"-pix_fmt", "yuv420p",
	)

	preset := spec.EncodePresetName
	if preset == "" {
		preset = "p4" // NVENC's numeric preset scale (p1 fastest/lowest-quality .. p7 slowest/highest-quality); p4 is the balanced default.
	}
	args = append(args, "-preset", preset)

	if spec.ConstantRateFactorQuality > 0 {
		args = append(args, "-rc", "constqp", "-cq", strconv.Itoa(spec.ConstantRateFactorQuality))
	} else if spec.TargetBitrateKilobitsPerSecond > 0 {
		bitrateArg := strconv.Itoa(spec.TargetBitrateKilobitsPerSecond) + "k"
		args = append(args,
			"-rc", "vbr",
			"-b:v", bitrateArg,
			"-maxrate", bitrateArg,
			"-bufsize", strconv.Itoa(spec.TargetBitrateKilobitsPerSecond*2)+"k",
		)
	}
	args = append(args, spec.OutputMediaFilePath)
	return args
}

// quickSyncArgumentBuilder implements CodecArgumentBuilder for H.264
// hardware-accelerated encoding via Intel Quick Sync Video. Stateless;
// safe for concurrent use.
type quickSyncArgumentBuilder struct{}

// NewQuickSyncArgumentBuilder constructs the CodecArgumentBuilder
// strategy for H.264 encodes via ffmpeg's h264_qsv encoder, requesting
// the QSV hardware-acceleration API ("-hwaccel qsv") for
// decode/scale/encode to stay on Intel's fixed-function media engine
// throughout.
//
// Rate control: Quick Sync's ffmpeg wrapper reuses the conventional
// "-b:v"/"-maxrate"/"-bufsize" bitrate flags (same as the software
// builders) when a target bitrate is supplied, and
// ConstantRateFactorQuality maps to "-global_quality" (Quick Sync's
// ICQ/constant-quality control, numerically on the same 0-51 scale as
// x264/x265's CRF) when set.
func NewQuickSyncArgumentBuilder() CodecArgumentBuilder { return quickSyncArgumentBuilder{} }

func (quickSyncArgumentBuilder) TargetCodec() SupportedVideoCodec { return CodecH264QuickSync }

func (quickSyncArgumentBuilder) BuildFfmpegCommandLineArguments(spec ChunkEncodeSpecification) []string {
	args := buildCommonHardwareInputAndScalingArguments("qsv", spec)
	args = append(args,
		"-c:v", "h264_qsv",
		"-pix_fmt", "nv12", // Quick Sync's native surface format; avoids an extra CPU-side color-format conversion.
	)

	preset := spec.EncodePresetName
	if preset == "" {
		preset = "medium"
	}
	args = append(args, "-preset", preset)

	if spec.ConstantRateFactorQuality > 0 {
		args = append(args, "-global_quality", strconv.Itoa(spec.ConstantRateFactorQuality))
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
