// Package cost implements encode cost accounting: tracking how many
// CPU-seconds (software CodecArgumentBuilder strategies) or GPU-seconds
// (hardware ffmpeg.NvencArgumentBuilder / ffmpeg.QuickSyncArgumentBuilder
// strategies) each completed ChunkEncodingTask consumed, and converting
// that consumption into an estimated dollar cost via configurable
// per-second billing rates, so operators can see (and later optimize) the
// real infrastructure cost of a given title's encode.
//
// # Design
//
// CostAccountant is a small thread-safe aggregator, analogous in spirit
// to worker.jobProgressTracker (internal/worker/dispatcher.go): many
// concurrent goroutines record ChunkEncodingTask completions as the
// WorkerPool finishes them, and CostAccountant folds each completion into
// a running per-job TitleCostReport under a mutex. It intentionally has
// no knowledge of ffmpeg, WorkerPool, or Dispatcher internals — it is
// wired into worker.Dispatcher via a small optional dependency (see
// Dispatcher.WithCostAccountant), following the same "accept the
// interface/small collaborator via a setter, default to a no-op" pattern
// used for JobCheckpointStore and metrics.OrchestratorMetrics.
//
// # What "CPU-seconds"/"GPU-seconds" means here
//
// This package treats a completed ChunkEncodingTask's wall-clock encode
// duration as a proxy for the compute-seconds it consumed, on the
// simplifying (and explicitly documented) assumption that each task
// occupies exactly one CPU core (for software codecs) or one GPU/NVENC or
// Quick Sync encode session (for the hardware strategies in
// internal/ffmpeg/hardwareCodecArgumentBuilder.go) for its entire
// duration — which matches how worker.WorkerPool actually schedules work
// (one goroutine, and in production one subprocess, per in-flight task,
// bounded by WorkerPoolConfiguration.MaxConcurrentTasks). A deployment
// that oversubscribes cores per task, or that shares one GPU across
// several concurrent encode sessions, would need a more detailed resource
// model than this package provides; that refinement is out of scope here
// (see the README's Limitations section for this and other explicitly
// out-of-scope concerns).
package cost

import (
	"sync"
	"time"
)

// ResourceKind distinguishes which billable compute resource a completed
// ChunkEncodingTask consumed, since CPU-second and GPU-second rates are
// typically priced very differently (a cloud GPU instance-hour is
// commonly 5-15x a comparable CPU instance-hour, though hardware encode
// is also typically 10-50x faster, which is exactly the kind of
// trade-off a TitleCostReport lets an operator evaluate empirically).
type ResourceKind int

const (
	// ResourceKindCpu marks consumption billed at
	// ComputeResourceRates.CpuSecondPriceUsd — the software
	// CodecArgumentBuilder strategies (H.264/HEVC/AV1 via libx264/
	// libx265/libaom-av1).
	ResourceKindCpu ResourceKind = iota
	// ResourceKindGpu marks consumption billed at
	// ComputeResourceRates.GpuSecondPriceUsd — the hardware
	// CodecArgumentBuilder strategies (NVENC, Quick Sync).
	ResourceKindGpu
)

// String renders a ResourceKind as a short label for reports/logs.
func (kind ResourceKind) String() string {
	switch kind {
	case ResourceKindCpu:
		return "cpu"
	case ResourceKindGpu:
		return "gpu"
	default:
		return "unknown"
	}
}

// ComputeResourceRates configures the per-second US-dollar billing rate
// for each ResourceKind. Both fields are simple linear multipliers —
// EstimatedCostUsd = resourceSecondsConsumed * rate — deliberately
// avoiding any tiered/discounted pricing model, since real cloud billing
// specifics (reserved capacity, spot pricing, committed-use discounts)
// vary enormously by deployment and are the caller's responsibility to
// fold into these two numbers.
type ComputeResourceRates struct {
	// CpuSecondPriceUsd is the estimated USD cost of one CPU-second of
	// software encode time, e.g. 0.00003 USD/CPU-second corresponds to
	// roughly $0.10/CPU-hour, a representative on-demand cloud compute
	// rate for a single vCPU as of the mid-2020s.
	CpuSecondPriceUsd float64
	// GpuSecondPriceUsd is the estimated USD cost of one GPU-second of
	// hardware encode time, e.g. 0.0003 USD/GPU-second corresponds to
	// roughly $1.08/GPU-hour, a representative shared/fractional GPU
	// encode-instance rate.
	GpuSecondPriceUsd float64
}

// rateFor returns the per-second USD rate for the given ResourceKind.
func (rates ComputeResourceRates) rateFor(kind ResourceKind) float64 {
	switch kind {
	case ResourceKindGpu:
		return rates.GpuSecondPriceUsd
	default:
		return rates.CpuSecondPriceUsd
	}
}

// ChunkCostRecord is the immutable record produced by
// CostAccountant.RecordChunkCompletion for one completed
// ChunkEncodingTask, returned to the caller and folded into the parent
// job's running TitleCostReport.
type ChunkCostRecord struct {
	// JobIdentifier and ChunkIdentifier identify the completed task,
	// mirroring checkpoint.ChunkCompletionRecord's identifier fields so
	// the two packages' records can be cross-referenced by callers that
	// want both checkpoint and cost history for the same chunk.
	JobIdentifier   string
	ChunkIdentifier string
	// ResourceKind is which billable resource this chunk consumed.
	ResourceKind ResourceKind
	// ResourceSecondsConsumed is the CPU- or GPU-seconds attributed to
	// this chunk (see the package doc comment for what that means here).
	ResourceSecondsConsumed float64
	// EstimatedCostUsd is ResourceSecondsConsumed multiplied by the
	// applicable ComputeResourceRates rate at the time of recording.
	EstimatedCostUsd float64
	// RecordedAt is when this record was created.
	RecordedAt time.Time
}

// TitleCostReport aggregates every ChunkCostRecord recorded so far for
// one EncodingJob ("title" in the streaming-industry sense: one source
// asset's full multi-rendition encode). It is the primary
// operator/billing-facing artifact this package produces.
type TitleCostReport struct {
	// JobIdentifier identifies the summarized EncodingJob.
	JobIdentifier string
	// ChunkCount is how many ChunkCostRecord values have been folded into
	// this report.
	ChunkCount int
	// TotalCpuSeconds and TotalGpuSeconds are the summed
	// ResourceSecondsConsumed across all recorded chunks, partitioned by
	// ResourceKind.
	TotalCpuSeconds float64
	TotalGpuSeconds float64
	// TotalEstimatedCostUsd is the summed EstimatedCostUsd across every
	// recorded chunk (CPU and GPU combined) — the headline number for
	// "what did encoding this title cost".
	TotalEstimatedCostUsd float64
}

// CostAccountant tracks per-chunk resource consumption and aggregates it
// into a running TitleCostReport per job. Safe for concurrent use: many
// goroutines (typically worker.Dispatcher's single outcome-processing
// loop per in-flight job, but potentially several jobs dispatched
// concurrently through a shared worker.Dispatcher) may call
// RecordChunkCompletion at once.
type CostAccountant struct {
	rates ComputeResourceRates

	mutex          sync.Mutex
	reportsByJobID map[string]*TitleCostReport
}

// NewCostAccountant constructs a CostAccountant billing at the given
// ComputeResourceRates. Negative rates are clamped to zero (a negative
// per-second price has no sensible interpretation and would otherwise
// silently produce a negative "cost").
func NewCostAccountant(rates ComputeResourceRates) *CostAccountant {
	if rates.CpuSecondPriceUsd < 0 {
		rates.CpuSecondPriceUsd = 0
	}
	if rates.GpuSecondPriceUsd < 0 {
		rates.GpuSecondPriceUsd = 0
	}
	return &CostAccountant{
		rates:          rates,
		reportsByJobID: make(map[string]*TitleCostReport),
	}
}

// RecordChunkCompletion records that one ChunkEncodingTask belonging to
// jobIdentifier (identified by chunkIdentifier, conventionally a
// domain.ChunkEncodingTask.TaskIdentifier) consumed
// resourceSecondsConsumed seconds of the given ResourceKind, converts
// that into an estimated USD cost via the configured
// ComputeResourceRates, folds it into jobIdentifier's running
// TitleCostReport, and returns the individual ChunkCostRecord that was
// created.
//
// Negative resourceSecondsConsumed is clamped to zero (a task cannot have
// consumed negative time; this guards against, e.g., a clock-skew-derived
// negative duration being fed in by a caller without this package having
// to trust every caller's arithmetic).
func (accountant *CostAccountant) RecordChunkCompletion(jobIdentifier string, chunkIdentifier string, kind ResourceKind, resourceSecondsConsumed float64) ChunkCostRecord {
	if resourceSecondsConsumed < 0 {
		resourceSecondsConsumed = 0
	}

	estimatedCostUsd := resourceSecondsConsumed * accountant.rates.rateFor(kind)

	record := ChunkCostRecord{
		JobIdentifier:           jobIdentifier,
		ChunkIdentifier:         chunkIdentifier,
		ResourceKind:            kind,
		ResourceSecondsConsumed: resourceSecondsConsumed,
		EstimatedCostUsd:        estimatedCostUsd,
		RecordedAt:              time.Now(),
	}

	accountant.mutex.Lock()
	defer accountant.mutex.Unlock()

	report, ok := accountant.reportsByJobID[jobIdentifier]
	if !ok {
		report = &TitleCostReport{JobIdentifier: jobIdentifier}
		accountant.reportsByJobID[jobIdentifier] = report
	}
	report.ChunkCount++
	switch kind {
	case ResourceKindGpu:
		report.TotalGpuSeconds += resourceSecondsConsumed
	default:
		report.TotalCpuSeconds += resourceSecondsConsumed
	}
	report.TotalEstimatedCostUsd += estimatedCostUsd

	return record
}

// TitleCostReportFor returns a copy of the current TitleCostReport for
// jobIdentifier and true, or a zero-value TitleCostReport and false if no
// chunks have been recorded for that job yet.
func (accountant *CostAccountant) TitleCostReportFor(jobIdentifier string) (TitleCostReport, bool) {
	accountant.mutex.Lock()
	defer accountant.mutex.Unlock()

	report, ok := accountant.reportsByJobID[jobIdentifier]
	if !ok {
		return TitleCostReport{}, false
	}
	return *report, true
}

// ClearJob discards the accumulated TitleCostReport for jobIdentifier,
// e.g. once its report has been persisted/emitted elsewhere and does not
// need to be retained in memory for the lifetime of the process.
func (accountant *CostAccountant) ClearJob(jobIdentifier string) {
	accountant.mutex.Lock()
	defer accountant.mutex.Unlock()
	delete(accountant.reportsByJobID, jobIdentifier)
}
