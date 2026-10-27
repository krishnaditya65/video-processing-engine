// Package metrics wires the distributed encoding orchestrator's
// operational signals into Prometheus via
// github.com/prometheus/client_golang: worker-pool utilization gauges,
// job/chunk completion counters, and cost-accounting sums, all exposed
// on a small "/metrics" HTTP endpoint that a real deployment's
// Prometheus server would scrape on an interval.
//
// # Design
//
// OrchestratorMetrics is a thin Facade (a single struct bundling every
// metric this service exposes, constructed once at startup and threaded
// into the collaborators that produce the underlying events) over a
// prometheus.Registry. worker.WorkerPool and worker.Dispatcher accept an
// optional *OrchestratorMetrics (nil by default, meaning "metrics
// disabled") via a small setter method, following the same
// optional-collaborator pattern already used for
// checkpoint.JobCheckpointStore and cost.CostAccountant — none of these
// cross-cutting concerns are required to construct or run a WorkerPool/
// Dispatcher, but each can be attached without changing any existing
// constructor signature or breaking any existing caller.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// OrchestratorMetrics bundles every Prometheus metric this service
// exposes. Construct with NewOrchestratorMetrics, which registers every
// metric against the supplied prometheus.Registerer; expose them for
// scraping via NewMetricsHttpHandler.
//
// Concurrency/safety: every field is a prometheus client-provided metric
// type, all of which are documented by that library as safe for
// concurrent use from multiple goroutines, so OrchestratorMetrics as a
// whole is safe for concurrent use without any additional locking in
// this package.
type OrchestratorMetrics struct {
	// WorkerPoolActiveTaskCount is a gauge of how many ChunkEncodingTask
	// executions are in flight right now across the whole WorkerPool.
	WorkerPoolActiveTaskCount prometheus.Gauge
	// WorkerPoolMaxConcurrentTaskCount is a gauge of the WorkerPool's
	// configured concurrency bound (WorkerPoolConfiguration.MaxConcurrentTasks),
	// exposed alongside WorkerPoolActiveTaskCount so a dashboard can graph
	// utilization as ActiveTaskCount/MaxConcurrentTaskCount without
	// needing to know the static configuration out of band.
	WorkerPoolMaxConcurrentTaskCount prometheus.Gauge
	// ChunkEncodingTasksCompletedTotal counts every settled
	// ChunkEncodingTask (after retries are exhausted or a success is
	// recorded), partitioned by the "result" label ("succeeded" or
	// "failed").
	ChunkEncodingTasksCompletedTotal *prometheus.CounterVec
	// EncodingJobsCompletedTotal counts every EncodingJob dispatch that
	// has fully settled, partitioned by the "final_status" label
	// ("completed" or "failed"), matching
	// worker.JobCompletionSummary.FinalStatus().
	EncodingJobsCompletedTotal *prometheus.CounterVec
	// ResourceSecondsConsumedTotal sums CPU-/GPU-seconds consumed by
	// completed chunks, partitioned by the "resource_kind" label ("cpu" or
	// "gpu"), mirroring cost.ResourceKind.
	ResourceSecondsConsumedTotal *prometheus.CounterVec
	// EstimatedCostUsdTotal sums the estimated USD cost of every recorded
	// chunk completion (see internal/cost.CostAccountant), across all
	// resource kinds and jobs — the single headline "how much has this
	// orchestrator spent so far" counter.
	EstimatedCostUsdTotal prometheus.Counter
}

// NewOrchestratorMetrics constructs an OrchestratorMetrics and registers
// every contained metric against registerer. Passing a fresh
// prometheus.NewRegistry() (rather than prometheus.DefaultRegisterer) is
// recommended for tests, so repeated test runs within the same process
// don't collide on already-registered metric names; production code
// typically passes prometheus.DefaultRegisterer so metrics are exposed on
// the process-wide default handler.
func NewOrchestratorMetrics(registerer prometheus.Registerer) *OrchestratorMetrics {
	metrics := &OrchestratorMetrics{
		WorkerPoolActiveTaskCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "worker_pool",
			Name:      "active_task_count",
			Help:      "Number of ChunkEncodingTask executions currently in flight across the WorkerPool.",
		}),
		WorkerPoolMaxConcurrentTaskCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "worker_pool",
			Name:      "max_concurrent_task_count",
			Help:      "The WorkerPool's configured MaxConcurrentTasks concurrency bound.",
		}),
		ChunkEncodingTasksCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "worker_pool",
			Name:      "chunk_encoding_tasks_completed_total",
			Help:      "Total number of ChunkEncodingTask units that have settled, partitioned by result (succeeded|failed).",
		}, []string{"result"}),
		EncodingJobsCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "dispatcher",
			Name:      "encoding_jobs_completed_total",
			Help:      "Total number of EncodingJob dispatches that have fully settled, partitioned by final_status (completed|failed).",
		}, []string{"final_status"}),
		ResourceSecondsConsumedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "cost",
			Name:      "resource_seconds_consumed_total",
			Help:      "Total CPU-/GPU-seconds consumed by completed chunk encodes, partitioned by resource_kind (cpu|gpu).",
		}, []string{"resource_kind"}),
		EstimatedCostUsdTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "distributed_encoding_orchestrator",
			Subsystem: "cost",
			Name:      "estimated_cost_usd_total",
			Help:      "Total estimated USD cost of every recorded chunk completion, summed across all jobs and resource kinds.",
		}),
	}

	registerer.MustRegister(
		metrics.WorkerPoolActiveTaskCount,
		metrics.WorkerPoolMaxConcurrentTaskCount,
		metrics.ChunkEncodingTasksCompletedTotal,
		metrics.EncodingJobsCompletedTotal,
		metrics.ResourceSecondsConsumedTotal,
		metrics.EstimatedCostUsdTotal,
	)

	return metrics
}

// SetWorkerPoolUtilization updates the two worker-pool utilization
// gauges. Callers (typically worker.WorkerPool, once per task
// dispatch/completion) pass the current in-flight task count and the
// pool's fixed configured concurrency bound.
func (metrics *OrchestratorMetrics) SetWorkerPoolUtilization(activeTaskCount int, maxConcurrentTaskCount int) {
	if metrics == nil {
		return
	}
	metrics.WorkerPoolActiveTaskCount.Set(float64(activeTaskCount))
	metrics.WorkerPoolMaxConcurrentTaskCount.Set(float64(maxConcurrentTaskCount))
}

// RecordChunkOutcome increments ChunkEncodingTasksCompletedTotal for a
// single settled ChunkEncodingTask.
func (metrics *OrchestratorMetrics) RecordChunkOutcome(succeeded bool) {
	if metrics == nil {
		return
	}
	if succeeded {
		metrics.ChunkEncodingTasksCompletedTotal.WithLabelValues("succeeded").Inc()
	} else {
		metrics.ChunkEncodingTasksCompletedTotal.WithLabelValues("failed").Inc()
	}
}

// RecordJobCompletion increments EncodingJobsCompletedTotal for a single
// settled EncodingJob dispatch, labeled by its final status string (e.g.
// domain.EncodingJobStatus.String(), which yields "completed" or
// "failed" for the two terminal states worker.JobCompletionSummary.FinalStatus()
// can return).
func (metrics *OrchestratorMetrics) RecordJobCompletion(finalStatus string) {
	if metrics == nil {
		return
	}
	metrics.EncodingJobsCompletedTotal.WithLabelValues(finalStatus).Inc()
}

// RecordCost increments the resource-seconds and estimated-cost counters
// for one completed chunk's cost.ChunkCostRecord. resourceKind should be
// cost.ResourceKind.String() ("cpu" or "gpu").
func (metrics *OrchestratorMetrics) RecordCost(resourceKind string, resourceSecondsConsumed float64, estimatedCostUsd float64) {
	if metrics == nil {
		return
	}
	metrics.ResourceSecondsConsumedTotal.WithLabelValues(resourceKind).Add(resourceSecondsConsumed)
	metrics.EstimatedCostUsdTotal.Add(estimatedCostUsd)
}

// NewMetricsHttpHandler returns an http.Handler serving the Prometheus
// text exposition format for every metric registered against registry,
// suitable for mounting at "/metrics" via http.Handle /
// http.ServeMux.Handle.
func NewMetricsHttpHandler(registry *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// NewMetricsHttpServeMux is a small convenience constructor bundling
// NewMetricsHttpHandler onto a fresh *http.ServeMux at the conventional
// "/metrics" path, ready to be passed directly to http.ListenAndServe (or
// mounted as a sub-handler of a larger service mux).
func NewMetricsHttpServeMux(registry *prometheus.Registry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", NewMetricsHttpHandler(registry))
	return mux
}
