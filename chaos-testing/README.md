# Chaos & Resilience Testing

Documented, runnable chaos experiments verifying that this platform's
resilience mechanisms (circuit breakers, retry policies, graceful
degradation) behave as designed under real failure conditions — worker
death, dependency outage, broker partition.

```
chaos-testing/
├── README.md   (this file)
└── experiments/
    ├── kill-encoding-worker-mid-job-verify-circuit-breaker-retry.sh
    ├── stop-redis-verify-control-plane-graceful-degradation.sh
    └── partition-redpanda-verify-analytics-consumer-survives.sh
```

Each script is self-documenting: its header comment states a
**hypothesis**, a step-by-step **procedure**, explicit **pass criteria**
tied to a real circuit-breaker/retry mechanism in the actual codebase (by
file path), and exactly **which Prometheus metric or log line** to watch
to observe the result. Run any script with no arguments (or `--help`-style
default) to print the same information plus a dry-run of the commands it
would execute.

## The three experiments

| Script | Fault injected | Mechanism under test | Real file(s) |
|---|---|---|---|
| `kill-encoding-worker-mid-job-verify-circuit-breaker-retry.sh` | `docker kill` an encoding worker mid-ffmpeg-subprocess | `FfmpegFailureCircuitBreaker` (CLOSED→OPEN→HALF_OPEN) + `ExponentialBackoffRetryPolicy` | `services/distributed-encoding-orchestrator/internal/ffmpeg/circuitBreaker.go`, `services/distributed-encoding-orchestrator/internal/worker/retryPolicy.go` |
| `stop-redis-verify-control-plane-graceful-degradation.sh` | `docker compose stop` Redis | `ioredis` client's `maxRetriesPerRequest`/`enableOfflineQueue` config; manifest cache-aside; rate limiter | `services/control-plane-api-gateway/src/redis/redisClientProvider.ts`, `manifestCacheAsideService.ts`, `src/common/rate-limiting/redisBackedRateLimitingGuard.ts` |
| `partition-redpanda-verify-analytics-consumer-survives.sh` | `docker compose stop` / `docker network disconnect` Redpanda | `kafkajs` `Consumer` reconnect/rebalance behavior | `services/analytics-telemetry-platform/src/ingestion/kafkaConsumer.ts` |

## How these were verified in this environment

**Documented, not executed against a live multi-service failure
scenario** — consistent with the rest of this monorepo's stated
limitations (no live multi-node cluster was exercised here; see
`docs/testing-and-benchmarking.md` "What Was Not Exercised Live"). What
**was** actually done:

- All three scripts pass `bash -n <script>.sh` (shell syntax validation).
- All three scripts were **actually executed** in their default dry-run
  mode in this environment, and produced the expected printed
  hypothesis/procedure/dry-run-command output shown in each script's
  header — confirming the scripts themselves are runnable, not just
  syntactically parseable.
- `stop-redis-...sh` and `partition-redpanda-...sh` reference the exact
  service names (`redisSessionAndCacheStore`, `redpandaTelemetryStreamBroker`)
  declared in `infra/docker-compose.yml`, so `docker compose stop/start
  <service>` in their `--execute*` paths is real, runnable
  docker-compose syntax against this repository's actual compose file —
  this was checked by reading `infra/docker-compose.yml` directly, not
  assumed.
- The `kill-encoding-worker-...sh` script's `--execute` path targets a
  container name (`distributedEncodingOrchestratorWorker0`) that does
  **not** exist in the current topology, because
  `distributed-encoding-orchestrator`'s only runnable binary today
  (`cmd/orchestrator/main.go`) is an in-process CLI demo, not a
  long-running containerized worker fleet — this is called out explicitly
  in the script's own comments and its dry-run output, rather than
  silently assumed to work.

**Not executed**: none of the three fault-injection actions (`docker
kill`, `docker compose stop redis...`, `docker network disconnect
redpanda...`) were actually run against a live service in this
environment, because no service in this monorepo was running with its
dependent infra up at the same time during this work. Running them for
real requires:

```bash
cd infra && docker compose up -d
# ...and separately start the relevant service(s) per their own README...
```

then invoking a script's `--execute` (or `--execute-stop` /
`--execute-partition`) flag as documented in its header comment.

## Why shell scripts using `docker compose pause/unpause`, `docker kill`, `tc netem`

These are the standard, dependency-free chaos primitives available on any
Docker host without pulling in a dedicated chaos-engineering framework
(Chaos Mesh, Gremlin, etc.) — appropriate for a reference monorepo where
the goal is to document and exercise specific, named failure paths against
specific, named resilience mechanisms, not to stand up a general chaos
platform. `docker compose pause`/`unpause` (freezes/thaws a container's
processes via cgroup freezer, useful for simulating a "hung" dependency
rather than a hard-down one) is mentioned in the task brief; the three
experiments here use the closest-fitting primitive per fault type:
`docker kill` for an abrupt worker death, `docker compose stop/start` for
a clean dependency outage/recovery cycle, and `docker network
disconnect/connect` (with `tc netem` documented as the finer-grained
alternative for partial/latency-based partitions) for a network partition.
