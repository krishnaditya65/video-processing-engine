# custom-video-processing-engine — Test Scripts (reference, NOT executed)

> v2 note: GraphQL op names, topic and table now come from source (see 02 §1); GraphQL argument names (`videoAssetIdentifier`, input type names) and the beacon JSON shape still need confirming in the resolvers and `analytics-telemetry-platform/src/schemas`.

Ports, GraphQL operation names, Kafka topic and ClickHouse table names are placeholders (`$GW`, `registerVideoAsset`, `$TOPIC`, `$CH_TABLE`). Resolve from the resolvers, `services/analytics-telemetry-platform/{src,sql}` and the existing `load-testing/*.js` (authoritative). Needs bash, curl, jq, docker, ffmpeg/ffprobe, k6, python3 (+`websockets`), rpk, clickhouse-client, mc.

## env.sh
```bash
set -euo pipefail
GW=${GW:-http://localhost:3001/graphql}; PKG=${PKG:-http://localhost:8080}; RT=${RT:-http://localhost:8081}; QOE=${QOE:-ws://localhost:8000}; INS=${INS:-http://localhost:8001}; AN=${AN:-http://localhost:3002}
TOPIC=${TOPIC:-qoe-playback-beacons}; CH_TABLE=${CH_TABLE:-telemetry_lake.playback_beacons}; JWT=${JWT:-}; DC="docker compose -f infra/docker-compose.yml"
PASS=0; FAIL=0; ok(){ PASS=$((PASS+1)); echo "PASS $1"; }; ko(){ FAIL=$((FAIL+1)); echo "FAIL $1 :: ${2:-}"; }
gql(){ curl -s $GW -H 'Content-Type: application/json' ${JWT:+-H "Authorization: Bearer $JWT"} -d "$(jq -nc --arg q "$1" --argjson v "${2:-{\}}" '{query:$q,variables:$v}')"; }
ch(){ curl -s 'http://localhost:8123/' --data-binary "$1"; }
summary(){ echo "pass=$PASS fail=$FAIL"; [[ $FAIL == 0 ]]; }
```

## fixtures.sh — generate the media matrix (NEG-001, FUN-005/006)
```bash
. ./env.sh; F=${F:-scripts/fixtures/qa}; mkdir -p $F; g(){ [[ -f $F/$1 ]] || ffmpeg -loglevel error -y "${@:2}" $F/$1; }
g ok_1080p.mp4   -f lavfi -i testsrc2=s=1920x1080:r=30:d=10 -f lavfi -i sine=d=10 -c:v libx264 -g 60 -pix_fmt yuv420p -c:a aac
g ok_4k.mp4      -f lavfi -i testsrc2=s=3840x2160:r=30:d=4 -c:v libx264 -pix_fmt yuv420p
g ok_480p.mp4    -f lavfi -i testsrc2=s=854x480:r=25:d=6 -c:v libx264 -pix_fmt yuv420p
g portrait.mp4   -f lavfi -i testsrc2=s=1080x1920:r=30:d=4 -c:v libx264 -pix_fmt yuv420p
g odd_dims.mp4   -f lavfi -i testsrc2=s=853x479:r=30:d=3 -c:v libx264 -pix_fmt yuv444p
g tiny.mp4       -f lavfi -i testsrc2=s=2x2:r=30:d=1 -c:v libx264 -pix_fmt yuv444p
g one_frame.mp4  -f lavfi -i testsrc2=s=640x360:r=30 -frames:v 1 -c:v libx264
g fps240.mp4     -f lavfi -i testsrc2=s=640x360:r=240:d=2 -c:v libx264
g audio_only.m4a -f lavfi -i sine=d=5 -c:a aac
g video_only.mp4 -f lavfi -i testsrc2=s=1280x720:r=30:d=5 -c:v libx264 -an
g black_silent.mp4 -f lavfi -i color=black:s=1280x720:r=30:d=6 -f lavfi -i anullsrc -shortest -c:v libx264 -c:a aac     # INS should flag black frames + silence
g rotated.mp4    -f lavfi -i testsrc2=s=1280x720:r=30:d=3 -c:v libx264 -metadata:s:v rotate=90
g moov_end.mp4   -f lavfi -i testsrc2=s=640x360:r=30:d=3 -c:v libx264 -movflags 0
g seg_boundary.mp4 -f lavfi -i testsrc2=s=1280x720:r=30:d=6.0333 -c:v libx264 -g 60      # duration = segment len + 1 frame
: > $F/zero.mp4; head -c 200000 $F/ok_1080p.mp4 > $F/truncated.mp4; echo "not a video" > $F/text.mp4; head -c 5000000 /dev/urandom > $F/random.mp4
printf '#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n' > $F/lfi.m3u8.mp4          # NEG-002: must NOT be read
printf '#EXTM3U\n#EXTINF:10,\nhttp://169.254.169.254/latest/meta-data/\n#EXT-X-ENDLIST\n' > $F/ssrf.mp4
ls -la $F
```

## smoke_e2e.sh (SMK, E2E-001, invariants 1–3)
```bash
. ./env.sh
for u in $PKG/healthz $RT/healthz $INS/healthz ${QOE/ws/http}/health; do [[ $(curl -s -o /dev/null -w '%{http_code}' $u) == 200 ]] && ok "SMK-001 $u" || ko "SMK-001 $u"; done
$DC exec -T redisSessionAndCacheStore redis-cli ping | grep -q PONG && ok redis || ko redis
ch 'SELECT 1' | grep -q 1 && ok clickhouse || ko clickhouse; $DC exec -T redpandaTelemetryStreamBroker rpk cluster health | grep -qi 'healthy.*true' && ok redpanda || ko redpanda
# register + encode (operation names: VERIFY)
ID=$(gql 'mutation($i:RegisterRawVideoAssetInputType!){registerRawVideoAsset(input:$i){id}}' '{"i":{"displayTitle":"qa","sourceMezzanineStorageUri":"s3://origin/qa/ok_1080p.mp4"}}' | jq -r '.data[]|.id')
[[ -n $ID && $ID != null ]] && ok register || { ko register; summary; exit 1; }
last=-1; for i in $(seq 180); do p=$(gql 'query($id:ID!){encodingWorkflowProgressForVideoAsset(videoAssetIdentifier:$id){percent status}}' "{\"id\":\"$ID\"}" | jq -c '.data[]'); pc=$(jq -r .percent <<<"$p"); st=$(jq -r .status <<<"$p")
  (( ${pc%.*} < last )) && ko FUN-002 "progress regressed $last -> $pc"; last=${pc%.*}; [[ $st =~ READY|COMPLETED|FAILED ]] && break; sleep 2; done; [[ $st =~ READY|COMPLETED ]] && ok SMK-003 || ko SMK-003 "status=$st"
# manifest crawl: every referenced URI must resolve and be decodable (invariant 3, CON-003)
crawl(){ local m=$(curl -sf "$1") || { ko "manifest $1"; return; }; grep -q '^#EXTM3U' <<<"$m" || { ko "not HLS $1"; return; }
  while read -r u; do [[ -z $u || $u == \#* ]] && continue; [[ $u != http* ]] && u="$(dirname "$1")/$u"
    if [[ $u == *.m3u8* || $u == */variant/* ]]; then crawl "$u"; else c=$(curl -s -o /tmp/seg -w '%{http_code}' "$u"); [[ $c == 200 && -s /tmp/seg ]] || ko "segment $u" $c; fi; done <<<"$m"; }
crawl "$PKG/manifest/$ID?format=hls"; ok crawl-finished
ffprobe -v error -show_entries stream=codec_name,width,height -of json "$PKG/manifest/$ID?format=hls" | jq -e '.streams|length>0' >/dev/null && ok SMK-004-decodable || ko SMK-004-decodable
# ladder never upscales (invariant 2)
curl -s "$PKG/manifest/$ID?format=hls" | grep -o 'RESOLUTION=[0-9x]*' | cut -d= -f2 | awk -Fx '$2>1080{bad=1}END{exit bad}' && ok no-upscale || ko no-upscale
h=$(curl -s "$PKG/manifest/$ID?format=hls&max_height=480" | grep -o 'RESOLUTION=[0-9x]*' | cut -dx -f2 | sort -n | tail -1); [[ ${h:-0} -le 480 ]] && ok FUN-012 || ko FUN-012 "max=$h"
curl -s "$PKG/manifest/$ID?format=dash" | python3 -c 'import sys,xml.dom.minidom as m; m.parse(sys.stdin)' && ok dash-wellformed || ko dash-wellformed
[[ $(curl -s "$RT/route/$ID?geo=US&asn=7922" | jq -r '.url // .cdn // empty') != "" ]] && ok SMK-005 || ko SMK-005
summary
```

## negative.sh (NEG-003/004/006/007)
```bash
. ./env.sh
for p in "/manifest/does-not-exist" "/manifest/x/variant/..%2f..%2f..%2fetc%2fpasswd" "/package/x/../../etc/passwd" "/package/x/-1" "/package/x/99999999999999999999"; do c=$(curl -s -o /tmp/b --path-as-is -w '%{http_code}' "$PKG$p"); grep -q 'root:' /tmp/b && ko "NEG-006 $p" TRAVERSAL; [[ $c == 404 || $c == 400 ]] && ok "NEG-006 $p" || ko "NEG-006 $p" $c; done
for q in 'format=xyz' 'max_height=-1' 'max_height=abc' 'max_height=0' 'drm=nope' 'network=%00'; do c=$(curl -s -o /dev/null -w '%{http_code}' "$PKG/manifest/${ASSET:-x}?$q"); [[ $c != 5* ]] && ok "NEG-006 ?$q ($c)" || ko "NEG-006 ?$q" $c; done
for q in '' 'geo=ZZ' 'geo=US&asn=abc' "geo=$(head -c 5000 /dev/zero | tr '\0' A)"; do c=$(curl -s -o /dev/null -w '%{http_code}' "$RT/route/x?$q"); [[ $c != 5* ]] && ok "NEG-007 [$c] ${q:0:20}" || ko "NEG-007 ${q:0:20}" $c; done
J=$JWT; JWT=; gql '{videoAssets{id}}' | jq -e '.errors' >/dev/null && ok NEG-003-unauth || ko NEG-003-unauth "data returned without auth"; JWT=$J
deep=$(python3 -c "print('{videoAssets'+'{playbackSessions{asset'*40+'{id}'+'}}'*40+'}')"); s=$(date +%s); gql "$deep" | jq -e '.errors' >/dev/null && ok NEG-003-depth || ko NEG-003-depth; (( $(date +%s)-s < 5 )) || ko NEG-003-depth-slow
alias_bomb=$(python3 -c "print('{'+' '.join(f'a{i}:videoAssets{{id}}' for i in range(5000))+'}')"); gql "$alias_bomb" | jq -e '.errors' >/dev/null && ok NEG-003-alias || ko NEG-003-alias
gql '{__schema{types{name}}}' | jq -e '.errors' >/dev/null && ok introspection-off || echo "WARN introspection enabled (must be off in prod)"
# NEG-004 idempotent start: 20 parallel starts => exactly one workflow
seq 20 | xargs -P20 -I{} bash -c ". ./env.sh; gql 'mutation(\$id:ID!){startEncodingForVideoAsset(videoAssetIdentifier:\$id){workflowId}}' '{\"id\":\"${ASSET:-x}\"}'" | jq -r '.data[]?.workflowId' | sort -u | wc -l | { read n; [[ $n -le 1 ]] && ok NEG-004 || ko NEG-004 "$n workflows"; }
summary
```

## bad_media_matrix.sh (NEG-001/002, invariant 1)
```bash
. ./env.sh; F=scripts/fixtures/qa
for f in zero truncated text random lfi.m3u8 ssrf audio_only.m4a tiny one_frame odd_dims fps240 rotated moov_end video_only seg_boundary portrait ok_4k; do fn=$(ls $F/$f* | head -1)
  mc cp -q "$fn" local/origin/qa/ 2>/dev/null || true
  ID=$(gql 'mutation($i:RegisterRawVideoAssetInputType!){registerRawVideoAsset(input:$i){id}}' "{\"i\":{\"displayTitle\":\"$f\",\"sourceMezzanineStorageUri\":\"s3://origin/qa/$(basename $fn)\"}}" | jq -r '.data[]|.id'); st=PENDING
  for i in $(seq 150); do st=$(gql 'query($id:ID!){encodingWorkflowProgressForVideoAsset(videoAssetIdentifier:$id){status failureReason}}' "{\"id\":\"$ID\"}" | jq -r '.data[]|.status'); [[ $st =~ READY|COMPLETED|FAILED|REJECTED ]] && break; sleep 2; done
  [[ $st =~ READY|COMPLETED|FAILED|REJECTED ]] && ok "terminal $f => $st" || ko "STUCK $f" "$st"; done
pgrep -fl ffmpeg && ko zombies "ffmpeg still running after all jobs terminal" || ok no-ffmpeg-zombies
$DC logs 2>&1 | grep -c 'root:x:0' | { read n; [[ $n == 0 ]] && ok NEG-002-no-lfi || ko NEG-002-no-lfi "/etc/passwd content reached logs/output"; }
summary
```

## telemetry_exact.py (CON-002, NEG-008, used with CHA-004/005)
```python
import sys, json, uuid, time, urllib.request, subprocess, os
AN = os.environ.get('AN', 'http://localhost:3002'); TABLE = os.environ.get('CH_TABLE', 'telemetry_lake.playback_beacons'); N = int(sys.argv[1]); run = uuid.uuid4().hex[:8]; acked = 0
ev = lambda i: {'eventId': f'{run}-{i}', 'sessionId': f's{i % 500}', 'type': 'heartbeat', 'timestamp': int(time.time() * 1000), 'bitrateKbps': 3000, 'rebufferMs': i % 7, 'cdn': 'cdn-a'}   # schema: VERIFY against src/schemas
def send(batch):
    global acked
    for a in range(10):
        try:
            r = urllib.request.urlopen(urllib.request.Request(f'{AN}/events', json.dumps({'events': batch}).encode(), {'Content-Type': 'application/json'}), timeout=5)
            if r.status < 300: acked += len(batch); return
        except Exception: time.sleep(min(0.2 * 2 ** a, 5))
for s in range(0, N, 200): send([ev(i) for i in range(s, min(s + 200, N))])
for bad in [{}, {'eventId': 1}, {**ev(0), 'timestamp': 'x'}, {**ev(0), 'rebufferMs': float('nan')}, {**ev(0), 'type': 'nope'}, {'events': 'notalist'}]:   # poison pills must not stop the consumer
    try: urllib.request.urlopen(urllib.request.Request(f'{AN}/events', json.dumps({'events': [bad]}, allow_nan=True).encode(), {'Content-Type': 'application/json'}), timeout=5)
    except Exception: pass
send([ev(i) for i in range(0, 200)])                       # deliberate duplicates -> must dedupe
q = lambda s: subprocess.check_output(['curl', '-s', 'http://localhost:8123/', '--data-binary', s]).decode().strip()
for _ in range(60):
    tot, uniq = q(f"SELECT count(), uniqExact(event_id) FROM {TABLE} FINAL WHERE event_id LIKE '{run}-%'").split()
    if int(uniq) >= N: break
    time.sleep(2)
print(f'acked={acked} rows={tot} unique={uniq} expected_unique={N}'); sys.exit(0 if int(uniq) == N and int(tot) == N else 1)
```

## qoe_ws_load.py (FUN-018, NEG-009, LOAD-005)
```python
import asyncio, json, time, sys, statistics, websockets
URL = sys.argv[1] if len(sys.argv) > 1 else 'ws://localhost:8000'; N = int(sys.argv[2]) if len(sys.argv) > 2 else 1000; lat = []; errs = 0
async def session(i):
    global errs
    try:
        async with websockets.connect(f'{URL}/ws/session/s{i}', open_timeout=10) as ws:
            for k in range(30):
                t = time.perf_counter(); await ws.send(json.dumps({'bufferMs': 4000 - k * 50, 'bitrateKbps': 3000, 'throughputKbps': 5000 - k * 100, 'droppedFrames': k % 3}))   # feature names: VERIFY
                r = json.loads(await asyncio.wait_for(ws.recv(), 5)); lat.append(time.perf_counter() - t); assert r is not None
                await asyncio.sleep(1)
    except Exception: errs += 1
async def abuse():
    for payload in ['{', 'null', '[]', '{"bufferMs":"NaN"}', 'x' * 1_000_000, b'\x00\xff']:
        try:
            async with websockets.connect(f'{URL}/ws/session/abuse') as ws: await ws.send(payload); await asyncio.wait_for(ws.recv(), 2)
        except Exception: pass
    async with websockets.connect(f'{URL}/ws/session/after-abuse') as ws: print('PASS NEG-009 server alive after malformed frames')
async def main():
    await abuse(); await asyncio.gather(*(session(i) for i in range(N)))
    print(f'sessions={N} errors={errs} p50={statistics.median(lat)*1000:.1f}ms p99={statistics.quantiles(lat, n=100)[98]*1000:.1f}ms'); sys.exit(1 if errs or statistics.quantiles(lat, n=100)[98] > 0.1 else 0)
asyncio.run(main())
```

## chaos.sh — extends `chaos-testing/experiments/*` (run those three first)
```bash
. ./env.sh
for e in chaos-testing/experiments/*.sh; do echo "== existing: $e (review, then run manually)"; done
# CHA-002 Temporal outage during encodes: all assets must still reach terminal state exactly once
ids=(); for i in $(seq 10); do ids+=($(gql 'mutation($i:RegisterRawVideoAssetInputType!){registerRawVideoAsset(input:$i){id}}' "{\"i\":{\"displayTitle\":\"c$i\",\"sourceMezzanineStorageUri\":\"s3://origin/qa/ok_1080p.mp4\"}}" | jq -r '.data[]|.id')); done
sleep 10; $DC stop temporalWorkflowServer; sleep 60; $DC start temporalWorkflowServer
for id in "${ids[@]}"; do for i in $(seq 300); do st=$(gql 'query($id:ID!){encodingWorkflowProgressForVideoAsset(videoAssetIdentifier:$id){status}}' "{\"id\":\"$id\"}" | jq -r '.data[]|.status'); [[ $st =~ READY|COMPLETED|FAILED ]] && break; sleep 2; done; [[ $st =~ READY|COMPLETED ]] && ok "CHA-002 $id" || ko "CHA-002 $id" "$st"; done
# CHA-004/005 telemetry exactly-once across broker / ClickHouse outage
python3 telemetry_exact.py 100000 & T=$!; sleep 5; $DC pause redpandaTelemetryStreamBroker; sleep 30; $DC unpause redpandaTelemetryStreamBroker; wait $T && ok CHA-004 || ko CHA-004
python3 telemetry_exact.py 100000 & T=$!; sleep 5; $DC stop clickhouseAnalyticsDataLake; sleep 60; $DC start clickhouseAnalyticsDataLake; wait $T && ok CHA-005 || ko CHA-005
# CHA-006 origin outage: PKG must fail fast, not hang
$DC stop minioOriginObjectStorage; t=$(curl -s -o /dev/null -m 15 -w '%{http_code} %{time_total}' "$PKG/package/${ASSET:-x}/0"); $DC start minioOriginObjectStorage; echo "CHA-006 $t (expect 5xx/cached 200 in <5s)"
# CHA-007 hung ffmpeg -> heartbeat timeout -> retry
pkill -STOP ffmpeg || true; sleep 120; pkill -KILL ffmpeg || true; echo "CHA-007: verify in Temporal UI that activity timed out on heartbeat and retried; asset must end READY"
summary
```

## Load
Use the existing scripts as the base (they hold the real request shapes):
```bash
k6 run load-testing/edge-packaging-and-manifest-service-http-load-test.js      # LOAD-001
k6 run load-testing/control-plane-api-gateway-graphql-load-test.js             # LOAD-002
```
Add to each an `-e PROFILE=` switch with these stage sets — load `[2m→N, 10m@N, 1m→0]`, stress `[N, 3N, 8N, 0]` 3 m each, spike `[1s→10N, 45s, 1s→0, 45s]×3`, soak `[0.3N for 24h]` — and thresholds from `docs/slo-sli-sla.md`. RT: `hey -z 5m -q 5000 "$RT/route/x?geo=US"`. QOE: `python3 qoe_ws_load.py ws://localhost:8000 10000`. AN: `python3 telemetry_exact.py 5000000`.

## Unit/contract tests to add
```go
// distributed-encoding-orchestrator/internal/ladder/property_test.go: for random (w,h,bitrate,fps): every rung w<=srcW && h<=srcH && even dims && |aspect-srcAspect|<1% && bitrates strictly decreasing
// .../internal/checkpoint/crash_test.go: write checkpoint, truncate file at each byte offset -> Load() returns previous valid checkpoint or error, never a half state
// edge-packaging/internal/fmp4/box_fuzz_test.go: FuzzParse(bytes) no panic; round-trip Build→Parse equality
// edge-packaging/internal/manifest/golden_test.go: fixtures -> golden .m3u8/.mpd; SSAI discontinuity sequence monotonic
// multi-cdn-edge-routing/internal/routing/property_test.go: never returns unhealthy when ≥1 healthy; weighted split within 2% over 100k draws; hysteresis: flapping input changes output ≤ once per window
```
```ts
// control-plane-api-gateway/test/workflow.replay.spec.ts — Temporal determinism (CHA-015)
import { Worker } from '@temporalio/worker';
it('replays recorded histories', async () => { for (const h of loadHistories('test/histories/*.json')) await Worker.runReplayHistory({ workflowsPath: require.resolve('../src/encoding-orchestration/workflows') }, h); });
// analytics-telemetry-platform/tests/consumer.poison.test.ts — a batch containing one invalid message commits offsets and routes it to DLQ; consumer keeps running
```
```python
# ingestion-inspection-service/tests/test_fixture_matrix.py — parametrize over scripts/fixtures/qa/*: probe() returns structured error for bad inputs within 10 s, never raises unhandled, never exceeds 1 GB RSS (resource.setrlimit in subprocess)
```

## Run order
unit (`go test -race`, `npm test`, `pytest`) → fixtures → smoke_e2e → negative → bad_media_matrix → telemetry_exact → loads → existing chaos scripts → chaos.sh → soak.
