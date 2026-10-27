# edge-packaging-and-manifest-service

The **Edge Routing & Packaging Engine**: simulates the edge-compute layer of a
hyperscale video platform that runs closest to the end user. It answers two
questions on every playback request:

1. *"What manifest should this specific device, on this specific network,
   with this DRM system and these ad breaks, receive right now?"* — personalized
   HLS/DASH manifest generation.
2. *"Give me this 2-second chunk, packaged into a container this player can
   decode."* — Just-In-Time (JIT) fMP4 packaging from separately-stored
   video/audio elementary-stream chunks.

Everything here is a real, structurally-correct implementation of the parts
that matter for those two questions (box-layout-correct fMP4 muxing, spec-shaped
HLS/DASH markup, deterministic DRM key derivation, SCTE-35-style ad splicing),
built against a small **mock content catalog** instead of a real encoding
pipeline or object store — see [Limitations](#limitations--whats-simulated).

## Table of contents

- [Architecture](#architecture)
- [fMP4 box structure implemented](#fmp4-box-structure-implemented)
- [HLS example output](#hls-example-output)
- [DASH example output](#dash-example-output)
- [SSAI timing model](#ssai-timing-model)
- [DRM multi-system key rotation scheme](#drm-multi-system-key-rotation-scheme)
- [Real AES-128-CTR media encryption](#real-aes-128-ctr-media-encryption)
- [QoE-driven manifest personalization](#qoe-driven-manifest-personalization)
- [QUIC/HTTP-3 edge termination](#quichttp-3-edge-termination)
- [Prometheus metrics](#prometheus-metrics)
- [Design patterns applied](#design-patterns-applied)
- [HTTP API](#http-api)
- [Latency targets](#latency-targets)
- [Running it](#running-it)
- [Test output](#test-output)
- [Limitations / what's simulated](#limitations--whats-simulated)

## Architecture

```
                         ┌──────────────────────────┐
   GET /manifest/{id} ──▶│       httpapi (net/http)  │◀── GET /package/{id}/{chunk}
                         └────────────┬──────────────┘
                                      │
              ┌───────────────────────┼───────────────────────┐
              ▼                       ▼                       ▼
      ┌───────────────┐     ┌─────────────────┐      ┌────────────────┐
      │  filtering     │     │     ssai         │      │     fmp4       │
      │ (Chain of      │     │ (ad-break        │      │ (ISO-BMFF box  │
      │  Responsibility│     │  timeline         │      │  writer /      │
      │  variant-ladder│     │  splicing)        │      │  JIT muxer)    │
      │  narrowing)    │     └─────────────────┘      └────────────────┘
      └───────┬────────┘              │
              │                       │
              ▼                       ▼
      ┌─────────────────────────────────────────┐        ┌──────────────┐
      │      manifest (Builder + Strategy)        │◀──────│     drm       │
      │  PersonalizedManifestBuilder ties          │       │ (Strategy:    │
      │  filtering → splicing → drm → hls/dash     │       │ Widevine/     │
      │  ManifestFormatGenerator together           │       │ FairPlay/     │
      └─────────────────────────────────────────┘        │ PlayReady)    │
                                                            └──────────────┘
                          ▲
                          │ (mock ladder / ad plan / chunk bytes)
                  ┌───────────────┐
                  │   catalog      │
                  │ (in-memory     │
                  │  mock content  │
                  │  store)        │
                  └───────────────┘
                          ▲
                  ┌───────────────┐
                  │    domain      │  (PlaybackContext, VariantLadder,
                  │ (shared types) │   AdBreakInsertionPoint, ...)
                  └───────────────┘
```

Package layout:

```
cmd/
  server/       - production HTTP server entrypoint
  gensample/     - dev utility that prints real generated manifests (used to produce this README's examples)
internal/
  domain/        - shared, dependency-free data model
  filtering/     - Chain of Responsibility device-capability/network/QoE variant-ladder narrowing
  ssai/          - SCTE-35-like ad-break cue splicing into a content timeline
  drm/           - Strategy DrmKeyProvider (Widevine/FairPlay/PlayReady mocks) + key rotation
                   + real AES-128-CTR content-key/IV derivation and media encryption
  fmp4/          - simplified-but-byte-correct ISO-BMFF box writer / JIT muxer
                   + mdat payload encryption wiring (internal/fmp4/encryption.go)
  manifest/      - Builder (PersonalizedManifestBuilder) + Strategy (HLS/DASH ManifestFormatGenerator)
  catalog/       - in-memory mock content catalog (variant ladder, ad plan, chunk bytes)
  httpapi/       - net/http handlers wiring it all together, plus /metrics
  metrics/       - Prometheus histograms/counters (manifest & JIT latency, per-DRM-system counts)
  quicedge/      - second QUIC/HTTP-3 listener serving the same handler as httpapi
```

Every request flows through the same pipeline regardless of output format:

1. **Filter** the asset's full `VariantLadder` down to what this
   `PlaybackContext`'s device/network/recent-QoE-signal can actually use
   (`internal/filtering`).
2. **Splice** ad breaks into each surviving rendition's chunk timeline
   (`internal/ssai`).
3. **Resolve** a DRM key provider strategy for the requested system, if any
   (`internal/drm`).
4. **Render** the result as HLS or DASH markup via the selected
   `ManifestFormatGenerator` strategy (`internal/manifest`).

JIT packaging is a separate, simpler flow: fetch video+audio chunk bytes
from the mock catalog, wrap them into an fMP4 byte stream (`internal/fmp4`),
and, when a DRM system is requested (`?drm=`), actually encrypt the packaged
chunk's `mdat` payload bytes in place (`internal/fmp4/encryption.go` +
`internal/drm/encryption.go`) under the same deterministically-derived
content key the manifest layer signals for that chunk's rotation epoch.

## fMP4 box structure implemented

```
ftyp                              (File Type box)
moov                               (Movie box — sent as part of every response
 ├─ mvhd                            for simplicity; a real player only needs
 ├─ trak (video, track_ID=1)        it once per rendition)
 │   ├─ tkhd
 │   └─ mdia
 │       ├─ mdhd
 │       └─ minf
 │           └─ stbl                (empty stub — see "simplified" below)
 ├─ trak (audio, track_ID=2)
 │   └─ ... (same shape)
 └─ mvex
     ├─ trex (track_ID=1)
     └─ trex (track_ID=2)

moof                               (Movie Fragment box — one per JIT-packaged chunk)
 ├─ mfhd
 ├─ traf (video)
 │   ├─ tfhd  (default-base-is-moof)
 │   ├─ tfdt  (64-bit base media decode time)
 │   └─ trun  (data_offset points into the following mdat)
 └─ traf (audio)
     └─ ... (same shape)

mdat                               (Media Data box — raw video bytes then raw audio bytes)
```

**Implemented, byte-correct** (verified by `internal/fmp4/fmp4_test.go`, which
asserts every box's 32-bit size field equals its true byte length, that
parent-box sizes equal the sum of their children, and that `trun`'s
`data_offset` correctly points at each track's sample bytes inside the
following `mdat`):

- `ftyp`, `moov` (`mvhd` + `trak`×N with `tkhd`/`mdia`/`mdhd`, `mvex`/`trex`)
- `moof` (`mfhd` + `traf`×N with `tfhd`/`tfdt`/`trun`)
- `mdat` with the real concatenated chunk payload bytes

**Simplified or omitted** (so this isn't mistaken for a spec-complete muxer):

- No `stbl` sample table with real `stsd`/`avcC`/`hvcC`/`esds` codec-init
  data — a real player needs this to configure its decoder; here it's an
  empty stub box.
- `trun` declares a single logical sample per chunk (no per-frame
  `sample_size`/`sample_duration`/`sample_flags` arrays).
- No `sidx`, `styp`, or CENC `senc`/`saiz`/`saio` encryption auxiliary-info
  boxes — DRM signaling in this service lives entirely in the manifest
  layer (`#EXT-X-KEY` / `<ContentProtection>`), not in the packaged bytes.
- No 64-bit "largesize" box headers (fine for ~2s chunks).
- No real bitstream parsing — video/audio bytes are opaque payload copied
  verbatim into `mdat`.

## HLS example output

Real output from `go run ./cmd/gensample` (also covered by
`internal/manifest/manifest_test.go`), for a HEVC+HDR-capable device on a
good network with Widevine DRM and one mid-roll ad break:

**Master playlist** (`GET /manifest/stranger-things-s5e1`):

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1",URI="https://license.edge-packaging.example/widevine/stranger-things-s5e1?kid=251a8c73cc9c4091a7e4e6c8bb02a654",KEYID=0x251a8c73cc9c4091a7e4e6c8bb02a654
#EXT-X-STREAM-INF:BANDWIDTH=700000,RESOLUTION=640x360,CODECS="avc1.640028,mp4a.40.2"
/manifest/stranger-things-s5e1/variant/v0-360p-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1200000,RESOLUTION=854x480,CODECS="avc1.640028,mp4a.40.2"
/manifest/stranger-things-s5e1/variant/v1-480p-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS="avc1.640028,mp4a.40.2"
/manifest/stranger-things-s5e1/variant/v2-720p-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L120.90,ec-3"
/manifest/stranger-things-s5e1/variant/v3-1080p-hevc.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,CODECS="hvc1.2.4.L120.90,ec-3"
/manifest/stranger-things-s5e1/variant/v4-1080p-hevc-hdr.m3u8
```

Note the AV1/HDR-at-2160p rung (`v5-2160p-av1-hdr`) is **absent** — this
device does not report AV1 support, so `filtering`'s codec-support stage
dropped it.

**Media playlist** for `v0-360p-h264` (`GET
/manifest/stranger-things-s5e1/variant/v0-360p-h264.m3u8`), with the
mid-roll ad break spliced in after chunk 9 and key rotation visible every 8
chunks:

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-MAP:URI="/package/stranger-things-s5e1/init/v0-360p-h264"
#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1",URI="https://license.edge-packaging.example/widevine/stranger-things-s5e1?kid=251a8c73cc9c4091a7e4e6c8bb02a654",KEYID=0x251a8c73cc9c4091a7e4e6c8bb02a654
#EXTINF:2.000,
/package/stranger-things-s5e1/0?rendition=v0-360p-h264
... (chunks 1-7) ...
#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",KEYFORMATVERSIONS="1",URI="https://license.edge-packaging.example/widevine/stranger-things-s5e1?kid=2df7861950eb5d29f6268d1172ab1277",KEYID=0x2df7861950eb5d29f6268d1172ab1277
#EXTINF:2.000,
/package/stranger-things-s5e1/8?rendition=v0-360p-h264
#EXTINF:2.000,
/package/stranger-things-s5e1/9?rendition=v0-360p-h264
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
/ad-creative/s3://edge-packaging-mock-bucket/ads/stranger-things-s5e1/creative-a.m4s
#EXTINF:2.000,
/ad-creative/s3://edge-packaging-mock-bucket/ads/stranger-things-s5e1/creative-b.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
/package/stranger-things-s5e1/10?rendition=v0-360p-h264
... (chunks 11-19, with a further #EXT-X-KEY rotation at chunk 16) ...
#EXT-X-ENDLIST
```

**Low-end device, poor network** (`GET
/manifest/stranger-things-s5e1?low_end=true&network=poor`) — narrowed to a
single conservative rung:

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES-CTR,KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed",...
#EXT-X-STREAM-INF:BANDWIDTH=700000,RESOLUTION=640x360,CODECS="avc1.640028,mp4a.40.2"
/manifest/stranger-things-s5e1/variant/v0-360p-h264.m3u8
```

## DASH example output

Same context, DASH Multi-Period SSAI model (`GET
/manifest/stranger-things-s5e1?format=dash`) — content Period, ad Period,
content Period again:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT44.000S">
  <Period id="content-0">
    <AdaptationSet mimeType="video/mp4" contentType="video" codecs="avc1.640028,mp4a.40.2">
      <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed" value="Widevine" cenc:default_KID="251a8c73-cc9c-4091-a7e4-e6c8bb02a654"><widevine:license_url>https://license.edge-packaging.example/widevine/stranger-things-s5e1</widevine:license_url></ContentProtection>
      <Representation id="v0-360p-h264" bandwidth="700000" width="640" height="360">
        <SegmentTemplate timescale="1000" media="$Number$.m4s" initialization="init.mp4" startNumber="0">
          <SegmentTimeline>
            <S d="2000" /> <!-- chunk 0 -->
            ... <!-- chunks 1-9 -->
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <!-- one AdaptationSet per surviving rendition (v1-480p-h264 ... v4-1080p-hevc-hdr) -->
  </Period>
  <Period id="ad-midroll-1">
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="ad-creative" bandwidth="0">
        <SegmentTemplate timescale="1000" media="$Number$.m4s" initialization="init.mp4" startNumber="0">
          <SegmentTimeline>
            <S d="2000" /> <!-- chunk .../creative-a.m4s -->
            <S d="2000" /> <!-- chunk .../creative-b.m4s -->
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
  <Period id="content-2">
    <!-- content resumes; note the ContentProtection cenc:default_KID has rotated
         to "2df78619-50eb-5d29-f626-8d1172ab1277" for this new rotation epoch -->
    ...
  </Period>
</MPD>
```

Full, untruncated output for both formats (and the intermediate low-end-device
variant) is reproducible with `go run ./cmd/gensample` — see
[Running it](#running-it).

## SSAI timing model

Each `domain.AdBreakInsertionPoint` carries a `CueTimeOffsetSeconds` — mirroring
an SCTE-35 `splice_insert()` event — expressed on the **unmodified content
timeline** (i.e. ignoring any ads already spliced in). `internal/ssai`
(`SpliceAdBreakSegmentsIntoManifestTimeline`) walks the content chunk list in
order, accumulating elapsed content duration, and inserts the ad break's
creative chunks immediately before the first content chunk whose start time
would be `>= CueTimeOffsetSeconds`. Consequences:

- **Ad breaks always land on a content-chunk boundary** — they never split a
  chunk, matching how real packagers require ad-insertion points aligned to
  segment boundaries.
- **Multiple ad breaks are sorted and interleaved in chronological order**,
  regardless of the order the caller supplied them in.
- **Post-roll**: an ad break whose cue point is at or beyond the end of the
  content timeline is appended after all content.
- Every `AdBreakInsertionPoint.TotalDurationSeconds` **must equal the sum**
  of its `AdCreativeChunks`' durations — the splicer returns an error
  otherwise, since a mismatch would desynchronize advertised segment timing
  from actual ad payload duration.
- HLS renders each ad break as content bracketed by two
  `#EXT-X-DISCONTINUITY` markers (the HLS-mandated signal for any
  encoding/timeline discontinuity). DASH renders each ad break as its own
  `<Period>`, the idiomatic DASH Multi-Period SSAI model.

Ordering and timing correctness are asserted directly in
`internal/ssai/ssai_test.go` (spliced-timeline start offsets, chronological
sorting of out-of-order ad breaks, post-roll placement, duration-mismatch
rejection) and end-to-end in `internal/manifest/manifest_test.go`
(`#EXT-X-DISCONTINUITY` bracketing + segment ordering in HLS,
`<Period>` ordering in DASH).

## DRM multi-system key rotation scheme

Content keys rotate every `RotationPeriodInSegments` (**N = 8**) segments. A
chunk at `ChunkSequenceIndex i` belongs to rotation epoch:

```
rotationIndex = i / RotationPeriodInSegments      (integer division)
```

The content key for a given `(assetIdentifier, rotationIndex)` pair is
derived **deterministically**:

```
contentKey = HMAC-SHA256(masterSecret, assetIdentifier + ":" + rotationIndex)[:16]
```

truncated to 16 bytes (128 bits). Determinism means any edge node can
independently recompute the identical key for the same `(asset, rotation
index)` pair without a round trip to a central key server — the entire point
of doing key derivation at the edge. A real deployment would source
`masterSecret` per-tenant from a KMS/HSM rather than hard-coding it, but the
derivation formula and rotation cadence stay the same.

`DrmKeyProvider` is a Strategy interface with three mock implementations
(`internal/drm/drm.go`), each attaching the format-correct signaling for its
system:

| System | HLS (`#EXT-X-KEY`) | DASH (`<ContentProtection>`) |
|---|---|---|
| Widevine | `METHOD=SAMPLE-AES-CTR`, `KEYFORMAT="urn:uuid:edef8ba9-..."` | `schemeIdUri="urn:uuid:edef8ba9-..."` + `cenc:default_KID` |
| FairPlay | `METHOD=SAMPLE-AES`, `KEYFORMAT="com.apple.streamingkeydelivery"`, `URI="skd://..."` | placeholder (FairPlay is HLS-only in practice) |
| PlayReady | `METHOD=SAMPLE-AES`, `KEYFORMAT="com.microsoft.playready"` | `schemeIdUri="urn:uuid:9a04f079-..."` + `cenc:default_KID` |

Per-format correctness and rotation-driven key-identifier changes are
asserted in `internal/drm/drm_test.go`.

## Real AES-128-CTR media encryption

Everything above (`#EXT-X-KEY` / `<ContentProtection>`) is manifest-layer
**signaling** — it tells a player which key/license server to use, but the
packaged media bytes themselves stay in the clear. `internal/drm/encryption.go`
and `internal/fmp4/encryption.go` close that gap with real encryption of the
packaged fMP4 `mdat` payload:

- **Cipher: AES-128 in CTR mode**, not CBC. CTR is a stream cipher (XOR
  against a keystream), so ciphertext length always equals plaintext length
  exactly. CBC requires block-size padding, which would change the `mdat`
  payload's byte length and invalidate the box's already-computed 32-bit
  size field and every `trun.data_offset` computed relative to it (see
  [fMP4 box structure implemented](#fmp4-box-structure-implemented)). This
  mirrors real-world practice: both HLS `SAMPLE-AES-CTR` and the CENC
  `'cenc'` scheme (as opposed to `'cbcs'`) standardize on AES-CTR for
  exactly this reason.
- **Key derivation**: reuses the exact same deterministic
  `HMAC-SHA256(masterSecret, assetIdentifier || ":" || rotationIndex)`
  scheme documented above for `ContentKey`, so the key a manifest signals
  for a given chunk's rotation epoch and the key that actually encrypted
  that chunk's bytes are the same key by construction — no separate
  key-management path to keep in sync.
- **IV derivation**: `HMAC-SHA256(masterSecret, assetIdentifier || ":" || rotationIndex || ":iv")[:16]`
  — the same construction with a distinct label suffix, so the derived key
  and IV are independent 128-bit outputs of the same derivation tree rather
  than the same bytes reused for two purposes. Both stay fully
  deterministic: any edge node can recompute the identical (key, IV) pair
  for a given (asset, rotation index) without any additional round trip.
- **Where it's applied**: `internal/fmp4/encryption.go`'s
  `EncryptMovieFragmentMediaData`/`DecryptMovieFragmentMediaData` locate the
  `mdat` box inside a packaged chunk's `moof`+`mdat` movie fragment (via the
  existing `WalkTopLevelBoxes` box walker) and transform only its payload
  bytes in place, leaving every other box byte-identical.
- **HTTP wiring**: `GET /package/{assetId}/{chunkId}?drm=widevine|fairplay|playready`
  now actually encrypts the response's `mdat` bytes under that chunk's
  rotation-epoch key (`?drm` unset/`none` preserves the prior clear-content
  behavior, so existing callers are unaffected).

**Round-trip correctness** — encrypt then decrypt recovers the original
bytes bit-for-bit — is asserted directly in
`internal/drm/encryption_test.go` (raw payload bytes) and
`internal/fmp4/encryption_test.go` (a full packaged chunk: package → encrypt
→ decrypt → reassemble → byte-identical to the original chunk), plus an
HTTP-level test in `internal/httpapi/httpapi_test.go` confirming
`?drm=widevine` produces a same-length, still box-valid, but different
payload than the clear-content response.

What is still **not** implemented (see
[Limitations](#limitations--whats-simulated)): CENC auxiliary-info boxes
(`senc`/`saiz`/`saio`) that would let a *real* player locate per-sample
encryption ranges and IVs inside the box stream itself, and a real PSSH
box / license server. This is genuine AES encryption of genuine media
bytes; it is not a spec-complete CENC/CBCS packaging pipeline.

## QoE-driven manifest personalization

`domain.PlaybackContext` gained an optional `RecentQoeSignal` field —
`RecentRebufferRatio` (fraction of recent playback time spent rebuffering)
and `MeasuredThroughputBitsPerSecond` (directly measured recent client
throughput) — and `internal/filtering` gained a new Chain-of-Responsibility
stage, `qoeDrivenVariantCeilingFilterStage`, run **last** in
`NewDefaultDeviceCapabilityFilterPipeline` (after the existing
network-quality-hint bitrate cap) so it can only ever further *tighten* the
offered ladder ceiling, never loosen what earlier stages already decided.

- A session is judged "struggling" when `RecentRebufferRatio >= 3%`
  (`domain.RecentQoeSignal.IndicatesStrugglingSession`) — 3% is a
  commonly-used real-world threshold above which viewers measurably start
  abandoning sessions.
- For a struggling session, the recommended bitrate ceiling
  (`RecommendedBitrateCeilingGivenNetworkQualityHint`) is the lesser of (a)
  the coarse `NetworkQualityHint`'s ceiling scaled down by 0.5×, and (b) 80%
  of `MeasuredThroughputBitsPerSecond`, if a nonzero measurement is present
  — a directly-measured throughput figure is trusted more than the coarse
  hint bucket when both are available.
- When `RecentQoeSignal` is `nil`, or the signal doesn't indicate a
  struggling session, the stage is a pure no-op.

**This is the hook point where `predictive-qoe-ml-service`'s output would
plug in.** In production, `RecentQoeSignal` would be populated either from
client-reported CMCD-style telemetry on the manifest request itself, or
from a side-channel lookup to `predictive-qoe-ml-service` keyed by
`RequestingSessionID` — a model predicting near-term rebuffer risk from
recent playback telemetry, rather than only reacting to network conditions
already captured by the coarse `NetworkQualityHint` bucket. Neither
integration exists in this service; `RecentQoeSignal` is accepted directly
as a `PlaybackContext` field precisely so the personalization hook itself
is real, tested, and pluggable without either integration existing yet.

The stage is tested in isolation (no-op with no/healthy signal,
ceiling-narrowing with a struggling signal, measured-throughput taking
precedence over the ratio-only downgrade) and through the full default
pipeline in `internal/filtering/filtering_test.go`.

## QUIC/HTTP-3 edge termination

Alongside the existing `net/http` (HTTP/1.1) listener, `cmd/server/main.go`
now also starts a second listener on `github.com/quic-go/quic-go/http3`,
serving the **identical** `http.ServeMux` (`internal/quicedge`). Both
transports expose the same routes/behavior; QUIC is UDP-based and cannot
share a TCP listen socket, so it binds a separate port
(`:8443` by default, vs. `:8080` for HTTP/1.1 — override with
`EDGE_PACKAGING_HTTP3_LISTEN_ADDR` / `EDGE_PACKAGING_LISTEN_ADDR`; set
`EDGE_PACKAGING_DISABLE_HTTP3=true` to disable it).

### Why QUIC matters for this service specifically

This service's job is serving many small, latency-sensitive objects per
playback session — a manifest, then dozens of ~2-second fMP4 chunks per
minute of playback, frequently over cellular/Wi-Fi with real packet loss:

- **Head-of-line blocking elimination.** Over HTTP/1.1/HTTP2, independent
  logical streams still share one TCP connection's in-order-delivery
  guarantee — one lost packet stalls *every* stream on that connection
  until retransmitted, so a lost packet from one chunk request can stall an
  unrelated concurrent request. QUIC multiplexes streams over UDP, where a
  lost packet only blocks the stream(s) whose data it carried.
- **Connection migration.** A QUIC connection is identified by a connection
  ID, not a client's (source IP, source port) 4-tuple. A viewer roaming
  from Wi-Fi to 5G gets a new IP mid-session; TCP always tears down and
  re-establishes (and re-handshakes TLS) on that change, stalling or
  failing in-flight requests at exactly the moment the network changes.
  QUIC connections survive the IP change with no new handshake.

### TLS: self-signed dev certificate

`internal/quicedge.GenerateSelfSignedDevelopmentTlsCertificate` generates a
fresh in-memory ECDSA P-256 self-signed certificate (valid for `localhost`
and `127.0.0.1`) at process start, purely so the HTTP/3 listener has
*something* to TLS-handshake with for local dev/test — QUIC mandates TLS
1.3, there is no "plaintext QUIC" option the way HTTP/1.1 can run without
TLS. **This is a development-only certificate**: it is not trusted by any
real client without explicitly disabling verification (e.g.
`InsecureSkipVerify`, as the smoke test does). A production deployment must
supply a CA-issued certificate (e.g. via ACME/Let's Encrypt) instead —
exactly as it would for a production TLS-terminated HTTP/1.1 listener.

### Congestion control: what's real, what's kernel-level, what's honestly out of scope

This is the part most write-ups get imprecise about, so to be exact:

1. **`sysctl net.ipv4.tcp_congestion_control=bbr` is a TCP-only, kernel-level
   setting.** BBR is implemented in the Linux kernel's TCP stack
   (`net/ipv4/tcp_bbr.c`) and selected per-socket or system-wide via that
   sysctl (or per-route via `ip route ... congestion bbr`). No Go
   application code can change which congestion-control algorithm the
   kernel's TCP stack uses. Running that sysctl on an edge node is a real,
   legitimate, and genuinely orthogonal operational step — it's just
   outside this codebase, and it applies only to this service's *existing*
   HTTP/1.1 (TCP) listener.
2. **That sysctl has zero effect on QUIC.** QUIC runs over UDP, and the
   Linux kernel has no congestion-control concept for UDP at all — UDP is
   unreliable/unordered/uncontrolled by design; the kernel does not pace or
   rate-limit it. Congestion control for a QUIC connection is implemented
   entirely in user-space QUIC library code (here, `quic-go`), not the
   kernel. So the honest answer to "does the BBR sysctl speed up this
   service's new QUIC listener" is a flat **no**, not "it depends."
3. **`quic-go` v0.61.0 (the version this service is built against) has no
   public API for swapping in a custom congestion-control algorithm** —
   verified directly against this module version: its congestion-control
   implementation lives in an unexported `internal/congestion` package, and
   neither `quic.Config` nor any exported interface accepts a caller-supplied
   algorithm. A deployment that wanted BBR-style behavior for QUIC/UDP
   traffic would have to either (a) fork/patch `quic-go`'s internal
   congestion package to add a BBR-like algorithm satisfying its internal
   `SendAlgorithm` interface (the approach Google's QUIC/BoringSSL and early
   `quiche` implementations took to ship BBRv1/BBRv2 for QUIC), or (b) adopt
   a different QUIC implementation that does expose pluggable congestion
   control — neither of which this service attempts, since vendoring and
   maintaining a fork of a third-party transport library is out of scope
   for an edge-packaging demo. `internal/quicedge.NewHttp3EdgeTerminationServer`
   does explicitly construct and pass a `*quic.Config` (rather than leaving
   it `nil`) specifically so the exact struct such an override would attach
   to is visible in this codebase as the integration point, once `quic-go`
   (or a replacement transport) exposes one.

### Testing

`internal/quicedge/quicedge_test.go` is a real smoke test, not a mock: it
generates the dev certificate, starts an actual `*http3.Server` on an
ephemeral UDP port, and issues a real HTTP/3 request against it with
`http3.Transport` (a real QUIC client), asserting the response comes back
correctly. This was verified to actually build and pass in this
environment — see [Test output](#test-output).

## Prometheus metrics

`GET /metrics` (wired via `github.com/prometheus/client_golang/prometheus/promhttp`)
exposes, alongside the Go/process default collectors:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `edge_packaging_manifest_generation_duration_seconds` | Histogram | `output_format` (`hls`/`dash`) | Wall-clock latency of the personalized manifest pipeline (filter→splice→DRM-resolve→render), per the [Latency targets](#latency-targets) this service tracks. |
| `edge_packaging_jit_packaging_duration_seconds` | Histogram | — | Wall-clock latency of the JIT fMP4 packaging path (object-storage fetch + box construction [+ encryption when `?drm=` is set]). |
| `edge_packaging_drm_requests_total` | Counter | `drm_system` (`none`/`widevine`/`fairplay`/`playready`) | Request volume per requested DRM system, across both the manifest and packaging endpoints. |

`internal/metrics/metrics.go` registers these against Prometheus's default
registry via `promauto`; `internal/httpapi/httpapi.go` observes them at the
end of the manifest and packaging handlers. Asserted in
`internal/httpapi/httpapi_test.go` (`TestMetricsEndpointExposesPrometheusOutput`):
drives one manifest and one packaging request, then verifies `/metrics`
output contains all three metric names.

## Design patterns applied

- **Strategy** — `drm.DrmKeyProvider` (Widevine/FairPlay/PlayReady mocks,
  selected by `drm.NewDrmKeyProviderForSystem`); `manifest.ManifestFormatGenerator`
  (HLS/DASH generators, selected by `ManifestOutputFormat` in
  `manifestFormatGeneratorsByOutputFormat`).
- **Builder** — `manifest.PersonalizedManifestBuilder` incrementally
  configures a variant ladder, output format, and filter pipeline before a
  single `Build()` call runs filter→splice→drm→render exactly once.
- **Chain of Responsibility** — `filtering.DeviceCapabilityFilterPipeline`:
  an ordered list of `VariantLadderFilterStage` links (low-end-device
  simplification → video codec → audio codec → HDR → resolution cap →
  network-quality bitrate cap → QoE-driven ceiling downgrade), each
  narrowing the surviving rendition set and handing it to the next stage.
  See [QoE-driven manifest personalization](#qoe-driven-manifest-personalization)
  for the newest link.

## HTTP API

| Endpoint | Description |
|---|---|
| `GET /manifest/{assetId}` | Personalized manifest. Query params: `format=hls\|dash`, `low_end`, `hevc`, `av1`, `hdr`, `eac3` (all `true`/`false`), `max_height=<px>`, `network=poor\|moderate\|good\|excellent`, `drm=none\|widevine\|fairplay\|playready`, `ads=true\|false`. |
| `GET /manifest/{assetId}/variant/{renditionId}.m3u8` | Single-rendition HLS media playlist (what a player fetches after resolving a master-playlist variant URI). Same query params. |
| `GET /package/{assetId}/{chunkId}` | JIT-packaged fMP4 chunk. Query params: `rendition=<renditionId>` (defaults to `v1-480p-h264`), `drm=none\|widevine\|fairplay\|playready` (defaults to `none`; when set, actually AES-128-CTR-encrypts the response's `mdat` payload — see [Real AES-128-CTR media encryption](#real-aes-128-ctr-media-encryption)). |
| `GET /healthz` | Liveness probe. |
| `GET /metrics` | Prometheus exposition format — see [Prometheus metrics](#prometheus-metrics). |

Both the HTTP/1.1 listener above and a second QUIC/HTTP-3 listener (default
`:8443`, self-signed dev TLS cert) serve this exact same set of routes —
see [QUIC/HTTP-3 edge termination](#quichttp-3-edge-termination).

## Latency targets

- **Personalized manifest generation: p99 < 15ms at the edge.**
  In-process benchmark (`internal/manifest`, Apple M4, `go test -bench`):
  HLS master-playlist generation (filter + splice + render) averages
  **~6.5µs/op**, DASH **~17.8µs/op** — three orders of magnitude under
  target, leaving ample headroom for real network/serialization overhead
  once deployed behind an actual edge POP.
- **JIT chunk packaging: p99 < 30ms for a 2-second chunk.**
  In-process benchmark (`internal/httpapi`, full HTTP handler path
  including mock object-storage fetch + fMP4 box construction) averages
  **~69µs/op** — again far under target; the real-world bottleneck this
  target guards against is object-storage fetch latency for the two
  elementary-stream chunks, not the muxing logic itself.

Reproduce with:

```
go test ./internal/manifest/... ./internal/httpapi/... -bench=. -benchmem -run='^$'
```

These are local, in-process, no-network measurements — they validate that
the packaging/generation *logic itself* is not the bottleneck; real edge p99
figures depend on network RTT, object-storage fetch latency, and CDN/POP
placement, none of which this simulation includes.

## Running it

```
go build ./...
go vet ./...
go test ./... -race

# start the server (HTTP/1.1 default :8080, QUIC/HTTP-3 default :8443)
go run ./cmd/server
EDGE_PACKAGING_DISABLE_HTTP3=true go run ./cmd/server   # HTTP/1.1 only

# reproduce this README's example output
go run ./cmd/gensample

curl 'http://localhost:8080/manifest/asset-1'
curl 'http://localhost:8080/manifest/asset-1?format=dash&drm=fairplay&low_end=true'
curl -o chunk0.mp4 'http://localhost:8080/package/asset-1/0?rendition=v1-480p-h264'
curl -o chunk0-encrypted.mp4 'http://localhost:8080/package/asset-1/0?rendition=v1-480p-h264&drm=widevine'
curl 'http://localhost:8080/metrics'

# HTTP/3 (self-signed dev cert, so TLS verification must be disabled):
curl -k --http3-only 'https://localhost:8443/healthz'
```

Environment variables: `EDGE_PACKAGING_LISTEN_ADDR` (HTTP/1.1, default
`:8080`), `EDGE_PACKAGING_HTTP3_LISTEN_ADDR` (QUIC/HTTP-3, default `:8443`),
`EDGE_PACKAGING_DISABLE_HTTP3=true` (skip starting the HTTP/3 listener
entirely).

## Test output

`go test ./... -race` (all packages, race detector on):

```
?   	.../cmd/gensample	[no test files]
?   	.../cmd/server	[no test files]
?   	.../internal/catalog	[no test files]
?   	.../internal/domain	[no test files]
ok  	.../internal/drm	1.653s
ok  	.../internal/filtering	1.364s
ok  	.../internal/fmp4	1.906s
ok  	.../internal/httpapi	2.536s
ok  	.../internal/manifest	1.863s
?   	.../internal/metrics	[no test files]
ok  	.../internal/quicedge	1.621s
ok  	.../internal/ssai	2.948s
```

55 test functions/subtests across 8 test-bearing packages, all passing —
39 from the original implementation plus 16 new ones added for this round
of capabilities:

- `internal/fmp4` — box byte-layout tests: size-field correctness, parent/child
  nesting, `trun.data_offset` correctness, malformed-input rejection; plus
  new mdat-encryption round-trip tests (`encryption_test.go`).
- `internal/filtering` — low-end-device track removal (the headline
  requirement), per-stage codec/HDR/resolution/network/**QoE** filtering,
  guaranteed-non-empty fallback.
- `internal/drm` — deterministic key derivation, rotation-epoch cadence,
  per-format (HLS/DASH) signaling correctness for all three DRM systems;
  plus new AES-128-CTR key/IV derivation and encrypt/decrypt round-trip
  tests (`encryption_test.go`).
- `internal/ssai` — ad-break ordering/timing correctness, multi-break
  chronological sorting, post-roll placement, duration-mismatch rejection.
- `internal/manifest` — HLS/DASH structural snapshot tests, end-to-end SSAI
  splicing in rendered output, DRM signaling presence/absence, low-end vs.
  capable-device manifest size comparison.
- `internal/httpapi` — end-to-end HTTP handler tests including the full
  master-playlist → variant-playlist discovery chain, fMP4 payload
  validity, **`?drm=` mdat encryption**, and the **`/metrics`** endpoint.
- `internal/quicedge` — **new**: self-signed dev certificate validity, and
  a real end-to-end HTTP/3 smoke test (starts an actual `*http3.Server`,
  issues a real QUIC request against it with `http3.Transport`).

## Limitations / what's simulated

- **No real encoder/object storage.** `internal/catalog` synthesizes a fixed
  six-rung variant ladder and deterministic filler bytes for every asset ID;
  there is no actual video/audio bitstream anywhere in this service.
- **fMP4 muxing is structurally correct but not player-playable** — see
  [fMP4 box structure implemented](#fmp4-box-structure-implemented) for the
  precise implemented/omitted box list (notably: no real `stsd` codec-init
  data, no per-sample `trun` tables).
- **DRM manifest-signaling is mocked; mdat encryption is real.** Key/IV
  derivation is real HMAC-SHA256 math with a documented, deterministic
  scheme, and `?drm=` genuinely AES-128-CTR-encrypts the packaged `mdat`
  bytes (see [Real AES-128-CTR media encryption](#real-aes-128-ctr-media-encryption)) —
  but there is still no real license server, no real PSSH box construction,
  no CENC `senc`/`saiz`/`saio` auxiliary-info boxes a real player would need
  to locate per-sample encryption ranges, and the "master secret" is a
  hard-coded placeholder rather than a KMS-sourced per-tenant secret.
- **RFC 6381 codec strings are representative, not fully spec-accurate**
  (e.g. a fixed `avc1.640028` rather than deriving profile/level from the
  actual encode).
- **No CDN/edge-POP simulation** — latency numbers above are in-process
  logic-only benchmarks, not measurements of a deployed edge node.
- **DASH ad Periods are not personalized per rendition** — ad creative in
  this simulation is a single, un-laddered `Representation`, unlike the
  content Periods which carry the full personalized rendition set.
- **QoE signal has no real producer.** `RecentQoeSignal` is a real,
  tested filtering-pipeline hook, but nothing in this service populates it
  from real client telemetry or calls `predictive-qoe-ml-service`; it must
  be set by the caller (or wired up by a future integration) to have any
  effect.
- **QUIC/HTTP-3 TLS is a self-signed dev certificate.** Real clients won't
  trust it without disabling verification; production needs a CA-issued
  certificate. See [QUIC/HTTP-3 edge termination](#quichttp-3-edge-termination)
  for exactly what is/isn't true about BBR congestion control here — short
  version: the `tcp_congestion_control=bbr` sysctl is real but TCP-only and
  has no effect on this service's QUIC/UDP traffic, and `quic-go` v0.61.0
  has no public pluggable-congestion-control API to plug a BBR-for-QUIC
  implementation into.
- **Prometheus metrics cover three specific signals** (manifest latency,
  JIT latency, per-DRM-system request counts) — not a full RED/USE-method
  instrumentation of every code path (no per-endpoint error-rate counters,
  no in-flight-request gauges, etc.).
