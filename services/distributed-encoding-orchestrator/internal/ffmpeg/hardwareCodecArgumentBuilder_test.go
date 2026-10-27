package ffmpeg

import (
	"reflect"
	"testing"
)

// TestNvencArgumentBuilder_ProducesExpectedArgumentSlice hand-verifies
// the exact ffmpeg argument slice produced for a bitrate-targeted NVENC
// encode, including the "-hwaccel cuda" flag appearing before "-i" and
// "-rc vbr" rate-control selection.
func TestNvencArgumentBuilder_ProducesExpectedArgumentSlice(t *testing.T) {
	builder := NewNvencArgumentBuilder()
	if builder.TargetCodec() != CodecH264Nvenc {
		t.Fatalf("expected TargetCodec()==CodecH264Nvenc, got %v", builder.TargetCodec())
	}
	if !builder.TargetCodec().IsHardwareAccelerated() {
		t.Fatal("expected CodecH264Nvenc.IsHardwareAccelerated()==true")
	}

	got := builder.BuildFfmpegCommandLineArguments(baseSpecFixture())
	want := []string{
		"-y",
		"-hwaccel", "cuda",
		"-i", "/in/scene0/chunk0.ts",
		"-vf", "scale=1920:1080",
		"-progress", "pipe:2",
		"-nostats",
		"-c:v", "h264_nvenc",
		"-pix_fmt", "yuv420p",
		"-preset", "slow",
		"-rc", "vbr",
		"-b:v", "4500k",
		"-maxrate", "4500k",
		"-bufsize", "9000k",
		"/out/scene0/chunk0_1080p.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argument slice mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestNvencArgumentBuilder_DefaultsPresetWhenUnset verifies the NVENC
// "p4" balanced default preset is applied when EncodePresetName is empty.
func TestNvencArgumentBuilder_DefaultsPresetWhenUnset(t *testing.T) {
	spec := baseSpecFixture()
	spec.EncodePresetName = ""
	got := NewNvencArgumentBuilder().BuildFfmpegCommandLineArguments(spec)
	if !containsConsecutivePair(got, "-preset", "p4") {
		t.Fatalf("expected default preset 'p4' in args: %#v", got)
	}
}

// TestNvencArgumentBuilder_ConstantQualityModeOmitsBitrateFlags verifies
// that setting ConstantRateFactorQuality with TargetBitrateKilobitsPerSecond==0
// selects NVENC's constqp rate-control mode via "-rc constqp -cq N" and
// omits the VBR bitrate flags.
func TestNvencArgumentBuilder_ConstantQualityModeOmitsBitrateFlags(t *testing.T) {
	spec := baseSpecFixture()
	spec.TargetBitrateKilobitsPerSecond = 0
	spec.ConstantRateFactorQuality = 23
	got := NewNvencArgumentBuilder().BuildFfmpegCommandLineArguments(spec)

	if !containsConsecutivePair(got, "-rc", "constqp") {
		t.Fatalf("expected -rc constqp: %#v", got)
	}
	if !containsConsecutivePair(got, "-cq", "23") {
		t.Fatalf("expected -cq 23: %#v", got)
	}
	if containsFlag(got, "-b:v") {
		t.Fatalf("expected -b:v to be omitted in constqp mode: %#v", got)
	}
	if containsFlag(got, "-rc") && containsConsecutivePair(got, "-rc", "vbr") {
		t.Fatalf("did not expect -rc vbr in constqp mode: %#v", got)
	}
}

// TestQuickSyncArgumentBuilder_ProducesExpectedArgumentSlice hand-verifies
// the exact ffmpeg argument slice produced for a bitrate-targeted Quick
// Sync encode, including the "-hwaccel qsv" flag before "-i".
func TestQuickSyncArgumentBuilder_ProducesExpectedArgumentSlice(t *testing.T) {
	builder := NewQuickSyncArgumentBuilder()
	if builder.TargetCodec() != CodecH264QuickSync {
		t.Fatalf("expected TargetCodec()==CodecH264QuickSync, got %v", builder.TargetCodec())
	}
	if !builder.TargetCodec().IsHardwareAccelerated() {
		t.Fatal("expected CodecH264QuickSync.IsHardwareAccelerated()==true")
	}

	got := builder.BuildFfmpegCommandLineArguments(baseSpecFixture())
	want := []string{
		"-y",
		"-hwaccel", "qsv",
		"-i", "/in/scene0/chunk0.ts",
		"-vf", "scale=1920:1080",
		"-progress", "pipe:2",
		"-nostats",
		"-c:v", "h264_qsv",
		"-pix_fmt", "nv12",
		"-preset", "slow",
		"-b:v", "4500k",
		"-maxrate", "4500k",
		"-bufsize", "9000k",
		"/out/scene0/chunk0_1080p.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argument slice mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestQuickSyncArgumentBuilder_DefaultsPresetWhenUnset verifies the
// "medium" default preset is applied when EncodePresetName is empty.
func TestQuickSyncArgumentBuilder_DefaultsPresetWhenUnset(t *testing.T) {
	spec := baseSpecFixture()
	spec.EncodePresetName = ""
	got := NewQuickSyncArgumentBuilder().BuildFfmpegCommandLineArguments(spec)
	if !containsConsecutivePair(got, "-preset", "medium") {
		t.Fatalf("expected default preset 'medium' in args: %#v", got)
	}
}

// TestQuickSyncArgumentBuilder_ConstantQualityModeUsesGlobalQualityFlag
// verifies CRF-equivalent mode maps to "-global_quality N" and omits
// bitrate flags.
func TestQuickSyncArgumentBuilder_ConstantQualityModeUsesGlobalQualityFlag(t *testing.T) {
	spec := baseSpecFixture()
	spec.TargetBitrateKilobitsPerSecond = 0
	spec.ConstantRateFactorQuality = 20
	got := NewQuickSyncArgumentBuilder().BuildFfmpegCommandLineArguments(spec)

	if !containsConsecutivePair(got, "-global_quality", "20") {
		t.Fatalf("expected -global_quality 20: %#v", got)
	}
	if containsFlag(got, "-b:v") {
		t.Fatalf("expected -b:v to be omitted in global_quality mode: %#v", got)
	}
}

// TestCodecArgumentBuilderFactory_ResolvesHardwareStrategies verifies the
// factory switch-point resolves the two new hardware codec constants to
// the correct concrete builder.
func TestCodecArgumentBuilderFactory_ResolvesHardwareStrategies(t *testing.T) {
	nvencBuilder, err := CodecArgumentBuilderFactory(CodecH264Nvenc)
	if err != nil {
		t.Fatalf("unexpected error resolving CodecH264Nvenc: %v", err)
	}
	if nvencBuilder.TargetCodec() != CodecH264Nvenc {
		t.Fatalf("expected resolved builder TargetCodec()==CodecH264Nvenc, got %v", nvencBuilder.TargetCodec())
	}

	qsvBuilder, err := CodecArgumentBuilderFactory(CodecH264QuickSync)
	if err != nil {
		t.Fatalf("unexpected error resolving CodecH264QuickSync: %v", err)
	}
	if qsvBuilder.TargetCodec() != CodecH264QuickSync {
		t.Fatalf("expected resolved builder TargetCodec()==CodecH264QuickSync, got %v", qsvBuilder.TargetCodec())
	}
}

// TestSupportedVideoCodec_IsHardwareAcceleratedDistinguishesSoftwareCodecs
// verifies the software codecs correctly report false.
func TestSupportedVideoCodec_IsHardwareAcceleratedDistinguishesSoftwareCodecs(t *testing.T) {
	for _, codec := range []SupportedVideoCodec{CodecH264, CodecHevc, CodecAv1} {
		if codec.IsHardwareAccelerated() {
			t.Fatalf("expected %v.IsHardwareAccelerated()==false", codec)
		}
	}
}
