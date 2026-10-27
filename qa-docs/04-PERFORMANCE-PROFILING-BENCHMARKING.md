# custom-video-processing-engine — Performance, Profiling & Benchmarking Reference (04)

Audience: LLM agents / engineers. Companion to 01/02/03. **[src]** = read from source. Targets are hypotheses to calibrate. Nothing executed.

## 0. Limits
Covers encoding throughput/efficiency, workflow orchestration latency, packaging/manifest latency, CDN routing decision latency, telemetry pipeline throughput/lag, ML inference latency and control-plane API performance for the listed workloads. Passing does not prove absence of performance defects for other content types, ladders, hardware (GPU/CPU), or traffic shapes.

## 1. Performance model [src]

| Service | Lang | Metrics port (Prometheus scrape [src infra/observability/prometheus.yml]) | Perf-critical work |
|---|---|---|---|
| control-plane-api-gateway | TS (NestJS GraphQL) | 4000 (confirmed) | GraphQL ops `registerRawVideoAsset`, `startEncodingForVideoAsset`, `encodingWorkflowProgressForVideoAsset`, `createPlaybackSession`; dev-jwt auth |
| ingestion-inspection-service | Python FastAPI | 8080 (confirmed) | ffprobe/inspection of source; upload handling |
| distributed-encoding-orchestrator | Go + Temporal (7233) | 9100 (ASSUMED in prometheus.yml) | chunked parallel ffmpeg encodes, worker pool (`MaxConcurrentTasks`), convex-hull bitrate ladder |
| edge-packaging-and-manifest-service | Go | 8081 scrape (8080 in container) | JIT packaging, personalized HLS/DASH manifests, DRM |
| multi-cdn-edge-routing-service | Go | 8082 (ASSUMED) | routing decision per request, prefetch prediction |
| analytics-telemetry-platform | TS | 9464 (ASSUMED) | Redpanda topic `qoe-playback-beacons` → ClickHouse `telemetry_lake.playback_beacons` |
| predictive-qoe-ml-service | Python | 8000 (uvicorn default) | QoE model inference |
| Infra | | | Redis 6379, Postgres(Temporal) 5432, Temporal 7233, Redpanda 9092, ClickHouse 8123/9000, MinIO 9010/9011, Prometheus 9090, Grafana 3000 |

Existing metrics [src]:
- orchestrator (namespace `distributed_encoding_orchestrator`): `active_task_count`, `max_concurrent_task_count`, `chunk_encoding_tasks_completed_total`, `encoding_jobs_completed_total`, `resource_seconds_consumed_total`, `estimated_cost_usd_total` (cost model ≈ `TotalCpuSeconds × 0.00003` [src]).
- packaging: `jit_packaging_duration_seconds`, `manifest_generation_duration_seconds`, `drm_requests_total`.
- routing: `multi_cdn_http_requests_total`, `multi_cdn_routing_decisions_total`, `multi_cdn_routing_no_healthy_provider_total`, `multi_cdn_prefetch_predictions_emitted_total`, `multi_cdn_prefetch_predictions_no_pattern_total`.
- analytics: OpenTelemetry setup `src/tracing/otelSetup.ts`.

Existing Go benchmarks [src]: `BenchmarkPersonalizedHlsManifestGeneration`, `BenchmarkPersonalizedDashManifestGeneration` (packaging/manifest), `BenchmarkJitPackageChunkEndpoint`, `BenchmarkPersonalizedManifestEndpoint` (packaging/httpapi), `BenchmarkComputeUpperConvexHullFromRateDistortionSamples` (orchestrator/ladder), `BenchmarkWorkerPool_DispatchThroughput` (orchestrator/worker).

Cost model of encoding: wall time ≈ (source_duration × Σ_renditions encode_cost_factor) / effective_parallel_workers + chunk split/merge + upload. ffmpeg speed (`speed=` field, realtime multiple) per preset/codec/resolution is the primary unit.

## 2. Environment rules
Record CPU model/cores/AVX-512 support, GPU (if NVENC/QSV used), ffmpeg version and build flags (`ffmpeg -buildconf`), codec libs (x264/x265/SVT-AV1 versions), Temporal version, Redpanda partitions per topic, ClickHouse version and table engine/ORDER BY, Python versions, Node version. Fixed test corpus with checksums (§3.2). Warm-up: one discarded encode; JIT/package caches warmed for packaging tests. 3 reps, CV < 5% (encoding is deterministic per preset; variance mostly from scheduling).

## 3. Data-retrieval contract
`perf-results/video/<YYYYMMDD-HHMM>-<sha>/<scenario>/`: `meta.json`, `corpus.sha256`, `ffmpeg/<rendition>.log` (with `-benchmark -stats_period 1`), `ffmpeg/vmaf-<rendition>.json`, `temporal/{workflow-history-<id>.json, sched_to_start.csv}`, `k6-*.json`, `rpk/{lag-<ts>.txt}`, `clickhouse/{query_log.csv,parts.csv,ingest_lag.csv}`, `prom/*.csv`, `profiles/<service>/*`, `summary.json`.

```json
{"project":"video","scenario":"PERF-ENC-03","git":"…","corpus":"bbb_1080p60_120s","ladder":"6 rungs h264",
 "wall_s":94.2,"realtime_factor":1.27,"cpu_core_s":2210,"cost_usd_model":0.0663,
 "per_rung":[{"res":"1080p","kbps":5800,"fps":212,"speed":3.5,"vmaf_mean":95.1,"vmaf_p5":89.9,"bytes":87000000}],
 "workflow":{"sched_to_start_p99_ms":180,"activity_retries":0},"chunks":24,"workers":8,"pass":true}
```
Retrieval: `jq -s 'map(select(.corpus=="bbb_1080p60_120s"))|.[]|[.ts,.git,.wall_s,.realtime_factor,.cost_usd_model]|@tsv' history.jsonl`.

### 3.1 PromQL set
```
enc_active      = distributed_encoding_orchestrator_active_task_count
enc_util        = distributed_encoding_orchestrator_active_task_count / distributed_encoding_orchestrator_max_concurrent_task_count
chunks_s        = rate(distributed_encoding_orchestrator_chunk_encoding_tasks_completed_total[1m])
jobs_s          = rate(distributed_encoding_orchestrator_encoding_jobs_completed_total[5m])
cost_rate       = rate(distributed_encoding_orchestrator_estimated_cost_usd_total[5m])
pkg_p99         = histogram_quantile(0.99, sum by (le)(rate(jit_packaging_duration_seconds_bucket[1m])))
manifest_p99    = histogram_quantile(0.99, sum by (le)(rate(manifest_generation_duration_seconds_bucket[1m])))
route_rps       = rate(multi_cdn_routing_decisions_total[30s]); no_healthy = rate(multi_cdn_routing_no_healthy_provider_total[1m])
temporal_s2s    = histogram_quantile(0.99, sum by (le,activity_type)(rate(temporal_activity_schedule_to_start_latency_bucket[1m])))   # Temporal SDK metrics, if exported (VERIFY)
```
### 3.2 Test corpus (fixed; store checksums)
| Name | Properties | Purpose |
|---|---|---|
| `bbb_1080p60_120s.mp4` | Big Buck Bunny, h264 1080p60 | baseline animation |
| `tos_4k_60s.mov` | Tears of Steel 4K, high detail | worst-case complexity |
| `talking_head_720p_300s` | low motion | ladder pruning |
| `sports_1080p50_60s` | high motion | rate-control stress |
| `vfr_phone_capture.mp4` | variable frame rate, rotation metadata | inspection/timing correctness |
| `10bit_hdr_hevc_30s.mkv` | HDR10 | color pipeline cost |
| `audio_only_5ch.m4a` | | edge |
| `long_2h_720p.mp4` | 2 h | chunking scale, memory |
Generate synthetic: `ffmpeg -f lavfi -i testsrc2=size=1920x1080:rate=60 -f lavfi -i sine=frequency=1000 -t 120 -c:v libx264 -preset veryfast -c:a aac synth_1080p60.mp4`.

## 4. Tool matrix
| Need | Tool | Command |
|---|---|---|
| ffmpeg throughput | `ffmpeg -benchmark -stats_period 1`, `hyperfine` | `hyperfine -w1 -r3 'ffmpeg -y -i in.mp4 -c:v libx264 -preset medium -b:v 5M -f null -'` |
| Quality | `libvmaf` (VMAF/PSNR/SSIM), BD-rate scripts | `ffmpeg -i dist.mp4 -i ref.mp4 -lavfi libvmaf=log_fmt=json:log_path=vmaf.json:n_threads=8 -f null -` |
| Encode CPU profile | `perf record -g -- ffmpeg …`, `perf stat` (IPC, AVX usage `-e fp_arith_inst_retired.512b_packed_single` on Intel) | |
| Go services | pprof, `go test -bench`, benchstat, `go tool trace` | |
| Node services | `clinic doctor|flame|bubbleprof`, `0x`, `node --cpu-prof --heap-prof`, `--inspect` + Chrome DevTools, `autocannon` | |
| GraphQL load | k6 (`http.post` with query), `graphql-bench` | |
| Python services | `py-spy record -o flame.svg --pid`, `py-spy dump`, `scalene`, `memray`, `locust` | |
| Temporal | Temporal UI, `tctl/temporal workflow show --output json`, SDK metrics, `temporal-bench`/omes (load generator for Temporal) | schedule-to-start latency |
| Redpanda | `rpk topic produce/consume`, `rpk group describe <group>` (lag), `rpk cluster partitions`, Redpanda `/public_metrics` (`:9644`) | |
| ClickHouse | `system.query_log`, `system.parts`, `system.metric_log`, `clickhouse-benchmark` | |
| MinIO | `warp`, `mc admin trace`, MinIO Prometheus metrics | upload/download of renditions |
| Packaging HTTP | wrk2/vegeta/k6 | manifests and chunks |

## 5. Micro-benchmarks
| ID | Benchmark | Params | Record |
|---|---|---|---|
| MB-01 | `BenchmarkPersonalizedHlsManifestGeneration` [src] | renditions 3/8/16, segments 100/10k (2 h VOD), ad markers | ns/op, B/op |
| MB-02 | `BenchmarkPersonalizedDashManifestGeneration` [src] | same | ns/op, B/op |
| MB-03 | `BenchmarkJitPackageChunkEndpoint` [src] | segment 2/6 s, 1080p bitrate | MB/s, allocs |
| MB-04 | `BenchmarkPersonalizedManifestEndpoint` [src] | | ns/op incl. HTTP |
| MB-05 | `BenchmarkComputeUpperConvexHullFromRateDistortionSamples` [src] | samples 10/100/10k | ns/op (O(n log n) check) |
| MB-06 | `BenchmarkWorkerPool_DispatchThroughput` [src] | MaxConcurrentTasks 1/8/64 | tasks/s |
| MB-07 (add) | routing decision function | providers 2/5/20, health states | ns/op |
| MB-08 (add) | prefetch predictor | history 10/1k | ns/op |
| MB-09 (add, Node) | beacon parse/validate + ClickHouse row build | batch 1/1000 | ops/s (`benchmark.js`/`tinybench`) |
| MB-10 (add, Python) | QoE model inference | batch 1/32/256, features | ms/batch (`pytest-benchmark`) |
```bash
cd services/edge-packaging-and-manifest-service && go test -run='^$' -bench=. -benchmem -count=10 ./internal/... | tee $OUT/pkg-micro.txt
cd ../distributed-encoding-orchestrator && go test -run='^$' -bench=. -benchmem -count=10 ./internal/... | tee $OUT/orch-micro.txt
benchstat base/pkg-micro.txt $OUT/pkg-micro.txt
```

## 6. Macro scenarios
### 6.1 Encoding
| ID | Workload | Measure | Target (calibrate) |
|---|---|---|---|
| PERF-ENC-01 | Single rendition encode per codec/preset (x264 veryfast/medium/slow; x265 medium; SVT-AV1 p8/p6) on 1080p corpus | fps, speed×, CPU core-seconds, VMAF, bytes | table per codec/preset |
| PERF-ENC-02 | Full ladder (6 rungs) single-pass vs chunked parallel (chunks 2/6/12 s) | wall time, realtime factor, VMAF delta at chunk boundaries (keyframe alignment) | chunked VMAF ≥ unchunked − 0.5 |
| PERF-ENC-03 | Workers 1/2/4/8/16 (MaxConcurrentTasks) | wall time vs workers; USL fit; `enc_util` | efficiency ≥ 70% at 8 |
| PERF-ENC-04 | 50 concurrent jobs of mixed corpus | queue wait (Temporal schedule-to-start), throughput jobs/h, fairness | |
| PERF-ENC-05 | 2 h source | memory of orchestrator, chunk count, merge time | bounded RSS |
| PERF-ENC-06 | Convex-hull ladder vs fixed ladder | total bits saved at equal VMAF (BD-rate), extra probe-encode cost | |
| PERF-ENC-07 | Cost model accuracy | `estimated_cost_usd_total` vs measured CPU-seconds × price | error < 10% |
| PERF-ENC-08 | Worker crash mid-chunk (kill ffmpeg) | retry overhead, wasted CPU-seconds | only failed chunk re-encoded |

### 6.2 Ingestion / control plane
| ID | Workload | Measure |
|---|---|---|
| PERF-ING-01 | Upload 100 MB / 2 GB / 20 GB sources (to MinIO via ingestion) | MB/s, RSS of FastAPI (streaming vs buffering), inspection time |
| PERF-ING-02 | 100 concurrent `registerRawVideoAsset` | GraphQL p99, DB/Temporal calls |
| PERF-API-01 | `encodingWorkflowProgressForVideoAsset` polled by 1k clients every 2 s | gateway p99, Temporal query load (workflow queries are expensive) |
| PERF-API-02 | `createPlaybackSession` at 500/2000 rps | p99, auth overhead (dev-jwt) |
| PERF-API-03 | GraphQL depth/complexity abuse (deep nested query) | CPU per request — no complexity limit ⇒ finding |

### 6.3 Delivery
| ID | Workload | Measure | Target |
|---|---|---|---|
| PERF-PKG-01 | Manifest requests (personalized) 1k→20k rps | p99, `manifest_p99`, CPU | p99 < 20 ms |
| PERF-PKG-02 | JIT chunk packaging 500→5000 rps, 6 s 1080p segments | MB/s, `pkg_p99`, cache hit | |
| PERF-PKG-03 | Cold cache vs warm cache (origin MinIO fetch) | TTFB delta | |
| PERF-PKG-04 | DRM license path | `drm_requests_total` rate, p99 | |
| PERF-CDN-01 | Routing decisions 10k→100k rps | p99 decision latency, `no_healthy` rate | p99 < 2 ms |
| PERF-CDN-02 | Provider health flapping during load | decision stability, latency | |
| PERF-PLAY-01 | Synthetic player sessions (HLS) 1k concurrent | startup time (manifest + first 2 segments), rebuffer ratio | |

### 6.4 Telemetry pipeline
| ID | Workload | Measure |
|---|---|---|
| PERF-TEL-01 | Produce beacons to `qoe-playback-beacons` 1k→100k msg/s | consumer lag (rpk), ClickHouse insert rate, e2e lag (beacon ts → row visible) |
| PERF-TEL-02 | Insert batch size effect (1 row vs 10k rows per INSERT) | `system.parts` count growth ("too many parts" risk), merges |
| PERF-TEL-03 | Dashboard queries over 1B rows (p50 bitrate by CDN/hour) | `query_duration_ms`, `read_rows`, `memory_usage` from `system.query_log` |
| PERF-TEL-04 | Consumer restart under load | catch-up rate, duplicate rows |
| PERF-ML-01 | QoE inference 50→2000 rps, batch 1/32 | p99, CPU, py-spy profile, GIL contention (workers=1 vs N) |

### 6.5 Stress / soak
| ID | Procedure | Pass |
|---|---|---|
| PERF-SOAK-01 | 24 h: 1 encode job/5 min + 2k rps manifests + 20k beacons/s | all service RSS flat; Temporal history size bounded; ClickHouse parts bounded; MinIO usage = expected renditions |
| PERF-STRESS-01 | Queue 500 encode jobs at once | no worker over-subscription (active ≤ max), Temporal stays responsive (UI/API p99) |

## 7. Profiling playbooks
### 7.1 ffmpeg
```bash
ffmpeg -hide_banner -benchmark -stats_period 1 -progress $OUT/ffmpeg/progress.txt -y -i in.mp4 -c:v libx264 -preset medium -b:v 5M out.mp4 2> $OUT/ffmpeg/1080p.log
grep -E 'bench: utime|bench: maxrss' $OUT/ffmpeg/1080p.log                 # CPU and max RSS
perf stat -e cycles,instructions,cache-misses -- ffmpeg … 2> $OUT/ffmpeg/perfstat.txt
perf record -g -- ffmpeg … ; perf report --sort dso,symbol | head -50     # time in libx264 vs swscale vs demux
```
Threads: compare `-threads 0` vs per-chunk `-threads 2` with more parallel chunks (often better total throughput).
### 7.2 Go services
Perf build with `net/http/pprof` on 127.0.0.1:606x; capture cpu/heap/allocs/mutex/goroutine + `trace` (same commands as other Go projects). Packaging hot spots expected: string building of manifests (use `strings.Builder`, pre-size), MP4 box rewriting allocations.
### 7.3 Node services
```bash
node --cpu-prof --cpu-prof-dir=$OUT/profiles/gateway --heap-prof dist/main.js       # stop after load; open .cpuprofile in DevTools/speedscope
npx clinic flame --dest $OUT/profiles/gateway -- node dist/main.js                  # run k6 meanwhile
npx clinic doctor -- node dist/main.js                                              # event-loop delay, GC, handles
```
Measure event-loop lag: `perf_hooks.monitorEventLoopDelay()` histogram exported to logs every 10 s (p99 > 50 ms ⇒ blocking work, e.g., large JSON/GraphQL resolution).
### 7.4 Python services
```bash
py-spy record -r 200 -d 60 -o $OUT/profiles/qoe/flame.svg --pid $(pgrep -f uvicorn) --subprocesses
py-spy dump --pid … > $OUT/profiles/qoe/dump.txt
memray run -o $OUT/profiles/ingest/mem.bin -m uvicorn app:app; memray flamegraph $OUT/profiles/ingest/mem.bin
```
### 7.5 Temporal
`temporal workflow show -w <id> -o json > history.json` → compute per-activity `ActivityTaskScheduled → Started` (schedule-to-start) and `Started → Completed`; export CSV. High schedule-to-start ⇒ not enough workers/pollers; high history size ⇒ use continue-as-new for long jobs.
### 7.6 Redpanda / ClickHouse
```bash
rpk group describe <analytics-consumer-group> > $OUT/rpk/lag-$(date +%s).txt          # every 10 s
curl -s localhost:9644/public_metrics | grep -E 'kafka_consumer_group_lag|request_latency' > $OUT/rpk/metrics.txt
clickhouse-client -q "SELECT event_time, query_duration_ms, read_rows, read_bytes, memory_usage, left(query,120) FROM system.query_log WHERE type='QueryFinish' AND event_time > now()-3600 ORDER BY query_duration_ms DESC LIMIT 50 FORMAT CSVWithNames" > $OUT/clickhouse/query_log.csv
clickhouse-client -q "SELECT table, count() parts, sum(rows), formatReadableSize(sum(bytes_on_disk)) FROM system.parts WHERE active AND database='telemetry_lake' GROUP BY table FORMAT CSVWithNames" > $OUT/clickhouse/parts.csv
clickhouse-client -q "SELECT max(now() - toDateTime(<beacon_ts_col>)) FROM telemetry_lake.playback_beacons WHERE <ingest_ts> > now()-60"   # e2e lag (VERIFY columns)
```

## 8. Scripts
### 8.1 Encode benchmark matrix
```bash
#!/usr/bin/env bash
OUT=$1; IN=$2; mkdir -p $OUT/ffmpeg
for P in "libx264 veryfast" "libx264 medium" "libx265 medium" "libsvtav1 8"; do set -- $P; C=$1; PR=$2
  for R in "1920x1080 5000k" "1280x720 2800k" "854x480 1200k"; do set -- $R; S=$1; B=$2; tag="$C-$PR-${S}-$B"
    /usr/bin/time -v ffmpeg -hide_banner -benchmark -y -i $IN -vf scale=$S -c:v $C -preset $PR -b:v $B -an $OUT/ffmpeg/$tag.mp4 2> $OUT/ffmpeg/$tag.log
    ffmpeg -i $OUT/ffmpeg/$tag.mp4 -i $IN -lavfi "[0:v]scale=1920x1080:flags=bicubic[d];[d][1:v]libvmaf=log_fmt=json:log_path=$OUT/ffmpeg/$tag.vmaf.json:n_threads=8" -f null - 2>/dev/null
    printf "%s\t%s\t%s\t%s\n" $tag "$(grep -oE 'speed= *[0-9.]+x' $OUT/ffmpeg/$tag.log | tail -1)" \
      "$(jq '.pooled_metrics.vmaf.mean' $OUT/ffmpeg/$tag.vmaf.json)" "$(stat -c%s $OUT/ffmpeg/$tag.mp4 2>/dev/null || stat -f%z $OUT/ffmpeg/$tag.mp4)"
  done; done | tee $OUT/encode_matrix.tsv
```
### 8.2 End-to-end pipeline timing (GraphQL)
```bash
G=http://localhost:4000/graphql; H="Authorization: Bearer $DEV_JWT"
t0=$(date +%s.%N)
ID=$(curl -s $G -H "$H" -H 'content-type: application/json' -d '{"query":"mutation{registerRawVideoAsset(input:{sourceUri:\"s3://raw/bbb.mp4\"}){id}}"}' | jq -r '.data.registerRawVideoAsset.id')   # VERIFY input shape
curl -s $G -H "$H" -H 'content-type: application/json' -d "{\"query\":\"mutation{startEncodingForVideoAsset(videoAssetId:\\\"$ID\\\"){workflowId}}\"}"
until curl -s $G -H "$H" -H 'content-type: application/json' -d "{\"query\":\"{encodingWorkflowProgressForVideoAsset(videoAssetId:\\\"$ID\\\"){status percentComplete}}\"}" | grep -q COMPLETED; do sleep 2; done
echo "e2e_s=$(echo "$(date +%s.%N)-$t0" | bc)" | tee -a $OUT/e2e.txt
```
### 8.3 k6 manifests + chunks
```javascript
import http from 'k6/http';
export const options={scenarios:{m:{executor:'constant-arrival-rate',rate:+__ENV.RPS||2000,timeUnit:'1s',duration:'5m',preAllocatedVUs:300,maxVUs:3000}},
  thresholds:{'http_req_duration':['p(99)<50'],'http_req_failed':['rate<0.001']}, summaryTrendStats:['med','p(95)','p(99)','p(99.9)','max']};
const P=__ENV.PKG||'http://localhost:8081';
export default function(){ const u=`viewer-${Math.floor(Math.random()*1e5)}`;
  http.get(`${P}/manifests/${__ENV.ASSET}/master.m3u8?viewer=${u}`, {tags:{k:'manifest'}});            // VERIFY routes in packaging httpapi
  http.get(`${P}/chunks/${__ENV.ASSET}/1080p/seg_${Math.floor(Math.random()*60)}.m4s`, {tags:{k:'chunk'}}); }
```
### 8.4 Beacon producer
```bash
python3 - <<'EOF'
import json,time,random
from kafka import KafkaProducer   # pip install kafka-python
p=KafkaProducer(bootstrap_servers='localhost:9092',linger_ms=5,batch_size=65536,value_serializer=lambda v: json.dumps(v).encode())
RATE=20000; n=0; t0=time.time()
while time.time()-t0<300:
    p.send('qoe-playback-beacons',{'sessionId':f's{random.randrange(1e5)}','ts':int(time.time()*1000),'bitrateKbps':random.choice([800,2800,5000]),
           'rebufferMs':random.choice([0,0,0,120]),'cdn':random.choice(['a','b','c'])})   # VERIFY beacon schema in analytics service
    n+=1
    if n%RATE==0: time.sleep(max(0,t0+n/RATE-time.time()))
p.flush(); print('sent',n)
EOF
```

## 9. Scalability
Encoding: USL over workers (contention from shared disk/MinIO, κ from merge step). Packaging/routing: GOMAXPROCS sweep. Telemetry: partitions × consumer instances (max parallelism = partitions). ClickHouse: rows/s per insert batch.

## 10. Regression gates
Encode matrix: speed −5% or VMAF −0.3 at same bitrate ⇒ fail; benchstat on MB-01..06; PERF-PKG-01 p99 ≤ 1.2× at fixed rps; telemetry e2e lag p99 ≤ 1.2×.

## 11. Report template
```
Run <date> sha <sha> | CPU/GPU | ffmpeg build | codecs | workers | corpus checksums
Encode table: | corpus | codec/preset | res/bitrate | fps | speed× | VMAF mean/p5 | bytes | core-s |
Pipeline: e2e seconds per corpus; schedule-to-start p99; retries
Delivery: | endpoint | rps | p50 | p99 | cache hit |
Telemetry: msgs/s | lag p99 | CH insert rows/s | parts | query p99
Profiles: top frames per service; findings
```

## 12. Risks from source
| # | Risk | Scenario |
|---|---|---|
| R1 | Several Prometheus ports are ASSUMED in prometheus.yml (orchestrator 9100, analytics 9464, routing 8082) — metrics may be missing | §3.1 verify first |
| R2 | ffmpeg without `-protocol_whitelist` (02) — perf runs must use local files only | corpus |
| R3 | Progress polling via GraphQL → Temporal queries is expensive at scale | PERF-API-01 |
| R4 | Cost metric is a fixed CPU-seconds multiplier — accuracy unknown | PERF-ENC-07 |
| R5 | Small ClickHouse inserts ⇒ too many parts | PERF-TEL-02 |
| R6 | GraphQL without complexity limits | PERF-API-03 |
