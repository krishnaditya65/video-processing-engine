package worker

import (
	"context"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/cost"
	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
	"distributed-encoding-orchestrator/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

// TestDispatcher_WithCostAccountant_RecordsCpuSecondsForEverySucceededTask
// verifies the Dispatcher <-> cost.CostAccountant wiring end to end: every
// successfully completed ChunkEncodingTask must be folded into the
// parent job's cost.TitleCostReport, and — since buildFixtureJob's tasks
// all use the software CodecH264 strategy — billed at the CPU rate.
func TestDispatcher_WithCostAccountant_RecordsCpuSecondsForEverySucceededTask(t *testing.T) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		ScriptedStandardErrorLines: []string{"frame=10", "progress=end"},
	}
	processor := NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", nil, nil)
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})

	accountant := cost.NewCostAccountant(cost.ComputeResourceRates{CpuSecondPriceUsd: 0.00003, GpuSecondPriceUsd: 0.0003})
	dispatcher := NewDispatcher(pool, nil, 64, nil).WithCostAccountant(accountant)

	job := buildFixtureJob(t) // 6 tasks, all software CodecH264
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary := dispatcher.DispatchEncodingJob(ctx, job)
	if summary.SucceededTaskCount != 6 {
		t.Fatalf("expected 6 succeeded tasks, got %d", summary.SucceededTaskCount)
	}

	report, ok := accountant.TitleCostReportFor(job.JobIdentifier)
	if !ok {
		t.Fatal("expected a TitleCostReport to exist for the dispatched job")
	}
	if report.ChunkCount != 6 {
		t.Fatalf("expected ChunkCount=6 (one cost record per succeeded task), got %d", report.ChunkCount)
	}
	if report.TotalGpuSeconds != 0 {
		t.Fatalf("expected TotalGpuSeconds=0 for an all-software-codec job, got %v", report.TotalGpuSeconds)
	}
	if report.TotalCpuSeconds <= 0 {
		t.Fatalf("expected TotalCpuSeconds>0 (each task's wall-clock duration was recorded), got %v", report.TotalCpuSeconds)
	}
	wantCost := report.TotalCpuSeconds * 0.00003
	const epsilon = 1e-12
	if diff := report.TotalEstimatedCostUsd - wantCost; diff > epsilon || diff < -epsilon {
		t.Fatalf("expected TotalEstimatedCostUsd == TotalCpuSeconds*rate (%v), got %v", wantCost, report.TotalEstimatedCostUsd)
	}
}

// TestDispatcher_WithCostAccountant_BillsHardwareCodecTasksAtGpuRate
// verifies that a job encoded with a hardware-accelerated codec strategy
// (NVENC) is billed at the GPU rate rather than the CPU rate, proving the
// Dispatcher correctly consults
// ffmpeg.SupportedVideoCodec.IsHardwareAccelerated() when recording cost.
func TestDispatcher_WithCostAccountant_BillsHardwareCodecTasksAtGpuRate(t *testing.T) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		ScriptedStandardErrorLines: []string{"frame=10", "progress=end"},
	}
	processor := NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", nil, nil)
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})

	accountant := cost.NewCostAccountant(cost.ComputeResourceRates{CpuSecondPriceUsd: 0.00003, GpuSecondPriceUsd: 0.0003})
	dispatcher := NewDispatcher(pool, nil, 64, nil).WithCostAccountant(accountant)

	job, err := domain.NewEncodingJobBuilder("gpu-job", "s3://bucket/gpu.mov").
		WithTargetCodec(ffmpeg.CodecH264Nvenc).
		WithSceneBoundaries([]domain.SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 24, FrameRateFramesPerSecond: 24},
		}).
		WithBitrateLadderRungs([]domain.BitrateLadderRung{
			{RenditionLabel: "1080p", WidthPixels: 1920, HeightPixels: 1080, BitrateKilobitsPerSecond: 5000},
		}).
		WithMaxChunkDurationSeconds(2.0).
		Build()
	if err != nil {
		t.Fatalf("failed to build gpu fixture job: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	summary := dispatcher.DispatchEncodingJob(ctx, job)
	if summary.SucceededTaskCount != 1 {
		t.Fatalf("expected 1 succeeded task, got %d", summary.SucceededTaskCount)
	}

	report, ok := accountant.TitleCostReportFor(job.JobIdentifier)
	if !ok {
		t.Fatal("expected a TitleCostReport to exist")
	}
	if report.TotalCpuSeconds != 0 {
		t.Fatalf("expected TotalCpuSeconds=0 for an NVENC job, got %v", report.TotalCpuSeconds)
	}
	if report.TotalGpuSeconds <= 0 {
		t.Fatalf("expected TotalGpuSeconds>0 for an NVENC job, got %v", report.TotalGpuSeconds)
	}
}

// TestDispatcher_WithMetricsSink_RecordsJobCompletionCounter verifies the
// Dispatcher <-> metrics.OrchestratorMetrics wiring: a fully-succeeded
// job dispatch must increment the "completed" job-completion counter.
func TestDispatcher_WithMetricsSink_RecordsJobCompletionCounter(t *testing.T) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		ScriptedStandardErrorLines: []string{"frame=10", "progress=end"},
	}
	processor := NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", nil, nil)
	pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: 8})

	registry := prometheus.NewRegistry()
	metricsSink := metrics.NewOrchestratorMetrics(registry)
	pool.SetMetricsSink(metricsSink)
	dispatcher := NewDispatcher(pool, nil, 64, nil).WithMetricsSink(metricsSink)

	job := buildFixtureJob(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary := dispatcher.DispatchEncodingJob(ctx, job)
	if summary.FinalStatus() != domain.EncodingJobStatusCompleted {
		t.Fatalf("expected FinalStatus()==Completed, got %v", summary.FinalStatus())
	}

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather returned error: %v", err)
	}

	var sawCompletedJob, sawSucceededChunks bool
	for _, family := range metricFamilies {
		switch family.GetName() {
		case "distributed_encoding_orchestrator_dispatcher_encoding_jobs_completed_total":
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					if label.GetName() == "final_status" && label.GetValue() == "completed" && metric.GetCounter().GetValue() == 1 {
						sawCompletedJob = true
					}
				}
			}
		case "distributed_encoding_orchestrator_worker_pool_chunk_encoding_tasks_completed_total":
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					if label.GetName() == "result" && label.GetValue() == "succeeded" && metric.GetCounter().GetValue() == 6 {
						sawSucceededChunks = true
					}
				}
			}
		}
	}
	if !sawCompletedJob {
		t.Fatal("expected a completed-status job-completion counter with value 1")
	}
	if !sawSucceededChunks {
		t.Fatal("expected a succeeded chunk-completion counter with value 6")
	}
}
