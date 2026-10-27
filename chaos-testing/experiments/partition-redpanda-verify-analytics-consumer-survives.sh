#!/usr/bin/env bash
#
# partition-redpanda-verify-analytics-consumer-survives.sh
#
# CHAOS EXPERIMENT — analytics-telemetry-platform broker-loss resilience
#
# HYPOTHESIS
#   Stopping or network-partitioning `redpandaTelemetryStreamBroker` does
#   not crash or hang the analytics-telemetry-platform's beacon-ingestion
#   process: `PlaybackBeaconKafkaConsumer`
#   (services/analytics-telemetry-platform/src/ingestion/kafkaConsumer.ts)
#   is built on `kafkajs`'s real `Consumer` API (`.connect()`,
#   `.subscribe()`, `.run({ eachBatch })`), and `kafkajs`'s default
#   retry/reconnection behavior means transient broker unavailability
#   surfaces as consumer-group rebalance/retry log activity, not a process
#   crash — matching this platform's documented SLO philosophy in
#   docs/slo-sli-sla.md §5 ("Decoupled via Redpanda so a slow consumer
#   never blocks playback; some beacon loss under extreme load is
#   acceptable, sustained data-lake unavailability is not").
#
# REAL CODE UNDER TEST
#   - services/analytics-telemetry-platform/src/ingestion/kafkaConsumer.ts
#     (PlaybackBeaconKafkaConsumer, built on the real `kafkajs` client)
#   - services/analytics-telemetry-platform/src/ingestion/pipeline/beaconBatchInsertStage.ts
#     (BeaconMicroBatchAccumulator — in-flight batch state when the broker
#     connection drops mid-batch)
#   - services/analytics-telemetry-platform/src/ingestion/pipeline/beaconIngestionPipeline.ts
#     (runPlaybackBeaconIngestionPipeline — the pure, already-unit-tested
#     processing logic each fetched batch is run through)
#
# PROCEDURE
#   1. Start the full stack (`cd infra && docker compose up -d`) and start
#      analytics-telemetry-platform's consumer process
#      (`npm run start` / `npm run dev`) so it connects to
#      `redpandaTelemetryStreamBroker` and begins consuming.
#   2. Confirm baseline: produce a handful of playback-beacon messages to
#      the consumer's topic and confirm they reach ClickHouse via the
#      micro-batch insert stage.
#   3a. STOP variant (simulates broker crash/restart):
#        docker compose -f infra/docker-compose.yml stop redpandaTelemetryStreamBroker
#   3b. PARTITION variant (simulates a network partition rather than a
#       crash — requires `tc`/`netem` inside the container's network
#       namespace, or an external network-chaos tool since bare
#       `docker network disconnect` is the closest equivalent achievable
#       with only Docker CLI primitives):
#        docker network disconnect infra_default redpandaTelemetryStreamBroker
#      (adjust the network name to whatever `docker network ls` shows for
#      this compose project — compose typically names it
#      `<project-directory-basename>_default`, e.g. `infra_default`).
#   4. Observe the consumer process during the outage: it should log
#      `kafkajs`'s own connection-retry/rebalance activity, not exit.
#   5. Restore the broker:
#        docker compose -f infra/docker-compose.yml start redpandaTelemetryStreamBroker
#        # or: docker network connect infra_default redpandaTelemetryStreamBroker
#   6. Confirm the consumer resumes consuming without requiring a manual
#      process restart, and that no previously-committed offsets are
#      reprocessed in a way that violates the pipeline's idempotency
#      assumptions (see beaconIngestionPipeline.ts's validate/normalize
#      stage for how malformed/duplicate beacons are handled).
#
# EXPECTED PASS CRITERIA
#   - The Node.js consumer process stays alive throughout the outage —
#     `kafkajs` connection errors must not be unhandled promise
#     rejections that crash the event loop.
#   - No beacons that were already committed (offset-acknowledged) before
#     the outage are lost from ClickHouse.
#   - Beacons produced DURING the outage are either buffered by the
#     broker itself (if only the consumer side is affected) or are
#     acceptable to lose per the documented "graceful beacon-loss
#     tolerance" SLA in docs/slo-sli-sla.md §5 — this experiment's pass
#     criteria is "the process survives and resumes", not "zero message
#     loss", which is explicitly not the target for this plane.
#   - After broker restoration, the consumer resumes without manual
#     intervention within a reasonable rebalance window (`kafkajs`
#     defaults its retry backoff; no custom override is currently
#     configured in kafkaConsumer.ts, so expect its library defaults).
#
# HOW TO OBSERVE THE RESULT
#   - Consumer stdout/stderr: `kafkajs` logs connection errors and
#     rebalance events via its own `logLevel` configuration (imported in
#     kafkaConsumer.ts) — grep for `ECONNREFUSED`, `Connection error`, or
#     `Rebalancing`.
#   - Prometheus: `telemetry_beacons_ingested_total` (rate) should drop to
#     ~0 during the outage and recover afterward — see the "Analytics
#     ingestion throughput" panel on
#     infra/observability/grafana/dashboards/platform-overview-dashboard.json.
#   - ClickHouse: `SELECT count() FROM playback_beacons WHERE eventTimestamp
#     BETWEEN <outage_start> AND <outage_end>` to directly confirm the
#     ingestion gap lines up with the outage window (and does not extend
#     beyond it, which would indicate the consumer failed to resume).
#   - `docker compose ps redpandaTelemetryStreamBroker` / `docker ps` to
#     confirm the broker container's own state through each phase.
#
# This script is safe to actually execute against a local docker-compose
# stack for the STOP variant (only affects the local Redpanda container).
# The PARTITION (tc/netem or network-disconnect) variant is documented
# above but left as a manual/`--execute-partition` step because it
# requires container-runtime/network-name specifics (project name,
# capability to run `tc` inside the container's netns) that vary by host
# and were not verified against a live Docker network in this environment.

set -euo pipefail

COMPOSE_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/infra/docker-compose.yml"
REDPANDA_SERVICE_NAME="redpandaTelemetryStreamBroker"
MODE="${1:-}"

echo "=== chaos experiment: partition-redpanda-verify-analytics-consumer-survives ==="
echo "compose file      : ${COMPOSE_FILE}"
echo "redpanda service  : ${REDPANDA_SERVICE_NAME}"
echo "hypothesis         : analytics-telemetry-platform's kafkajs consumer survives broker loss without a process crash"
echo

case "${MODE}" in
  --execute-stop)
    echo "[EXECUTE] Stopping ${REDPANDA_SERVICE_NAME}..."
    docker compose -f "${COMPOSE_FILE}" stop "${REDPANDA_SERVICE_NAME}"
    echo "Redpanda stopped. Watch the analytics-telemetry-platform consumer"
    echo "process logs for kafkajs connection-retry activity (not a crash)."
    read -r -p "Press Enter to restart Redpanda... " _
    docker compose -f "${COMPOSE_FILE}" start "${REDPANDA_SERVICE_NAME}"
    echo "Redpanda restarted. Confirm the consumer resumes without a manual restart."
    ;;
  --execute-partition)
    echo "[EXECUTE] Network-disconnecting ${REDPANDA_SERVICE_NAME} to simulate a partition..."
    echo "NOTE: adjust NETWORK_NAME below to match 'docker network ls' output"
    echo "for this compose project (commonly <dir>_default)."
    NETWORK_NAME="${CHAOS_DOCKER_NETWORK_NAME:-infra_default}"
    docker network disconnect "${NETWORK_NAME}" "${REDPANDA_SERVICE_NAME}"
    echo "Disconnected from network '${NETWORK_NAME}'. Watch consumer logs."
    read -r -p "Press Enter to reconnect Redpanda to the network... " _
    docker network connect "${NETWORK_NAME}" "${REDPANDA_SERVICE_NAME}"
    echo "Reconnected. Confirm the consumer resumes without a manual restart."
    ;;
  *)
    echo "[DRY RUN] Would run one of:"
    echo "  $0 --execute-stop        # docker compose stop/start ${REDPANDA_SERVICE_NAME}"
    echo "  $0 --execute-partition   # docker network disconnect/connect ${REDPANDA_SERVICE_NAME}"
    echo
    echo "See this script's header comment for the full hypothesis,"
    echo "procedure, pass criteria, and how to observe the result."
    exit 0
    ;;
esac
