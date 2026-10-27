package ffmpeg

import (
	"reflect"
	"testing"
)

// hdr10Profile returns a representative HDR10 (static-only) profile fixture.
func hdr10ProfileFixture() HdrMetadataProfile {
	return HdrMetadataProfile{
		ColorPrimaries:             "bt2020",
		TransferCharacteristic:     HdrTransferFunctionSmpte2084Pq,
		ColorSpace:                 "bt2020nc",
		MasterDisplayMetadataValue: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)",
		MaxContentLightLevel:       1000,
		MaxFrameAverageLightLevel:  400,
	}
}

// TestHdrMetadataPassthroughCodecArgumentBuilder_SplicesStaticColorFlagsBeforeOutputPath
// hand-verifies the exact spliced argument slice for an HDR10 profile
// wrapping the H.264 software builder.
func TestHdrMetadataPassthroughCodecArgumentBuilder_SplicesStaticColorFlagsBeforeOutputPath(t *testing.T) {
	inner := NewH264ArgumentBuilder()
	decorated := NewHdrMetadataPassthroughCodecArgumentBuilder(inner, hdr10ProfileFixture())

	if decorated.TargetCodec() != CodecH264 {
		t.Fatalf("expected TargetCodec() to delegate to inner (CodecH264), got %v", decorated.TargetCodec())
	}

	got := decorated.BuildFfmpegCommandLineArguments(baseSpecFixture())
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
		"-color_primaries", "bt2020",
		"-color_trc", "smpte2084",
		"-colorspace", "bt2020nc",
		"-master-display", "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)",
		"-max_cll", "1000,400",
		"/out/scene0/chunk0_1080p.ts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argument slice mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestHdrMetadataPassthroughCodecArgumentBuilder_ZeroValueProfileIsNoOp
// verifies wrapping with the zero-value (non-HDR) HdrMetadataProfile
// leaves the inner builder's argument slice byte-for-byte unchanged, per
// the documented safe no-op contract.
func TestHdrMetadataPassthroughCodecArgumentBuilder_ZeroValueProfileIsNoOp(t *testing.T) {
	inner := NewHevcArgumentBuilder()
	decorated := NewHdrMetadataPassthroughCodecArgumentBuilder(inner, HdrMetadataProfile{})

	spec := baseSpecFixture()
	want := inner.BuildFfmpegCommandLineArguments(spec)
	got := decorated.BuildFfmpegCommandLineArguments(spec)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected zero-value profile to be a no-op:\n got: %#v\nwant: %#v", got, want)
	}
}

// TestHdrMetadataPassthroughCodecArgumentBuilder_WrapsHardwareBuildersToo
// verifies the decorator composes with a hardware CodecArgumentBuilder
// (NVENC) just as it does with software builders, proving the Decorator
// pattern is orthogonal to the Strategy choice of encoder.
func TestHdrMetadataPassthroughCodecArgumentBuilder_WrapsHardwareBuildersToo(t *testing.T) {
	inner := NewNvencArgumentBuilder()
	profile := HdrMetadataProfile{
		ColorPrimaries:         "bt2020",
		TransferCharacteristic: HdrTransferFunctionAribStdB67Hlg,
		ColorSpace:             "bt2020nc",
	}
	decorated := NewHdrMetadataPassthroughCodecArgumentBuilder(inner, profile)

	if decorated.TargetCodec() != CodecH264Nvenc {
		t.Fatalf("expected TargetCodec()==CodecH264Nvenc, got %v", decorated.TargetCodec())
	}

	got := decorated.BuildFfmpegCommandLineArguments(baseSpecFixture())
	last := got[len(got)-1]
	if last != "/out/scene0/chunk0_1080p.ts" {
		t.Fatalf("expected output path to remain the final argument, got %q", last)
	}
	if !containsConsecutivePair(got, "-color_trc", "arib-std-b67") {
		t.Fatalf("expected HLG -color_trc flag present: %#v", got)
	}
	if !containsConsecutivePair(got, "-hwaccel", "cuda") {
		t.Fatalf("expected inner NVENC builder's -hwaccel cuda to be preserved: %#v", got)
	}
}

// TestHdrMetadataProfile_BuildStaticColorMetadataArguments_OmitsOptionalFieldsWhenUnset
// verifies MasterDisplayMetadataValue and MaxContentLightLevel are only
// emitted when set, and ColorPrimaries/ColorSpace are only emitted when
// non-empty, leaving only the mandatory -color_trc for a minimal HDR
// profile.
func TestHdrMetadataProfile_BuildStaticColorMetadataArguments_OmitsOptionalFieldsWhenUnset(t *testing.T) {
	profile := HdrMetadataProfile{TransferCharacteristic: HdrTransferFunctionSmpte2084Pq}
	got := profile.BuildStaticColorMetadataArguments()
	want := []string{"-color_trc", "smpte2084"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected minimal HDR args %#v, got %#v", want, got)
	}
}

// TestHdrMetadataProfile_BuildStaticColorMetadataArguments_NonHdrReturnsEmptySlice
// verifies the zero-value profile (SDR/non-HDR) returns no flags.
func TestHdrMetadataProfile_BuildStaticColorMetadataArguments_NonHdrReturnsEmptySlice(t *testing.T) {
	got := HdrMetadataProfile{}.BuildStaticColorMetadataArguments()
	if len(got) != 0 {
		t.Fatalf("expected no flags for non-HDR profile, got %#v", got)
	}
}

// TestHdrMetadataProfile_BuildHdr10PlusInjectionX265ParamsFragment verifies
// the dhdr10-info fragment is only produced for HDR10+ dynamic metadata
// with a non-empty JSON path, and documents (via the honest boundary
// comment on the source type) that this fragment merely instructs x265 to
// consume an already-extracted file rather than performing extraction
// itself.
func TestHdrMetadataProfile_BuildHdr10PlusInjectionX265ParamsFragment(t *testing.T) {
	hdr10PlusProfile := HdrMetadataProfile{
		TransferCharacteristic:        HdrTransferFunctionSmpte2084Pq,
		DynamicMetadataKind:           HdrDynamicMetadataHdr10Plus,
		Hdr10PlusMetadataJsonFilePath: "/metadata/scene0-hdr10plus.json",
	}
	got := hdr10PlusProfile.BuildHdr10PlusInjectionX265ParamsFragment()
	want := "dhdr10-info=/metadata/scene0-hdr10plus.json"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}

	if !hdr10PlusProfile.RequiresExternalDynamicMetadataToolStep() {
		t.Fatal("expected HDR10+ profile to require an external dynamic-metadata tool step")
	}

	dolbyVisionProfile := HdrMetadataProfile{DynamicMetadataKind: HdrDynamicMetadataDolbyVisionRpu}
	if got := dolbyVisionProfile.BuildHdr10PlusInjectionX265ParamsFragment(); got != "" {
		t.Fatalf("expected empty fragment for a Dolby Vision (non-HDR10+) profile, got %q", got)
	}
	if !dolbyVisionProfile.RequiresExternalDynamicMetadataToolStep() {
		t.Fatal("expected Dolby Vision profile to require an external dynamic-metadata tool step")
	}

	staticOnlyProfile := hdr10ProfileFixture()
	if staticOnlyProfile.RequiresExternalDynamicMetadataToolStep() {
		t.Fatal("expected a static-only HDR10 profile to NOT require an external dynamic-metadata tool step")
	}
}

// TestHdrDynamicMetadataKind_StringRendersExpectedLabels verifies the
// small String helper used in logs/reports.
func TestHdrDynamicMetadataKind_StringRendersExpectedLabels(t *testing.T) {
	cases := map[HdrDynamicMetadataKind]string{
		HdrDynamicMetadataNone:           "none",
		HdrDynamicMetadataHdr10Plus:      "hdr10plus",
		HdrDynamicMetadataDolbyVisionRpu: "dolby_vision_rpu",
		HdrDynamicMetadataKind(99):       "unknown",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Fatalf("HdrDynamicMetadataKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}
