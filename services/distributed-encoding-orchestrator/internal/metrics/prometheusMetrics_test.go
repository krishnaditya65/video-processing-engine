package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// scrapeMetricsText starts an httptest server serving registry's metrics
// and returns the full scraped exposition-format body text.
func scrapeMetricsText(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	server := httptest.NewServer(NewMetricsHttpServeMux(registry))
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body failed: %v", err)
	}
	return string(body)
}

// TestOrchestratorMetrics_SetWorkerPoolUtilization_ExposedViaHttpEndpoint
// verifies the worker-pool utilization gauges are registered, updated,
// and correctly rendered by the /metrics HTTP endpoint.
func TestOrchestratorMetrics_SetWorkerPoolUtilization_ExposedViaHttpEndpoint(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewOrchestratorMetrics(registry)

	m.SetWorkerPoolUtilization(37, 500)

	body := scrapeMetricsText(t, registry)
	if !strings.Contains(body, `distributed_encoding_orchestrator_worker_pool_active_task_count 37`) {
		t.Fatalf("expected active_task_count==37 in scrape output:\n%s", body)
	}
	if !strings.Contains(body, `distributed_encoding_orchestrator_worker_pool_max_concurrent_task_count 500`) {
		t.Fatalf("expected max_concurrent_task_count==500 in scrape output:\n%s", body)
	}
}

// TestOrchestratorMetrics_RecordChunkOutcome_IncrementsLabeledCounters
// verifies succeeded/failed chunk outcomes are counted under distinct
// label values.
func TestOrchestratorMetrics_RecordChunkOutcome_IncrementsLabeledCounters(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewOrchestratorMetrics(registry)

	m.RecordChunkOutcome(true)
	m.RecordChunkOutcome(true)
	m.RecordChunkOutcome(false)

	body := scrapeMetricsText(t, registry)
	if !strings.Contains(body, `distributed_encoding_orchestrator_worker_pool_chunk_encoding_tasks_completed_total{result="succeeded"} 2`) {
		t.Fatalf("expected succeeded=2 in scrape output:\n%s", body)
	}
	if !strings.Contains(body, `distributed_encoding_orchestrator_worker_pool_chunk_encoding_tasks_completed_total{result="failed"} 1`) {
		t.Fatalf("expected failed=1 in scrape output:\n%s", body)
	}
}

// TestOrchestratorMetrics_RecordJobCompletion_IncrementsLabeledCounter
// verifies job completion counting by final_status label.
func TestOrchestratorMetrics_RecordJobCompletion_IncrementsLabeledCounter(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewOrchestratorMetrics(registry)

	m.RecordJobCompletion("completed")
	m.RecordJobCompletion("completed")
	m.RecordJobCompletion("failed")

	body := scrapeMetricsText(t, registry)
	if !strings.Contains(body, `distributed_encoding_orchestrator_dispatcher_encoding_jobs_completed_total{final_status="completed"} 2`) {
		t.Fatalf("expected completed=2 in scrape output:\n%s", body)
	}
	if !strings.Contains(body, `distributed_encoding_orchestrator_dispatcher_encoding_jobs_completed_total{final_status="failed"} 1`) {
		t.Fatalf("expected failed=1 in scrape output:\n%s", body)
	}
}

// TestOrchestratorMetrics_RecordCost_SumsResourceSecondsAndEstimatedCost
// hand-verifies the cost-accounting sums arithmetic exposed via metrics:
// two CPU chunks (15s, 10s) and one GPU chunk (2s), with a specific
// per-chunk cost, must sum correctly per resource_kind and in the
// combined total.
func TestOrchestratorMetrics_RecordCost_SumsResourceSecondsAndEstimatedCost(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewOrchestratorMetrics(registry)

	m.RecordCost("cpu", 15.0, 0.00045)
	m.RecordCost("cpu", 10.0, 0.00030)
	m.RecordCost("gpu", 2.0, 0.00060)

	body := scrapeMetricsText(t, registry)
	if !strings.Contains(body, `distributed_encoding_orchestrator_cost_resource_seconds_consumed_total{resource_kind="cpu"} 25`) {
		t.Fatalf("expected cpu resource-seconds==25 in scrape output:\n%s", body)
	}
	if !strings.Contains(body, `distributed_encoding_orchestrator_cost_resource_seconds_consumed_total{resource_kind="gpu"} 2`) {
		t.Fatalf("expected gpu resource-seconds==2 in scrape output:\n%s", body)
	}
	// 0.00045 + 0.00030 + 0.00060 == 0.00135
	if !strings.Contains(body, `distributed_encoding_orchestrator_cost_estimated_cost_usd_total 0.00135`) {
		t.Fatalf("expected combined estimated_cost_usd_total==0.00135 in scrape output:\n%s", body)
	}
}

// TestOrchestratorMetrics_NilReceiverMethodsAreSafeNoOps verifies every
// mutating method is a safe no-op on a nil *OrchestratorMetrics, so
// callers (worker.WorkerPool/Dispatcher) can unconditionally invoke these
// hooks even when no metrics sink was configured, matching the "metrics
// disabled by default" contract documented on the type.
func TestOrchestratorMetrics_NilReceiverMethodsAreSafeNoOps(t *testing.T) {
	var m *OrchestratorMetrics
	m.SetWorkerPoolUtilization(1, 2)
	m.RecordChunkOutcome(true)
	m.RecordJobCompletion("completed")
	m.RecordCost("cpu", 1, 1)
	// Reaching this line without a panic is the assertion.
}
