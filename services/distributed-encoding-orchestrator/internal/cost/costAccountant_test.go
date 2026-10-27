package cost

import "testing"

// TestCostAccountant_RecordChunkCompletion_CpuArithmeticMatchesHandComputedValue
// hand-verifies the core CPU-cost arithmetic:
// EstimatedCostUsd = seconds * rate.
func TestCostAccountant_RecordChunkCompletion_CpuArithmeticMatchesHandComputedValue(t *testing.T) {
	rates := ComputeResourceRates{CpuSecondPriceUsd: 0.00003, GpuSecondPriceUsd: 0.0003}
	accountant := NewCostAccountant(rates)

	record := accountant.RecordChunkCompletion("job-1", "job-1/scene0/chunk0/1080p", ResourceKindCpu, 15.0)

	// 15 CPU-seconds * $0.00003/CPU-second = $0.00045.
	wantCost := 15.0 * 0.00003
	if record.EstimatedCostUsd != wantCost {
		t.Fatalf("expected EstimatedCostUsd=%v, got %v", wantCost, record.EstimatedCostUsd)
	}
	if record.ResourceKind != ResourceKindCpu {
		t.Fatalf("expected ResourceKindCpu, got %v", record.ResourceKind)
	}
}

// TestCostAccountant_RecordChunkCompletion_GpuArithmeticMatchesHandComputedValue
// verifies the GPU rate is applied instead of the CPU rate for GPU
// resource records.
func TestCostAccountant_RecordChunkCompletion_GpuArithmeticMatchesHandComputedValue(t *testing.T) {
	rates := ComputeResourceRates{CpuSecondPriceUsd: 0.00003, GpuSecondPriceUsd: 0.0003}
	accountant := NewCostAccountant(rates)

	record := accountant.RecordChunkCompletion("job-1", "job-1/scene0/chunk0/1080p-nvenc", ResourceKindGpu, 2.0)

	// NVENC finishes the same chunk ~7.5x faster (2s vs 15s) but the
	// per-second GPU rate is 10x the CPU rate in this fixture, so the
	// per-chunk GPU cost ($0.0006) is actually *higher* than the CPU cost
	// ($0.00045) despite the shorter wall-clock time — exactly the kind of
	// real trade-off a TitleCostReport should surface to an operator
	// deciding whether hardware encode is worth it for a given title.
	wantCost := 2.0 * 0.0003
	if record.EstimatedCostUsd != wantCost {
		t.Fatalf("expected EstimatedCostUsd=%v, got %v", wantCost, record.EstimatedCostUsd)
	}
}

// TestCostAccountant_TitleCostReportFor_AggregatesMultipleChunksAcrossResourceKinds
// hand-verifies aggregation arithmetic across several chunks, mixing CPU
// and GPU resource kinds within the same title (a realistic scenario: a
// title's SDR renditions are encoded on CPU while its top HDR rung is
// encoded on a GPU node for speed).
func TestCostAccountant_TitleCostReportFor_AggregatesMultipleChunksAcrossResourceKinds(t *testing.T) {
	rates := ComputeResourceRates{CpuSecondPriceUsd: 0.00003, GpuSecondPriceUsd: 0.0003}
	accountant := NewCostAccountant(rates)

	accountant.RecordChunkCompletion("job-1", "chunk-a", ResourceKindCpu, 15.0) // $0.00045
	accountant.RecordChunkCompletion("job-1", "chunk-b", ResourceKindCpu, 10.0) // $0.00030
	accountant.RecordChunkCompletion("job-1", "chunk-c", ResourceKindGpu, 2.0)  // $0.00060

	report, ok := accountant.TitleCostReportFor("job-1")
	if !ok {
		t.Fatal("expected a report to exist for job-1")
	}
	if report.ChunkCount != 3 {
		t.Fatalf("expected ChunkCount=3, got %d", report.ChunkCount)
	}

	wantTotalCpuSeconds := 25.0
	if report.TotalCpuSeconds != wantTotalCpuSeconds {
		t.Fatalf("expected TotalCpuSeconds=%v, got %v", wantTotalCpuSeconds, report.TotalCpuSeconds)
	}
	wantTotalGpuSeconds := 2.0
	if report.TotalGpuSeconds != wantTotalGpuSeconds {
		t.Fatalf("expected TotalGpuSeconds=%v, got %v", wantTotalGpuSeconds, report.TotalGpuSeconds)
	}

	wantTotalCost := 0.00045 + 0.00030 + 0.00060 // == 0.00135
	const epsilon = 1e-12
	if diff := report.TotalEstimatedCostUsd - wantTotalCost; diff > epsilon || diff < -epsilon {
		t.Fatalf("expected TotalEstimatedCostUsd≈%v, got %v", wantTotalCost, report.TotalEstimatedCostUsd)
	}
}

// TestCostAccountant_TitleCostReportFor_UnknownJobReturnsFalse verifies
// the documented not-found contract.
func TestCostAccountant_TitleCostReportFor_UnknownJobReturnsFalse(t *testing.T) {
	accountant := NewCostAccountant(ComputeResourceRates{CpuSecondPriceUsd: 0.00003})
	if _, ok := accountant.TitleCostReportFor("no-such-job"); ok {
		t.Fatal("expected ok=false for a job with no recorded chunks")
	}
}

// TestCostAccountant_RecordChunkCompletion_NegativeSecondsClampedToZero
// verifies the defensive clamp against a negative duration input.
func TestCostAccountant_RecordChunkCompletion_NegativeSecondsClampedToZero(t *testing.T) {
	accountant := NewCostAccountant(ComputeResourceRates{CpuSecondPriceUsd: 0.00003})
	record := accountant.RecordChunkCompletion("job-1", "chunk-a", ResourceKindCpu, -5.0)
	if record.ResourceSecondsConsumed != 0 {
		t.Fatalf("expected clamped ResourceSecondsConsumed=0, got %v", record.ResourceSecondsConsumed)
	}
	if record.EstimatedCostUsd != 0 {
		t.Fatalf("expected clamped EstimatedCostUsd=0, got %v", record.EstimatedCostUsd)
	}
}

// TestNewCostAccountant_NegativeRatesClampedToZero verifies negative
// configured rates are clamped rather than silently producing negative
// costs.
func TestNewCostAccountant_NegativeRatesClampedToZero(t *testing.T) {
	accountant := NewCostAccountant(ComputeResourceRates{CpuSecondPriceUsd: -1, GpuSecondPriceUsd: -1})
	cpuRecord := accountant.RecordChunkCompletion("job-1", "chunk-a", ResourceKindCpu, 10)
	gpuRecord := accountant.RecordChunkCompletion("job-1", "chunk-b", ResourceKindGpu, 10)
	if cpuRecord.EstimatedCostUsd != 0 || gpuRecord.EstimatedCostUsd != 0 {
		t.Fatalf("expected zero cost with clamped negative rates, got cpu=%v gpu=%v", cpuRecord.EstimatedCostUsd, gpuRecord.EstimatedCostUsd)
	}
}

// TestResourceKind_StringRendersExpectedLabels verifies the small String
// helper used in reports/logs.
func TestResourceKind_StringRendersExpectedLabels(t *testing.T) {
	cases := map[ResourceKind]string{
		ResourceKindCpu:  "cpu",
		ResourceKindGpu:  "gpu",
		ResourceKind(99): "unknown",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Fatalf("ResourceKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}
