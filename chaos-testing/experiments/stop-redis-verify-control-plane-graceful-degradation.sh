#!/usr/bin/env bash
#
# stop-redis-verify-control-plane-graceful-degradation.sh
#
# CHAOS EXPERIMENT — control-plane-api-gateway Redis dependency failure
#
# HYPOTHESIS
#   Stopping `redisSessionAndCacheStore` does not crash
#   control-plane-api-gateway: the `ioredis` client
#   (services/control-plane-api-gateway/src/redis/redisClientProvider.ts)
#   is constructed with `maxRetriesPerRequest: 3` and
#   `enableOfflineQueue: true`, so in-flight and new Redis-backed calls
#   (manifest cache-aside reads via manifestCacheAsideService.ts, rate
#   limiting via redisBackedRateLimitingGuard.ts) fail fast with
#   `ECONNREFUSED`-class errors instead of hanging indefinitely, and the
#   NestJS process itself keeps running and keeps serving GraphQL
#   requests that do not depend on Redis. This reproduces, as a runnable
#   chaos experiment, the exact behavior already observed and recorded in
#   docs/testing-and-benchmarking.md: "the compiled app was booted
#   directly (`node dist/main.js`); Redis `ECONNREFUSED` errors appeared
#   as expected (no live Redis in this sandbox) without crashing the
#   process."
#
# REAL CODE UNDER TEST
#   - services/control-plane-api-gateway/src/redis/redisClientProvider.ts
#     (maxRetriesPerRequest: 3, enableOfflineQueue: true)
#   - services/control-plane-api-gateway/src/redis/manifestCacheAsideService.ts
#     (cache-aside manifest resolution — degrades to cache-miss behavior)
#   - services/control-plane-api-gateway/src/common/rate-limiting/redisBackedRateLimitingGuard.ts
#     (rate limiter — must not deadlock the request pipeline if Redis is down)
#   - services/control-plane-api-gateway/src/common/circuit-breaker/circuitBreakerUtility.ts
#     (the general-purpose circuit breaker utility available to wrap
#     exactly this kind of dependency call)
#
# PROCEDURE
#   1. Start the full stack: `cd infra && docker compose up -d`, then
#      start control-plane-api-gateway (`npm run start:dev` or
#      `node dist/main.js`) so it establishes its initial Redis
#      connection.
#   2. Confirm baseline: a GraphQL request that touches Redis (e.g.
#      `resolvePersonalizedManifestUrlForPlaybackSession`) succeeds.
#   3. Stop Redis WITHOUT stopping the gateway:
#        docker compose -f infra/docker-compose.yml stop redisSessionAndCacheStore
#      (a real outage; `docker compose pause` is an alternative if you
#      want a resumable/frozen-process variant of this experiment rather
#      than a full stop).
#   4. Re-issue the same GraphQL request (and a rate-limited mutation,
#      e.g. `createPlaybackSession`) against the still-running gateway.
#   5. Restart Redis and confirm the gateway recovers on its own without
#      a restart (ioredis reconnects automatically by default):
#        docker compose -f infra/docker-compose.yml start redisSessionAndCacheStore
#
# EXPECTED PASS CRITERIA
#   - The NestJS process (`node dist/main.js` or `nest start --watch`)
#     stays alive throughout steps 3-4 — no process exit, no unhandled
#     promise rejection crash.
#   - GraphQL requests during the outage return a clean HTTP response
#     (a GraphQL error payload or a 5xx from the resolver), NOT a hung
#     connection — `maxRetriesPerRequest: 3` bounds how long ioredis will
#     retry a single command before surfacing the error.
#   - Once Redis is restarted in step 5, subsequent requests succeed again
#     without restarting the gateway process (`ioredis` auto-reconnects).
#
# HOW TO OBSERVE THE RESULT
#   - Gateway stdout/stderr: look for `ECONNREFUSED` / `Redis connection
#     ... closed` lines from ioredis during the outage window, and a
#     reconnection log line after step 5.
#   - Prometheus: the `redis_client_connected{service="control-plane-api-gateway"}`
#     gauge (see the "control-plane-api-gateway Redis connectivity" stat
#     panel on infra/observability/grafana/dashboards/platform-overview-dashboard.json)
#     should drop from 1 to 0 during the outage and back to 1 after
#     recovery.
#   - Prometheus: `http_requests_total{service="control-plane-api-gateway",
#     status_code=~"5.."}` rate should tick up during the outage for
#     Redis-dependent resolvers specifically (manifest resolution, rate
#     -limited mutations) while Redis-independent queries keep succeeding.
#   - `curl` directly against the gateway during the outage to confirm it
#     is still accepting TCP connections at all (process liveness), even
#     if individual GraphQL operations return errors.
#
# This script drives the real docker-compose service by name
# (`redisSessionAndCacheStore`, exactly as declared in
# infra/docker-compose.yml) and is safe to actually execute against a
# local docker-compose stack — no --execute gate is needed for the
# stop/start actions themselves, since they only affect the local Redis
# container. It IS gated so it isn't run accidentally as a side effect of
# sourcing/linting this file.

set -euo pipefail

COMPOSE_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/infra/docker-compose.yml"
REDIS_SERVICE_NAME="redisSessionAndCacheStore"
EXECUTE="${1:-}"

echo "=== chaos experiment: stop-redis-verify-control-plane-graceful-degradation ==="
echo "compose file      : ${COMPOSE_FILE}"
echo "redis service     : ${REDIS_SERVICE_NAME}"
echo "hypothesis         : control-plane-api-gateway degrades (ECONNREFUSED, no crash) rather than hard-failing when Redis is unavailable"
echo

if [[ "${EXECUTE}" != "--execute" ]]; then
  echo "[DRY RUN] Would run, in order:"
  echo "  docker compose -f \"${COMPOSE_FILE}\" stop ${REDIS_SERVICE_NAME}"
  echo "  # ... issue GraphQL requests against the running gateway here ..."
  echo "  docker compose -f \"${COMPOSE_FILE}\" start ${REDIS_SERVICE_NAME}"
  echo
  echo "Re-run with --execute to actually stop/restart Redis in your local"
  echo "infra/docker-compose.yml stack. Make sure control-plane-api-gateway"
  echo "is already running separately (npm run start:dev) before executing,"
  echo "so you can observe its behavior live during the outage window."
  exit 0
fi

echo "[EXECUTE] Stopping ${REDIS_SERVICE_NAME}..."
docker compose -f "${COMPOSE_FILE}" stop "${REDIS_SERVICE_NAME}"
echo "Redis stopped. control-plane-api-gateway should now be serving"
echo "degraded responses (ECONNREFUSED on Redis-backed paths) without"
echo "crashing. Issue some GraphQL requests now, then press Enter to"
echo "restart Redis and observe recovery."
read -r -p "Press Enter to restart Redis... " _
docker compose -f "${COMPOSE_FILE}" start "${REDIS_SERVICE_NAME}"
echo "Redis restarted. Confirm the gateway recovers without a process restart."
