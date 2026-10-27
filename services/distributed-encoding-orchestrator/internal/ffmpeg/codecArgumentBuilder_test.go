package ffmpeg

import (
	"reflect"
	"testing"
)

// baseSpecFixture returns a representative ChunkEncodeSpecification used
// across the codec argument builder tests.
func baseSpecFixture() ChunkEncodeSpecification {
	return ChunkEncodeSpecification{
		InputMediaFilePath:             "/in/scene0/chunk0.ts",
		OutputMediaFilePath:            "/out/scene0/chunk0_1080p.ts",
		TargetWidthPixels:              1920,
		TargetHeightPixels:             1080,
		TargetBitrateKilobitsPerSecond: 4500,
		EncodePresetName:               "slow",
	}
}

// TestH264ArgumentBuilder_ProducesExpectedArgumentSlice hand-verifies the
// exact ffmpeg argument slice produced for a bitrate-targeted H.264 encode.
func TestH264ArgumentBuilder_ProducesExpectedArgumentSlice(t *testing.T) {
	builder := NewH264ArgumentBuilder()
	if builder.TargetCodec() != CodecH264 {
		t.Fatalf("expected TargetCodec() == CodecH264, got %v", builder.TargetCodec())
	}

	got := builder.BuildFfmpegCommandLineArguments(baseSpecFixture())
	want := []string{
		"-y",
		"-i", "/in/scene0/chunk0.ts",
		"-vf", "scale=1920:1080",
		"-progress", "pipe:2",
		"-nostats",
		"-c:v", "libx264",
		"-profile:v", "high",
		"-pix_fmt", "yuv420p",
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

// TestH264ArgumentBuilder_DefaultsPresetWhenUnset verifies the "medium"
// preset default is applied when EncodePresetName is empty.
func TestH264ArgumentBuilder_DefaultsPresetWhenUnset(t *testing.T) {
	spec := baseSpecFixture()
	spec.EncodePresetName = ""
	got := NewH264ArgumentBuilder().BuildFfmpegCommandLineArguments(spec)
	if !containsConsecutivePair(got, "-preset", "medium") {
		t.Fatalf("expected default preset 'medium' in args: %#v", got)
	}
}

// TestH264ArgumentBuilder_CrfModeOmitsBitrateFlagsWhenBitrateZero verifies
// that CRF-only (quality-constrained) mode is expressible: setting
// ConstantRateFactorQuality with TargetBitrateKilobitsPerSecond == 0
// should include -crf and omit -b:v/-maxrate/-bufsize.
func TestH264ArgumentBuilder_CrfModeOmitsBitrateFlagsWhenBitrateZero(t *testing.T) {
	spec := baseSpecFixture()
	spec.TargetBitrateKilobitsPerSecond = 0
	spec.ConstantRateFactorQuality = 20
	got := NewH264ArgumentBuilder().BuildFfmpegCommandLineArguments(spec)

	if !containsConsecutivePair(got, "-crf", "20") {
		t.Fatalf("expected -crf 20 present: %#v", got)
	}
	if containsFlag(got, "-b:v") {
		t.Fatalf("expected -b:v to be omitted in CRF-only mode: %#v", got)
	}
}

// TestHevcArgumentBuilder_ProducesExpectedArgumentSlice hand-verifies the
// HEVC/libx265 argument slice, including the hvc1 tag critical for Apple
// device compatibility.
func TestHevcArgumentBuilder_ProducesExpectedArgumentSlice(t *testing.T) {
	builder := NewHevcArgumentBuilder()
	if builder.TargetCodec() != CodecHevc {
		t.Fatalf("expected TargetCodec() == CodecHevc, got %v", builder.TargetCodec())
	}

	got := builder.BuildFfmpegCommandLineArguments(baseSpecFixture())
	want := []string{
		"-y",
		"-i", "/in/scene0/chunk0.ts",
		"-vf", "scale=1920:1080",
		"-progress", "pipe:2",
		"-nostats",
		"-c:v", "libx265",
		"-pix_fmt", "yuv420p",
		"-tag:v", "hvc1",
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

// TestAv1ArgumentBuilder_ProducesExpectedArgumentSlice hand-verifies the
// AV1/libaom-av1 argument slice, including row-mt/tiles flags and
// numeric cpu-used parsing from EncodePresetName.
func TestAv1ArgumentBuilder_ProducesExpectedArgumentSlice(t *testing.T) {
	builder := NewAv1ArgumentBuilder()
	if builder.TargetCodec() != CodecAv1 {
		t.Fatalf("expected TargetCodec() == CodecAv1, got %v", builder.TargetCodec())
	}

	spec := baseSpecFixture()
	spec.EncodePresetName = "3" // numeric cpu-used level
	got := builder.BuildFfmpegCommandLineArguments(spec)
	want := []string{
		"-y",
		"-i", "/in/scene0/chunk0.ts",
		"-vf", "scale=1920:1080",
		"-progress", "pipe:2",
		"-nostats",
		"-c:v", "libaom-av1",
		"-pix_fmt", "yuv420p",
		"-row-mt", "1",
		"-tiles", "2x2",
		"-cpu-used", "3",
		"-b:v", "4500k",
		"-maxrate", "4500k",
		"-bufsize", "9000k",
		"/out/scene0/chunk0_1080p.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argument slice mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestAv1ArgumentBuilder_DefaultsCpuUsedWhenPresetNotNumeric verifies the
// documented default cpu-used=4 fallback for non-numeric/unset preset names.
func TestAv1ArgumentBuilder_DefaultsCpuUsedWhenPresetNotNumeric(t *testing.T) {
	spec := baseSpecFixture()
	spec.EncodePresetName = "slow" // not a valid AV1 cpu-used integer
	got := NewAv1ArgumentBuilder().BuildFfmpegCommandLineArguments(spec)
	if !containsConsecutivePair(got, "-cpu-used", "4") {
		t.Fatalf("expected default cpu-used=4 in args: %#v", got)
	}
}

// TestAv1ArgumentBuilder_CrfModeSetsZeroBitrate verifies AV1's
// libaom-specific "-crf N -b:v 0" quality-constrained convention.
func TestAv1ArgumentBuilder_CrfModeSetsZeroBitrate(t *testing.T) {
	spec := baseSpecFixture()
	spec.TargetBitrateKilobitsPerSecond = 0
	spec.ConstantRateFactorQuality = 30
	got := NewAv1ArgumentBuilder().BuildFfmpegCommandLineArguments(spec)
	if !containsConsecutivePair(got, "-crf", "30") {
		t.Fatalf("expected -crf 30: %#v", got)
	}
	if !containsConsecutivePair(got, "-b:v", "0") {
		t.Fatalf("expected -b:v 0 (libaom CRF convention): %#v", got)
	}
}

// TestCodecArgumentBuilderFactory_ReturnsCorrectStrategyPerCodec verifies
// the factory dispatch (the single switch-on-codec point) is correct.
func TestCodecArgumentBuilderFactory_ReturnsCorrectStrategyPerCodec(t *testing.T) {
	cases := []struct {
		codec SupportedVideoCodec
		want  SupportedVideoCodec
	}{
		{CodecH264, CodecH264},
		{CodecHevc, CodecHevc},
		{CodecAv1, CodecAv1},
	}
	for _, tc := range cases {
		builder, err := CodecArgumentBuilderFactory(tc.codec)
		if err != nil {
			t.Fatalf("unexpected error for codec %v: %v", tc.codec, err)
		}
		if builder.TargetCodec() != tc.want {
			t.Fatalf("expected builder for codec %v, got builder targeting %v", tc.want, builder.TargetCodec())
		}
	}
}

// TestCodecArgumentBuilderFactory_UnknownCodecReturnsError verifies the
// factory rejects unsupported codec values rather than panicking or
// silently returning nil.
func TestCodecArgumentBuilderFactory_UnknownCodecReturnsError(t *testing.T) {
	_, err := CodecArgumentBuilderFactory(SupportedVideoCodec(999))
	if err == nil {
		t.Fatal("expected error for unknown codec, got nil")
	}
}

// TestSupportedVideoCodec_StringRendersKnownNames verifies String().
func TestSupportedVideoCodec_StringRendersKnownNames(t *testing.T) {
	if CodecH264.String() != "h264" {
		t.Errorf("got %q", CodecH264.String())
	}
	if CodecHevc.String() != "hevc" {
		t.Errorf("got %q", CodecHevc.String())
	}
	if CodecAv1.String() != "av1" {
		t.Errorf("got %q", CodecAv1.String())
	}
	if SupportedVideoCodec(999).String() != "unknown" {
		t.Errorf("got %q", SupportedVideoCodec(999).String())
	}
}

// containsConsecutivePair reports whether args contains flag immediately
// followed by value at some index i, i+1.
func containsConsecutivePair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// containsFlag reports whether args contains the exact string flag
// anywhere.
func containsFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
