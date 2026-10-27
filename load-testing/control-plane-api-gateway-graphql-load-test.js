/**
 * control-plane-api-gateway-graphql-load-test.js
 *
 * k6 load test against services/control-plane-api-gateway's GraphQL BFF
 * (default port 4000, per services/control-plane-api-gateway/src/config/
 * applicationConfigurationService.ts's HTTP_SERVER_LISTEN_PORT default).
 *
 * Exercises the two flows called out in the task brief on every iteration:
 *   1. Asset lookup      -> `videoAsset(videoAssetIdentifier: ...)` query
 *   2. Session creation   -> `createPlaybackSession(input: ...)` mutation
 *
 * Both operation names, field names, and input-type shapes are copied
 * verbatim from the real resolvers/DTOs:
 *   - services/control-plane-api-gateway/src/asset-registry/graphql/assetRegistryResolver.ts
 *   - services/control-plane-api-gateway/src/playback-session/graphql/playbackSessionResolver.ts
 *   - services/control-plane-api-gateway/src/playback-session/dto/createPlaybackSessionInputType.ts
 *
 * `createPlaybackSession` is guarded by `JsonWebTokenAuthenticationGuard`
 * (see playbackSessionResolver.ts) — this script sends a `Bearer` token via
 * the `GRAPHQL_LOAD_TEST_BEARER_TOKEN` env var so it can be pointed at a
 * real deployment with a real signer; against an un-configured local
 * docker-compose instance the mutation will legitimately 401, which is
 * still a useful signal (see load-testing/README.md's "how to interpret
 * results" section) — the asset-lookup query has no such requirement and
 * exercises the full resolver -> service -> Redis cache-aside path
 * regardless.
 *
 * Thresholds are taken directly from docs/slo-sli-sla.md §1:
 *   - GraphQL asset lookup (Redis cache hit):     p50 8ms / p95 25ms / p99 50ms, SLA 99.9% < 100ms
 *   - Playback session creation:                  p50 40ms / p95 90ms / p99 120ms, SLA 99.9% < 250ms
 *
 * Uses the classic single-`default`-function k6 pattern (rather than
 * multiple named `scenarios`) specifically so this script can be driven
 * either by its own `options.stages` ramp profile below, or overridden
 * ad hoc via `k6 run --vus <n> --duration <t>` for a quick syntax/smoke
 * check (see load-testing/README.md for exactly what was verified this
 * way in this environment).
 *
 * Run (full ramp profile, thresholds enforced):
 *   k6 run load-testing/control-plane-api-gateway-graphql-load-test.js
 *
 * Quick syntax/connectivity smoke check:
 *   k6 run --vus 1 --duration 1s --summary-mode=disabled load-testing/control-plane-api-gateway-graphql-load-test.js
 *
 * Point at a different host/port:
 *   k6 run -e GRAPHQL_ENDPOINT_BASE_URL=http://localhost:4000 load-testing/control-plane-api-gateway-graphql-load-test.js
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const graphqlEndpointBaseUrl = __ENV.GRAPHQL_ENDPOINT_BASE_URL || 'http://localhost:4000';
const graphqlEndpointUrl = `${graphqlEndpointBaseUrl}/graphql`;
const bearerToken = __ENV.GRAPHQL_LOAD_TEST_BEARER_TOKEN || '';

// Separate latency trends per operation so each can be asserted against
// its own SLO target independently (a single blended p95 across both
// operations would hide which one is regressing).
const assetLookupLatencyTrend = new Trend('asset_lookup_duration_ms', true);
const sessionCreationLatencyTrend = new Trend('session_creation_duration_ms', true);

export const options = {
  stages: [
    { duration: '30s', target: 20 },   // ramp up
    { duration: '2m', target: 50 },    // sustained load — cache-hit steady state
    { duration: '30s', target: 100 },  // spike
    { duration: '30s', target: 0 },    // ramp down
  ],
  thresholds: {
    // docs/slo-sli-sla.md §1 — GraphQL asset lookup (Redis cache hit)
    asset_lookup_duration_ms: ['p(50)<8', 'p(95)<25', 'p(99)<50'],
    'http_req_duration{operation:videoAsset}': ['p(95)<25'],
    // docs/slo-sli-sla.md §1 — Playback session creation
    session_creation_duration_ms: ['p(50)<40', 'p(95)<90', 'p(99)<120'],
    'http_req_duration{operation:createPlaybackSession}': ['p(95)<90'],
    // SLA-level: 99.9% success target overall; kept looser here on purpose
    // during authoring/dry-run against a non-live target (see README).
    http_req_failed: ['rate<0.05'],
  },
};

// A handful of asset identifiers to look up — the control-plane-api-gateway
// asset registry is not seeded by this script, so a real run against a live
// instance should replace these with ids returned by
// `registerRawVideoAsset` mutations run ahead of time (see README).
const SAMPLE_VIDEO_ASSET_IDENTIFIERS = [
  'asset-stranger-things-s05e01',
  'asset-the-crown-s06e01',
  'asset-our-planet-ii-e03',
  'asset-formula-1-drive-to-survive-s07e02',
];

const SAMPLE_DEVICE_PLATFORM_FAMILIES = ['ios', 'android', 'web', 'smarttv', 'xbox'];
const SAMPLE_NETWORK_CONDITION_HINTS = ['CELLULAR_LOW', 'CELLULAR_HIGH', 'WIFI', 'WIRED_ETHERNET'];

function pickRandom(list) {
  return list[Math.floor(Math.random() * list.length)];
}

function graphqlRequest(query, variables, operationTag) {
  const payload = JSON.stringify({ query, variables });
  const headers = { 'Content-Type': 'application/json' };
  if (bearerToken) {
    headers['Authorization'] = `Bearer ${bearerToken}`;
  }
  return http.post(graphqlEndpointUrl, payload, {
    headers,
    tags: { operation: operationTag },
  });
}

/** Flow 1: asset lookup — `videoAsset(videoAssetIdentifier: ID!)`. */
function runAssetLookupQuery() {
  const videoAssetIdentifier = pickRandom(SAMPLE_VIDEO_ASSET_IDENTIFIERS);
  const query = `
    query VideoAssetLookup($videoAssetIdentifier: ID!) {
      videoAsset(videoAssetIdentifier: $videoAssetIdentifier) {
        id
        displayTitle
        sourceMezzanineStorageUri
      }
    }
  `;

  const response = graphqlRequest(query, { videoAssetIdentifier }, 'videoAsset');
  assetLookupLatencyTrend.add(response.timings.duration);

  check(response, {
    'asset lookup: HTTP 200': (r) => r.status === 200,
    'asset lookup: no transport-level error': (r) => r.status !== 0,
    'asset lookup: well-formed GraphQL response body': (r) => {
      try {
        const body = JSON.parse(r.body);
        return body.data !== undefined || body.errors !== undefined;
      } catch (parseError) {
        return false;
      }
    },
  });
}

/** Flow 2: session creation — `createPlaybackSession(input: CreatePlaybackSessionInput!)`. */
function runCreatePlaybackSessionMutation() {
  const mutation = `
    mutation CreatePlaybackSession($input: CreatePlaybackSessionInput!) {
      createPlaybackSession(input: $input) {
        id
      }
    }
  `;

  const variables = {
    input: {
      videoAssetIdentifier: pickRandom(SAMPLE_VIDEO_ASSET_IDENTIFIERS),
      devicePlatformFamily: pickRandom(SAMPLE_DEVICE_PLATFORM_FAMILIES),
      networkConditionHint: pickRandom(SAMPLE_NETWORK_CONDITION_HINTS),
      clientSupportedDrmSystemIdentifiers: ['widevine', 'fairplay'],
    },
  };

  const response = graphqlRequest(mutation, variables, 'createPlaybackSession');
  sessionCreationLatencyTrend.add(response.timings.duration);

  check(response, {
    // 401 is an expected, non-crash outcome against an un-authenticated
    // local run (JsonWebTokenAuthenticationGuard) — see README for how to
    // supply a real bearer token to exercise the full mutation path.
    'session creation: HTTP 200 or 401 (auth-guard) — not a 5xx crash': (r) =>
      r.status === 200 || r.status === 401,
    'session creation: no transport-level error': (r) => r.status !== 0,
  });
}

export default function graphqlLoadTestIteration() {
  runAssetLookupQuery();
  sleep(0.5);
  runCreatePlaybackSessionMutation();
  sleep(0.5);
}
