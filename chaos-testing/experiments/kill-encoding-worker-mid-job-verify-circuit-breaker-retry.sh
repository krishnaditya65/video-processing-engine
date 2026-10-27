#!/usr/bin/env bash
#
# kill-encoding-worker-mid-job-verify-circuit-breaker-retry.sh
#
# CHAOS EXPERIMENT — distributed-encoding-orchestrator worker failure
#
# HYPOTHESIS
#   Killing an ffmpeg worker mid-job does not stall or crash the encoding
#   fleet as a whole: the worker's ExponentialBackoffRetryPolicy retries
#   the interrupted ChunkEncodingTask (up to MaxAttempts=4, i.e. 1 initial
#   + 3 retries, per DefaultExponentialBackoffRetryPolicy) with growing
#   backoff, and if the failure persists past
#   consecutiveFailureThreshold consecutive attempts, that worker's
#   FfmpegFailureCircuitBreaker trips OPEN, stops hammering the broken
#   path, and the rest of the WorkerPool's goroutines continue draining
#   the shared JobQueue unaffected.
#
# REAL CODE UNDER TEST
#   - services/distributed-encoding-orchestrator/internal/ffmpeg/circuitBreaker.go
#     (FfmpegFailureCircuitBreaker: CLOSED -> OPEN -> HALF_OPEN state machine)
#   - services/distributed-encoding-orchestrator/internal/worker/retryPolicy.go
#     (ExponentialBackoffRetryPolicy: DefaultExponentialBackoffRetryPolicy())
#   - services/distributed-encoding-orchestrator/internal/worker/ (WorkerPool /
#     Dispatcher bounded-goroutine fan-out)
#
# PROCEDURE (as run against a real containerized worker fleet — this
# repository's current `cmd/orchestrator/main.go` is a CLI demo binary
# with no long-running containerized worker process to target with
# `docker kill` today; this script is written against the topology
# described in docs/architecture.md and is AUTHORED/DOCUMENTED, NOT
# EXECUTED, in this environment — see chaos-testing/README.md):
#
#   1. Submit a multi-scene EncodingJob (several ChunkEncodingTasks) to a
#      running distributed-encoding-orchestrator worker fleet.
#   2. While at least one worker is mid-ffmpeg-subprocess on a chunk,
#      forcibly kill that worker's container/process:
#        docker kill <encodingWorkerContainerName>
#      (or, for a bare-process fleet: `kill -9 <ffmpeg worker PID>`).
#   3. Observe the Dispatcher detect the failed attempt and hand the
#      ChunkEncodingTask to `ExponentialBackoffRetryPolicy` for a retry,
#      scheduled after `ComputeBackoffDelayForRetryAttempt(1)` (~500ms +
#      up to 20% jitter per DefaultExponentialBackoffRetryPolicy).
#   4. If the retried attempt also fails (e.g. worker container was
#      removed, not just paused), confirm the per-worker
#      FfmpegFailureCircuitBreaker trips to OPEN after
#      `consecutiveFailureThreshold` consecutive failures and stops
#      dispatching further attempts to that worker slot until
#      `openStateCooldownDuration` elapses, at which point it probes via
#      HALF_OPEN.
#   5. Confirm sibling workers in the WorkerPool are unaffected: their
#      goroutines continue pulling and completing other
#      ChunkEncodingTasks from the shared JobQueue the entire time.
#
# EXPECTED PASS CRITERIA
#   - The overall EncodingJob still reaches a JobCompletionSummary (either
#     fully succeeded once the retried chunk completes on another worker
#     slot, or the specific chunk is marked permanently failed after
#     exhausting MaxAttempts=4 while the REST of the job's chunks succeed
#     — see distributed-encoding-orchestrator/README.md for
#     JobCompletionSummary's partial-failure semantics).
#   - No goroutine leak: `runtime.NumGoroutine()` returns to baseline once
#     the killed worker's retries are exhausted/settled (this exact
#     invariant is unit-tested in the real test suite — see
#     services/distributed-encoding-orchestrator/internal/worker's
#     goroutine-leak test referenced in docs/testing-and-benchmarking.md).
#   - The process as a whole does not crash or exit non-zero because one
#     worker's ffmpeg subprocess died.
#
# HOW TO OBSERVE THE RESULT
#   - Prometheus: `encoding_circuit_breaker_state{service="distributed-encoding-orchestrator"}`
#     transitions 0 (CLOSED) -> 2 (OPEN) -> 1 (HALF_OPEN) -> 0 (CLOSED),
#     visible on the "FfmpegFailureCircuitBreaker state" stat panel in
#     infra/observability/grafana/dashboards/encoding-factory-dashboard.json.
#   - Prometheus: `encoding_worker_retry_attempts_total{attempt_number}`
#     increments once per retry (panel: "ffmpeg subprocess failure rate +
#     retry attempts" on the same dashboard).
#   - Logs: the structured logger (`internal/logging`) emits a log line
#     per retry attempt and per circuit-breaker state transition — grep
#     for `"circuit breaker"` or `"retry attempt"` in worker stdout/stderr.
#   - `encoding_jobs_completed_total{outcome="permanently_failed"}` should
#     increment by at most the number of chunks assigned to the killed
#     worker slot at the moment of the kill, not the whole job.
#
# This script is intentionally conservative/dry-run by default (it prints
# what it would do rather than assuming a specific container topology
# that does not exist in this repository yet). Pass --execute plus a real
# CONTAINER_NAME to actually run the `docker kill` step against a real
# deployment.

set -euo pipefail

CONTAINER_NAME="${ENCODING_WORKER_CONTAINER_NAME:-distributedEncodingOrchestratorWorker0}"
EXECUTE="${1:-}"

echo "=== chaos experiment: kill-encoding-worker-mid-job ==="
echo "target container : ${CONTAINER_NAME}"
echo "hypothesis        : circuit breaker + retry policy absorb a mid-job worker kill without stalling the fleet"
echo

if [[ "${EXECUTE}" != "--execute" ]]; then
  echo "[DRY RUN] Would run:"
  echo "  docker kill \"${CONTAINER_NAME}\""
  echo
  echo "This is a dry run because no long-running containerized worker fleet"
  echo "exists in this repository's current infra/docker-compose.yml (the"
  echo "distributed-encoding-orchestrator's cmd/orchestrator/main.go is a"
  echo "CLI demo binary, not a server process). Re-run with --execute once"
  echo "a worker fleet is deployed as containers named e.g."
  echo "distributedEncodingOrchestratorWorker0, ...Worker1, etc."
  echo
  echo "See this script's header comment for the full hypothesis,"
  echo "procedure, pass criteria, and how to observe the result via"
  echo "Prometheus/Grafana (infra/observability/) and structured logs."
  exit 0
fi

echo "[EXECUTE] Killing worker container ${CONTAINER_NAME}..."
docker kill "${CONTAINER_NAME}"
echo "Killed. Now watch:"
echo "  - Prometheus target 'distributed-encoding-orchestrator' for encoding_circuit_breaker_state"
echo "  - Worker container logs for 'retry attempt' / 'circuit breaker' lines"
echo "  - The submitting client's JobCompletionSummary for partial-failure handling"
