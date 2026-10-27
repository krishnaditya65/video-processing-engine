// Command orchestrator is a small runnable demonstration of the
// distributed-encoding-orchestrator service: it constructs a synthetic
// EncodingJob (a handful of scenes, an H.264 ladder), dispatches it
// through a Dispatcher/WorkerPool backed by a FakeSubprocessRunner (so
// no real ffmpeg binary is required to see the whole pipeline run
// end-to-end), prints the resulting JobCompletionSummary, and separately
// demonstrates the convex-hull bitrate-ladder optimizer and the A/B
// forensic watermark scheduler on illustrative sample data.
//
// This binary is a demo/reference entry point, not a production service
// listener (no HTTP/gRPC server is started); see the README for how a
// production deployment would wire a real ffmpeg.NewRealOsExecSubprocessRunner
// and an HTTP/gRPC job-submission API around the same Dispatcher/WorkerPool
// core.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"distributed-encoding-orchestrator/internal/checkpoint"
	"distributed-encoding-orchestrator/internal/cost"
	"distributed-encoding-orchestrator/internal/domain"
	"distributed-encoding-orchestrator/internal/ffmpeg"
	"distributed-encoding-orchestrator/internal/ladder"
	"distributed-encoding-orchestrator/internal/logging"
	"distributed-encoding-orchestrator/internal/metrics"
	"distributed-encoding-orchestrator/internal/watermark"
	"distributed-encoding-orchestrator/internal/worker"

	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	logger := logging.DefaultStdoutLogger()

	demonstrateBitrateLadderOptimization(logger)
	demonstrateGlobalPerTitleBitrateLadder(logger)
	demonstrateForensicWatermarkScheduling(logger)
	demonstrateHardwareAndHdrArgumentBuilders(logger)
	demonstrateEndToEndJobDispatch(logger)
}

func demonstrateBitrateLadderOptimization(logger logging.StructuredEventLogger) {
	samples := []ladder.RateDistortionSamplePoint{
		{ResolutionLabel: "1920x1080", BitrateKilobitsPerSecond: 8000, VmafScore: 98.0},
		{ResolutionLabel: "1920x1080", BitrateKilobitsPerSecond: 5000, VmafScore: 95.0},
		{ResolutionLabel: "1920x1080", BitrateKilobitsPerSecond: 3000, VmafScore: 88.0},
		{ResolutionLabel: "1280x720", BitrateKilobitsPerSecond: 3000, VmafScore: 90.0},
		{ResolutionLabel: "1280x720", BitrateKilobitsPerSecond: 2000, VmafScore: 84.0},
		{ResolutionLabel: "1280x720", BitrateKilobitsPerSecond: 1200, VmafScore: 70.0},
		{ResolutionLabel: "854x480", BitrateKilobitsPerSecond: 1200, VmafScore: 74.0},
		{ResolutionLabel: "854x480", BitrateKilobitsPerSecond: 600, VmafScore: 55.0},
	}
	rungs := ladder.ComputeParetoOptimalBitrateLadderForScene(samples)
	fmt.Println("=== Convex-Hull Bitrate Ladder ===")
	for _, rung := range rungs {
		fmt.Printf("  %s  (marginal gain: %.4f VMAF/kbps)\n", rung.SamplePoint.String(), rung.MarginalVmafGainPerKilobitOverPreviousRung)
	}
	fmt.Println()
}

func demonstrateGlobalPerTitleBitrateLadder(logger logging.StructuredEventLogger) {
	scenes := []ladder.SceneHullWithDuration{
		{
			SceneIndex:      0,
			DurationSeconds: 10,
			HullRungs: []ladder.ParetoOptimalBitrateLadderRung{
				{SamplePoint: ladder.RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 80}},
				{SamplePoint: ladder.RateDistortionSamplePoint{BitrateKilobitsPerSecond: 3000, VmafScore: 95}},
			},
		},
		{
			SceneIndex:      1,
			DurationSeconds: 30,
			HullRungs: []ladder.ParetoOptimalBitrateLadderRung{
				{SamplePoint: ladder.RateDistortionSamplePoint{BitrateKilobitsPerSecond: 1000, VmafScore: 90}},
				{SamplePoint: ladder.RateDistortionSamplePoint{BitrateKilobitsPerSecond: 2000, VmafScore: 96}},
			},
		},
	}
	rungs, err := ladder.ComputeGlobalPerTitleBitrateLadder(scenes, []float64{84, 90, 93, 95})
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to compute global per-title ladder:", err)
		return
	}
	fmt.Println("=== Global Per-Title Bitrate Ladder (duration-weighted, two-scene demo title) ===")
	for _, rung := range rungs {
		fmt.Printf("  VMAF floor %.0f -> effective bitrate %.2f kbps (quality-limited: %v)\n",
			rung.TargetVmafFloor, rung.EffectiveBitrateKilobitsPerSecond, rung.AnyScenesQualityLimited)
	}
	fmt.Println()
}

func demonstrateHardwareAndHdrArgumentBuilders(logger logging.StructuredEventLogger) {
	spec := ffmpeg.ChunkEncodeSpecification{
		InputMediaFilePath:             "/in/scene0/chunk0.ts",
		OutputMediaFilePath:            "/out/scene0/chunk0_1080p_hdr.ts",
		TargetWidthPixels:              1920,
		TargetHeightPixels:             1080,
		TargetBitrateKilobitsPerSecond: 8000,
	}
	hdrProfile := ffmpeg.HdrMetadataProfile{
		ColorPrimaries:             "bt2020",
		TransferCharacteristic:     ffmpeg.HdrTransferFunctionSmpte2084Pq,
		ColorSpace:                 "bt2020nc",
		MasterDisplayMetadataValue: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)",
		MaxContentLightLevel:       1000,
		MaxFrameAverageLightLevel:  400,
	}

	nvencHdr := ffmpeg.NewHdrMetadataPassthroughCodecArgumentBuilder(ffmpeg.NewNvencArgumentBuilder(), hdrProfile)
	fmt.Println("=== NVENC + HDR10 static metadata passthrough (Strategy + Decorator composed) ===")
	fmt.Println(" ", nvencHdr.BuildFfmpegCommandLineArguments(spec))

	qsvBuilder := ffmpeg.NewQuickSyncArgumentBuilder()
	fmt.Println("=== Quick Sync (SDR) ===")
	fmt.Println(" ", qsvBuilder.BuildFfmpegCommandLineArguments(spec))
	fmt.Println()
}

func demonstrateForensicWatermarkScheduling(logger logging.StructuredEventLogger) {
	sequence := watermark.GenerateDeterministicVariantSequenceForSession("user-42", "session-abc", 16)
	fmt.Println("=== Forensic Watermark A/B Sequence ===")
	fmt.Print("  ")
	for _, v := range sequence {
		fmt.Print(v.String())
	}
	fmt.Println()
	matches := watermark.VerifyRecoveredVariantSequenceMatchesUserSession("user-42", "session-abc", sequence[:8])
	fmt.Printf("  recovered-prefix verify against correct session: %v\n\n", matches)
}

func demonstrateEndToEndJobDispatch(logger logging.StructuredEventLogger) {
	fakeRunner := &ffmpeg.FakeSubprocessRunner{
		ScriptedStandardErrorLines: []string{"frame=100", "out_time_us=2000000", "speed=3.2x", "progress=end"},
	}
	processor := worker.NewFfmpegBackedChunkEncodingTaskProcessor(fakeRunner, "ffmpeg", logger, nil)

	pool := worker.NewWorkerPool(processor, worker.WorkerPoolConfiguration{
		MaxConcurrentTasks: 16,
		Logger:             logger,
	})

	// Wire the optional cross-cutting collaborators added alongside the
	// core Dispatcher/WorkerPool: Prometheus metrics (served on
	// :9090/metrics for a real Prometheus scraper to poll), checkpoint-
	// resume support, and encode cost accounting. Every one of these is
	// attached via a fluent With* setter and is safe to omit entirely (see
	// each package's doc comment) — this demo simply shows all three
	// wired in at once.
	metricsRegistry := prometheus.NewRegistry()
	orchestratorMetrics := metrics.NewOrchestratorMetrics(metricsRegistry)
	pool.SetMetricsSink(orchestratorMetrics)

	metricsServer := &http.Server{Addr: ":9090", Handler: metrics.NewMetricsHttpServeMux(metricsRegistry)}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Warn("metrics HTTP server exited", "error", err.Error())
		}
	}()
	defer metricsServer.Close()

	checkpointStore := checkpoint.NewInMemoryJobCheckpointStore()
	costAccountant := cost.NewCostAccountant(cost.ComputeResourceRates{
		CpuSecondPriceUsd: 0.00003, // ≈$0.10/CPU-hour
		GpuSecondPriceUsd: 0.0003,  // ≈$1.08/GPU-hour
	})

	dispatcher := worker.NewDispatcher(pool, logger, 256, func(taskID string) (string, string) {
		return "/tmp/in/" + taskID + ".ts", "/tmp/out/" + taskID + ".ts"
	}).WithMetricsSink(orchestratorMetrics).
		WithJobCheckpointStore(checkpointStore).
		WithCostAccountant(costAccountant)

	job, err := domain.NewEncodingJobBuilder("demo-job-1", "s3://bucket/demo-movie.mov").
		WithTargetCodec(ffmpeg.CodecH264).
		WithSceneBoundaries([]domain.SceneBoundary{
			{SceneIndex: 0, StartFrameIndex: 0, EndFrameIndex: 240, FrameRateFramesPerSecond: 24},
			{SceneIndex: 1, StartFrameIndex: 240, EndFrameIndex: 480, FrameRateFramesPerSecond: 24},
		}).
		WithBitrateLadderRungs([]domain.BitrateLadderRung{
			{RenditionLabel: "1080p-5000k", WidthPixels: 1920, HeightPixels: 1080, BitrateKilobitsPerSecond: 5000},
			{RenditionLabel: "720p-2500k", WidthPixels: 1280, HeightPixels: 720, BitrateKilobitsPerSecond: 2500},
		}).
		WithMaxChunkDurationSeconds(2.0).
		Build()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to build demo job:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("=== End-to-End Dispatch Demo (metrics on :9090/metrics, checkpoint-resume + cost accounting wired in) ===")
	summary := dispatcher.DispatchEncodingJob(ctx, job)
	fmt.Printf("  job=%s total=%d succeeded=%d failed=%d duration=%s\n",
		summary.JobIdentifier, summary.TotalTaskCount, summary.SucceededTaskCount, summary.FailedTaskCount, summary.WallClockDuration)

	if report, ok := costAccountant.TitleCostReportFor(job.JobIdentifier); ok {
		fmt.Printf("  cost report: chunks=%d cpuSeconds=%.4f estimatedCostUsd=$%.6f\n",
			report.ChunkCount, report.TotalCpuSeconds, report.TotalEstimatedCostUsd)
	}
	if completed, err := checkpointStore.ListCompletedChunks(ctx, job.JobIdentifier); err == nil {
		fmt.Printf("  checkpoint store now holds %d completed-chunk records for this job\n", len(completed))
	}

	fmt.Println("  keeping the /metrics HTTP server up for 3s so `curl http://localhost:9090/metrics` can be tried...")
	time.Sleep(3 * time.Second)
}
