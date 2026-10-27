/**
 * edge-packaging-and-manifest-service-http-load-test.js
 *
 * k6 load test against services/edge-packaging-and-manifest-service's HTTP
 * API (default listen address ":8080", per
 * services/edge-packaging-and-manifest-service/cmd/server/main.go's
 * `defaultListenAddress`, overridable via `EDGE_PACKAGING_LISTEN_ADDR`).
 *
 * Exercises the two endpoints named in the task brief, using the exact
 * route patterns and query parameters registered in
 * services/edge-packaging-and-manifest-service/internal/httpapi/httpapi.go:
 *
 *   GET /manifest/{assetId}          - personalized HLS/DASH manifest generation
 *   GET /package/{assetId}/{chunkId} - JIT fMP4/ISO-BMFF chunk packaging
 *
 * Query parameters on /manifest/{assetId} mirror
 * parsePlaybackContextFromRequest's documented set: low_end, hevc, av1,
 * hdr, eac3, max_height, network, drm, format, ads.
 *
 * Thresholds are taken directly from docs/slo-sli-sla.md §4:
 *   - Personalized manifest generation: p99 < 15ms
 *   - JIT chunk packaging (2s chunk):   p99 < 30ms
 *   - Edge availability target: 99.99% (error rate assertions below are
 *     intentionally looser than that — see README's honesty note)
 *
 * Uses the classic single-`default`-function k6 pattern (rather than
 * multiple named `scenarios`) so this script can be driven either by its
 * own `options.stages` ramp profile below, or overridden ad hoc via
 * `k6 run --vus <n> --duration <t>` for a quick syntax/smoke check.
 *
 * Run (full ramp profile, thresholds enforced):
 *   k6 run load-testing/edge-packaging-and-manifest-service-http-load-test.js
 *
 * Quick syntax/connectivity smoke check:
 *   k6 run --vus 1 --duration 1s --summary-mode=disabled load-testing/edge-packaging-and-manifest-service-http-load-test.js
 *
 * Point at a different host/port:
 *   k6 run -e EDGE_PACKAGING_BASE_URL=http://localhost:8080 load-testing/edge-packaging-and-manifest-service-http-load-test.js
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const edgePackagingBaseUrl = __ENV.EDGE_PACKAGING_BASE_URL || 'http://localhost:8080';

const manifestGenerationLatencyTrend = new Trend('manifest_generation_duration_ms', true);
const jitPackagingLatencyTrend = new Trend('jit_packaging_duration_ms', true);

export const options = {
  stages: [
    { duration: '20s', target: 50 },    // ramp up — this plane runs on every playback request
    { duration: '2m', target: 200 },    // sustained high-QPS load
    { duration: '30s', target: 400 },   // spike, simulating a release-day traffic burst
    { duration: '20s', target: 0 },     // ramp down
  ],
  thresholds: {
    // docs/slo-sli-sla.md §4 — Personalized manifest generation p99 < 15ms
    manifest_generation_duration_ms: ['p(99)<15'],
    'http_req_duration{operation:generateManifest}': ['p(99)<15'],
    // docs/slo-sli-sla.md §4 — JIT chunk packaging (2s chunk) p99 < 30ms
    jit_packaging_duration_ms: ['p(99)<30'],
    'http_req_duration{operation:jitPackageChunk}': ['p(99)<30'],
    // Edge availability target is 99.99% — expressed here as a max error
    // rate; tighten to 0.0001 once run against real infra with real
    // volume (see README).
    http_req_failed: ['rate<0.05'],
  },
};

const SAMPLE_ASSET_IDENTIFIERS = [
  'asset-stranger-things-s05e01',
  'asset-the-crown-s06e01',
  'asset-our-planet-ii-e03',
  'asset-formula-1-drive-to-survive-s07e02',
];

const NETWORK_QUALITY_HINTS = ['poor', 'moderate', 'good', 'excellent'];
const DRM_SYSTEMS = ['widevine', 'fairplay', 'playready'];
const MANIFEST_FORMATS = ['hls', 'dash'];

function pickRandom(list) {
  return list[Math.floor(Math.random() * list.length)];
}

function randomChunkId() {
  // 2-second chunks per docs/slo-sli-sla.md's "2-second chunk" framing —
  // simulate a client requesting an arbitrary chunk within a ~1-hour asset.
  const chunkIndex = Math.floor(Math.random() * 1800);
  return `chunk-${chunkIndex}`;
}

/** Flow 1: GET /manifest/{assetId} — personalized manifest generation. */
function runPersonalizedManifestRequest() {
  const assetId = pickRandom(SAMPLE_ASSET_IDENTIFIERS);
  const params = {
    low_end: Math.random() < 0.15 ? 'true' : 'false',
    hevc: 'true',
    av1: Math.random() < 0.2 ? 'true' : 'false',
    hdr: 'true',
    eac3: 'true',
    max_height: pickRandom(['720', '1080', '2160']),
    network: pickRandom(NETWORK_QUALITY_HINTS),
    drm: pickRandom(DRM_SYSTEMS),
    format: pickRandom(MANIFEST_FORMATS),
    ads: 'true',
  };
  const query = Object.entries(params)
    .map(([key, value]) => `${key}=${value}`)
    .join('&');

  const response = http.get(`${edgePackagingBaseUrl}/manifest/${assetId}?${query}`, {
    headers: { 'X-Session-Id': `load-test-session-${__VU}-${__ITER}` },
    tags: { operation: 'generateManifest' },
  });
  manifestGenerationLatencyTrend.add(response.timings.duration);

  check(response, {
    'manifest generation: HTTP 200': (r) => r.status === 200,
    'manifest generation: non-empty body': (r) => r.body && r.body.length > 0,
    'manifest generation: no transport-level error': (r) => r.status !== 0,
  });
}

/** Flow 2: GET /package/{assetId}/{chunkId} — JIT fMP4/ISO-BMFF packaging. */
function runJitChunkPackagingRequest() {
  const assetId = pickRandom(SAMPLE_ASSET_IDENTIFIERS);
  const chunkId = randomChunkId();

  const response = http.get(`${edgePackagingBaseUrl}/package/${assetId}/${chunkId}`, {
    headers: { 'X-Session-Id': `load-test-session-${__VU}-${__ITER}` },
    tags: { operation: 'jitPackageChunk' },
  });
  jitPackagingLatencyTrend.add(response.timings.duration);

  check(response, {
    'JIT packaging: HTTP 200': (r) => r.status === 200,
    'JIT packaging: non-empty body (fMP4 bytes)': (r) => r.body && r.body.length > 0,
    'JIT packaging: no transport-level error': (r) => r.status !== 0,
  });
}

export default function edgePackagingLoadTestIteration() {
  runPersonalizedManifestRequest();
  sleep(0.3);
  runJitChunkPackagingRequest();
  sleep(0.3);
}
