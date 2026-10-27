# Ingestion & Inspection Plane

Pre-transcode deep-analysis microservice for raw mezzanine video assets on
a hyperscale video processing/streaming platform. Given a source file, it
produces a **structural map** JSON artifact describing:

- Ordered scene-cut boundaries (with per-scene motion/edge complexity score)
- Black-frame / fade-to-black time ranges
- Color space and HDR metadata (BT.709 vs. BT.2020, HDR10 / HDR10+ /
  Dolby Vision / HLG heuristic detection)
- An approximate integrated loudness (LUFS) pass, EBU R128-inspired

Downstream, a Go orchestrator uses this map to parameterize the transcode
fleet (shot-aware GOP placement, correct HDR tone-mapping pipeline
selection, loudness normalization targets) and to quarantine obviously
broken ingests before they consume expensive encode capacity.

Beyond the core structural map, six extended, opt-in capabilities are
available (see [Extended capabilities](#extended-capabilities)): rule-
based shot-type classification (close-up/wide/dialogue/action),
perceptual-hash near-duplicate-asset detection, subtitle/caption
extraction with forced-narrative detection, thumbnail/sprite-sheet
generation for player scrubbing previews, ffmpeg `loudnorm`-based
loudness remediation (not just analysis), and Prometheus metrics.

---

## Table of contents

1. [Architecture & design patterns](#architecture--design-patterns)
2. [Algorithms](#algorithms)
3. [Extended capabilities](#extended-capabilities)
4. [Naming convention](#naming-convention)
5. [Project layout](#project-layout)
6. [Setup](#setup)
7. [Running it](#running-it)
8. [Testing](#testing)
9. [Performance targets & the sampling tradeoff](#performance-targets--the-sampling-tradeoff)
10. [Known limitations](#known-limitations)

---

## Architecture & design patterns

```
                 ┌────────────────────────────────────────────────────┐
                 │                InspectionPipeline                   │
                 │        (Chain of Responsibility / Pipeline)          │
                 │                                                      │
 sampled frames  │  ┌───────────────┐  ┌────────────────┐  ┌─────────┐ │
 + PCM audio ───▶│─▶│ BlackFrame    │─▶│ SceneCut        │─▶│ ColorSpace│─▶ Loudness ──▶ StructuralMap
                 │  │ DetectionStage│  │ DetectionStage  │  │Extraction│ │ AnalysisStage
                 │  └───────────────┘  └────────┬────────┘  └─────────┘ │
                 └────────────────────────────────┼─────────────────────┘
                                                    │ drives (Strategy pattern)
                                                    ▼
                                     ┌──────────────────────────────┐
                                     │   SceneDetectionStrategy      │
                                     │  ┌──────────────────────┐    │
                                     │  │ HistogramDifference    │   │
                                     │  │ Strategy (fast)         │  │
                                     │  ├──────────────────────┤    │
                                     │  │ StructuralSimilarity    │  │
                                     │  │ (SSIM) Strategy (accurate)│ │
                                     │  └──────────────────────┘    │
                                     └──────────────────────────────┘
```

- **Strategy pattern** (`strategies/scene_detection.py`): scene-cut
  detection is pluggable behind the `SceneDetectionStrategy` interface.
  Two concrete strategies ship today —
  `HistogramDifferenceSceneDetectionStrategy` (fast, coarse) and
  `StructuralSimilaritySceneDetectionStrategy` (SSIM, slower, more
  discriminative). `SceneCutDetectionStage` is written against the
  interface only, so a third algorithm (e.g. a learned shot-boundary
  model) can be added without touching the pipeline.

- **Chain of Responsibility / Pipeline** (`pipeline/stages.py`,
  `pipeline/pipeline.py`): each `InspectionStage` does one job, mutates a
  shared `InspectionContext`, and forwards to the next stage.
  `InspectionPipeline` wires the required order — **black-frame
  detection → scene-cut detection → color-space extraction → loudness
  analysis** — and exposes a single `run()` entry point.

- **Dataclass domain model** (`domain/models.py`): `StructuralMap`,
  `SceneBoundary`, `BlackFrameRange`, `ColorSpaceProfile`,
  `LoudnessAnalysisResult`, `VideoStreamProfile`, `AudioStreamProfile` are
  plain, frozen-where-sensible `@dataclasses.dataclass` types with zero
  I/O dependencies, so they can be built directly in tests without a real
  video file, `ffprobe`, or `cv2`.

- **Adapters** (`adapters/`): all I/O boundaries — the `ffprobe`
  subprocess client, the OpenCV-based frame sampler, the `ffmpeg`-based
  PCM audio extractor — live here, isolated from the pure computation in
  `strategies/` and `pipeline/stages.py`'s module-level helper functions.

- **Entry points** (`cli/main.py`, `api/app.py`): both are thin
  marshalling layers around `pipeline.pipeline.create_inspection_context_from_source_file`
  and `InspectionPipeline`, guaranteeing the CLI and the HTTP service
  produce identical results for identical inputs.

## Algorithms

### Scene-cut detection

Two interchangeable per-frame-pair "dissimilarity" scores, both mapped
into a common `[0, 1]` range (0 = identical, 1 = maximally different):

- **Histogram difference.** Per sampled frame, build a 256-bin grayscale
  luma histogram (`cv2.calcHist`), then measure Pearson-correlation-style
  similarity between consecutive frames' histograms
  (`cv2.compareHist(..., cv2.HISTCMP_CORREL)`). `dissimilarity = max(0, (1 - correlation) / 2)`.
  O(H·W) per frame with a tiny constant factor — the cheap option.

- **Structural Similarity Index (SSIM).** Implemented from first
  principles (no `scikit-image` dependency), following Wang et al.,
  *"Image Quality Assessment: From Error Visibility to Structural
  Similarity,"* IEEE Transactions on Image Processing, 2004:

  ```
  SSIM(x, y) = ((2·μx·μy + C1)·(2·σxy + C2)) / ((μx² + μy² + C1)·(σx² + σy² + C2))
  ```

  where μ, σ², σxy are local (windowed) mean, variance, and covariance,
  estimated with an 11×11 Gaussian window (σ=1.5, via
  `cv2.GaussianBlur`), and `C1 = (0.01·255)²`, `C2 = (0.03·255)²` are the
  paper's standard stabilization constants for 8-bit imagery. The scalar
  index is the mean of the per-pixel SSIM map; `dissimilarity = 1 - mean_SSIM`.
  Several Gaussian blur passes per frame pair make this several times
  slower than the histogram approach in wall-clock terms, in exchange for
  materially better discrimination between "same shot, camera panned" and
  "genuinely different shot."

**Adaptive thresholding.** Rather than one global cutoff,
`detect_scene_boundaries_via_adaptive_thresholding` accepts a candidate
cut only when the dissimilarity score exceeds *both* the strategy's fixed
floor threshold *and* a rolling `mean + k·std` computed over the
preceding N samples (defaults: window=15, k=2.5) — so the detector
self-calibrates to each asset's own baseline noise level instead of
false-triggering on grainy/high-motion footage that has a naturally
higher floor. Candidate cuts closer together than
`minimum_scene_duration_seconds` (default 0.5s) are merged to suppress
flicker.

**Motion complexity score.** Per scene, a heuristic blend of Canny
edge-density ratio (spatial detail proxy) and normalized grayscale pixel
variance (contrast/texture proxy) — `0.6·edge_density + 0.4·normalized_variance`
— intended purely as a relative, asset-internal signal for downstream
bitrate-ladder decisions, not a calibrated absolute metric.

### Black-frame detection

Per sampled frame, convert to grayscale and compute mean and max
luminance. A frame is "black" when *both* its mean luminance is below 16
(of 255) *and* its max luminance is below 32 — the max-luminance check
specifically prevents misclassifying a mostly-black frame with a small
bright highlight (e.g. white titles over black) as a true black frame.
Consecutive black samples merge into ranges; runs shorter than 2 samples
are dropped as noise.

### Color space / HDR detection

Delegates to `ffprobe -show_streams -show_format` (JSON output), parsed
into typed dataclasses. HDR format classification is a **documented
heuristic**, not a full SEI/RPU/NAL parse:

| Signal | Classification |
|---|---|
| `side_data_list` mentions "DOVI"/"Dolby Vision" | `DOLBY_VISION` |
| `side_data_list` mentions "HDR10+"/"hdr10plus" | `HDR10_PLUS` |
| `color_transfer == "arib-std-b67"` | `HLG` |
| `color_transfer == "smpte2084"` (PQ) | `HDR10` |
| BT.2020 primaries, no PQ/HLG transfer | `UNKNOWN` (not guessed) |
| Otherwise | `SDR_BT709` |

HDR10+ dynamic metadata is frequently frame-level rather than
stream-level in ffprobe's output, so its absence at the stream level does
**not** prove an asset lacks HDR10+; this is a best-effort upgrade signal
over plain HDR10, not a negative-proof mechanism.

### Loudness (approximate EBU R128 / ITU-R BS.1770 methodology)

`compute_approximate_integrated_loudness_lufs` implements a **documented
simplification** of the full spec:

**What matches the spec:**
- 400ms measurement blocks, 100ms hop (75% overlap) — same blocking
  scheme as BS.1770.
- Per-block loudness `L = -0.691 + 10·log10(mean_square)` — identical
  formula, minus K-weighting (see below).
- Two-stage gating: absolute gate at -70 LUFS, then relative gate at
  `mean(absolute_gated) - 10 LU`, with final integration as the **energy**
  average (not dB average) of the relative-gated blocks — exactly as EBU
  R128 specifies.
- A loudness-range-style spread (95th minus 10th percentile of gated
  block loudness), echoing EBU Tech 3342's LRA methodology in simplified
  form.

**What's simplified (and why it matters):**
- **No K-weighting pre-filter.** The real algorithm applies a two-stage
  filter (high-shelf + high-pass) approximating human loudness
  perception before power measurement. This implementation measures raw
  sample power, so results will diverge from a certified meter for
  strongly bass- or treble-weighted material.
- **Mono-only.** Real BS.1770 channel-weights each channel independently
  (e.g. +1.5 dB for surrounds) before summing. This implementation
  assumes the caller has already downmixed to mono (the `ffmpeg` audio
  adapter does an unweighted `-ac 1` downmix).
- **True peak is a sample-peak approximation.** A conformant dBTP
  measurement requires ≥4x oversampling to catch inter-sample peaks; this
  reports plain sample-peak level, which can under-estimate true peaks by
  roughly 0.5–3 dB for typical program material.

This is intentionally **not** presented as a certified R128 loudness
meter — treat `integrated_loudness_lufs` as a fast pre-transcode
screening signal (e.g. "is this asset wildly off the streaming target?"),
not as a mastering/QC-grade measurement.

## Extended capabilities

Six additional, purely additive capabilities extend the original
four-stage pipeline/CLI/API. None of them change the default
`build_default_inspection_pipeline` stage order or the default CLI/API
output shape -- every one is opt-in (a stage you must explicitly wire
in, a CLI flag, or a separate endpoint), so existing callers/tests see
identical behavior unless they ask for the new capability.

### 1. Shot-type classification

`strategies/shot_type_classification.py` classifies each detected
`SceneBoundary` into `ShotType.CLOSE_UP` / `WIDE` / `DIALOGUE` /
`ACTION` via `RuleBasedShotTypeClassificationStrategy`, a **rule-based
heuristic** (no ML training) combining two signals computed purely with
`cv2`/`numpy`:

- **Motion magnitude** -- mean absolute grayscale frame-differencing
  delta between consecutive sampled frames in the scene, normalized by
  255.
- **Edge density** -- mean fraction of Canny-edge-flagged pixels across
  the scene's frames (same Canny thresholds as the existing
  `compute_motion_complexity_score`).

Decision list (first match wins), with the documented rationale for
each threshold in `config.ShotTypeClassificationThresholds` and the
strategy class's own docstring:

| Rule | Shot type |
|---|---|
| `motion_magnitude >= 0.08` | ACTION |
| `edge_density >= 0.16` and `motion_magnitude <= 0.03` | CLOSE_UP |
| `motion_magnitude <= 0.03` | DIALOGUE |
| otherwise | WIDE |

Wired via `pipeline.stages.ShotTypeClassificationStage` (Strategy
pattern client, same shape as `SceneCutDetectionStage`) and the opt-in
`pipeline.pipeline.build_inspection_pipeline_with_shot_type_classification`
builder (kept separate from the default 4-stage pipeline specifically
so the existing `pipeline_stage_names_executed` contract is
unaffected). `SceneBoundary.shot_type` is an `Optional[ShotType]`
field defaulting to `None`, so existing code constructing
`SceneBoundary` without it is unaffected. CLI: `--enable-shot-type-classification`.

**Known limitation:** no face/person detector is involved at all --
this is a coarse motion/texture heuristic, not framing-aware computer
vision, and can misclassify e.g. a static wide shot of a highly
detailed set as CLOSE_UP.

### 2. Perceptual hashing / near-duplicate-asset detection

`hashing/perceptual_hash.py` implements a **dHash (difference hash)
from first principles** with `cv2`/`numpy` only (no `imagehash`/`phash`
dependency): grayscale -> resize to `(grid+1) x grid` via
`cv2.INTER_AREA` -> per-row left/right pixel comparisons -> packed bit
integer (64 bits for the default 8x8 grid). An asset-level hash is the
dHash of the **elementwise mean** of its representative frames (more
robust to one unrepresentative frame than hashing just the first
frame). Similarity is Hamming distance
(`compute_hamming_distance`, via `int.bit_count()`), with two assets
considered near-duplicates at or below a configurable threshold
(default 8 bits out of 64, ~12.5%, a common practical dHash
threshold). `InMemoryPerceptualHashRegistry` is a small in-process
`{asset_id: hash}` store with `find_near_duplicate_assets` doing an
O(n) linear scan (a production-scale index would use a BK-tree/LSH
instead -- explicitly out of scope here). Exposed via
`POST /assets/{assetId}/duplicates` (registers the asset's hash and
returns matches) and CLI `--print-perceptual-hash`.

### 3. Subtitle/caption extraction

`adapters/subtitle_extraction.py` shells out to `ffprobe` to enumerate
embedded subtitle streams (`detect_subtitle_streams`) and to `ffmpeg`
to extract a selected stream to standalone WebVTT
(`extract_subtitle_stream_to_webvtt`). A small first-principles WebVTT
cue parser (`parse_webvtt_cues`) feeds
`detect_forced_narrative_heuristic`, which flags a track as likely
"forced narrative" when it is **both** sparse (≤2 cues/minute by
default) **and** short-cued (≤3s mean cue duration by default) --
both conditions must hold, so e.g. sparse-but-long on-screen-text
tracks or dense-but-brief full-dialogue tracks are not misflagged.
Assets with no subtitle streams are handled as a normal, non-error
empty result; ffmpeg/ffprobe-dependent tests self-skip per this
project's established pattern. CLI: `--extract-subtitles-dir`.

### 4. Thumbnail / sprite-sheet generation

`adapters/thumbnail_sprite_generation.py` extracts one representative
frame per detected scene via `ffmpeg` (`-ss <t> -frames:v 1`, piped
PNG decoded with `cv2.imdecode` -- no temp files), then composites them
into a grid sprite sheet with pure `numpy`/`cv2` resize +
array-placement (no external stitching library), wrapping rows at a
configurable column count. `build_webvtt_sprite_index` emits a WebVTT
document whose cues point at
`<sheet.png>#xywh=<x>,<y>,<w>,<h>` Media Fragment URIs per scene time
range -- the same convention video players (e.g. video.js's
`vtt-thumbnails` plugin) use for scrubbing-preview thumbnails. CLI:
`--sprite-sheet-output` / `--sprite-vtt-output`.

### 5. Audio loudness remediation

`adapters/loudness_remediation.py` extends the existing *analysis-only*
LUFS pass into actual *correction*, applying ffmpeg's built-in
`loudnorm` filter (EBU R128 normalization) to produce a new,
loudness-corrected output file, then re-runs this service's own
`compute_approximate_integrated_loudness_lufs` against both the
original and the normalized output for a consistent-methodology
before/after report (`LoudnessRemediationResult`). Uses **single-pass**
`loudnorm` (measurement and correction in one ffmpeg invocation) rather
than the more precise two-pass mode, trading a small amount of final
accuracy (typically within ~1 LU of target) for one ffmpeg
invocation instead of two full-file passes -- documented as a known
limitation, not silently glossed over. CLI:
`--normalize-loudness-output` / `--target-lufs`.

### 6. Prometheus metrics

`metrics.py` centralizes three `prometheus_client` metrics:
`http_requests_total` (Counter, labeled `method`/`path`/`status_code`),
`http_request_duration_seconds` (Histogram, labeled `method`/`path`,
recorded by a FastAPI middleware wrapping every request), and
`pipeline_stage_duration_seconds` (Histogram, labeled `stage_name`,
recorded generically inside `InspectionStage.handle` so every current
and future stage -- including `ShotTypeClassificationStage` -- is
instrumented automatically with zero per-stage boilerplate). Exposed at
`GET /metrics` in standard Prometheus text exposition format.

## Naming convention

Python identifiers in this codebase use **snake_case** for
functions/variables/modules and **PascalCase** for classes, per
[PEP 8](https://peps.python.org/pep-0008/) — this is standard Python
convention, not a stylistic choice specific to this service. Where the
wider platform specification calls for long, descriptive,
"camelCase-style" naming (as is conventional in the platform's Go/Java
services), this codebase honors the *spirit* of that requirement — names
that are unambiguous and self-documenting — while following Python's own
casing convention. For example:

```python
def detect_scene_boundaries_via_adaptive_thresholding(...) -> list[SceneBoundary]:
    ...
```

is the Pythonic equivalent of a hypothetical Go/Java
`detectSceneBoundariesViaAdaptiveThresholding`, not a terser
`detect_scenes`.

## Project layout

```
ingestion-inspection-service/
├── pyproject.toml
├── requirements.txt
├── README.md
├── src/ingestion_inspection_service/
│   ├── config.py                    # Tunable thresholds (frame sampling, scene-cut, black-frame, loudness, shot-type, hashing, sprites)
│   ├── metrics.py                   # Prometheus Counter/Histogram metric objects
│   ├── domain/
│   │   └── models.py                # StructuralMap, SceneBoundary (+ shot_type), ColorSpaceProfile, LoudnessAnalysisResult, ShotType, ...
│   ├── strategies/
│   │   ├── scene_detection.py       # Strategy pattern: histogram-diff & SSIM scene-cut detectors
│   │   └── shot_type_classification.py  # Strategy pattern: rule-based close-up/wide/dialogue/action classifier
│   ├── hashing/
│   │   └── perceptual_hash.py       # dHash from first principles + Hamming distance + in-memory duplicate registry
│   ├── pipeline/
│   │   ├── context.py               # InspectionContext (shared chain-of-responsibility state)
│   │   ├── stages.py                # BlackFrame / SceneCut / ShotType / ColorSpace / Loudness stages
│   │   └── pipeline.py              # InspectionPipeline orchestrator + context builder + pipeline variants
│   ├── adapters/
│   │   ├── ffprobe_client.py        # ffprobe subprocess → typed dataclasses
│   │   ├── frame_source.py          # OpenCV fixed-rate frame sampling
│   │   ├── audio_extraction.py      # ffmpeg subprocess → mono PCM for loudness analysis
│   │   ├── loudness_remediation.py  # ffmpeg loudnorm-based loudness correction + before/after LUFS
│   │   ├── subtitle_extraction.py   # ffprobe/ffmpeg subtitle detection, WebVTT extraction, forced-narrative heuristic
│   │   └── thumbnail_sprite_generation.py  # ffmpeg frame extraction + sprite-sheet compositing + WebVTT sprite index
│   ├── api/
│   │   ├── app.py                   # FastAPI app: /assets/{assetId}/inspect, /assets/{assetId}/duplicates, /metrics
│   │   └── schemas.py               # Pydantic request/response models
│   └── cli/
│       └── main.py                  # CLI entry point (argparse) + optional post-inspection extras
└── tests/
    ├── conftest.py                  # Synthetic frame builders + real synthetic-video fixture
    ├── test_domain_models.py
    ├── test_scene_detection.py
    ├── test_shot_type_classification.py
    ├── test_perceptual_hashing.py
    ├── test_subtitle_extraction.py
    ├── test_thumbnail_sprite_generation.py
    ├── test_loudness_remediation.py
    ├── test_metrics_and_duplicate_detection_api.py
    ├── test_black_frame_detection.py
    ├── test_loudness_analysis.py
    ├── test_pipeline.py
    ├── test_ffprobe_and_frame_source_integration.py
    └── test_cli_and_api_integration.py
```

## Setup

This service depends on `numpy`, `opencv-python-headless`, `fastapi`,
`uvicorn`, `pydantic`, and `prometheus_client` at runtime, plus the
external `ffmpeg`/`ffprobe` binaries (used via subprocess for container
metadata, audio extraction, subtitle extraction, thumbnail/sprite
extraction, and loudness remediation — not required for the pure
scene-cut/black-frame/shot-type/perceptual-hash math, which operates on
NumPy arrays you supply).

```bash
cd services/ingestion-inspection-service
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
# or: pip install -e ".[dev]"
```

**In this environment**, a virtualenv was created at `.venv/` inside this
service directory and `pip install numpy opencv-python-headless fastapi
uvicorn pydantic prometheus_client pytest httpx` completed successfully —
all dependencies installed cleanly with no sandboxing issues.
`ffmpeg`/`ffprobe` were already present on `PATH` (Homebrew installs at
`/opt/homebrew/bin/`). If your environment lacks internet access for
`pip install` or lacks `ffmpeg`/`ffprobe`, the pure algorithmic logic
(scene-cut math, black-frame math, shot-type classification math,
perceptual-hash math, sprite-sheet coordinate math, loudness math,
pipeline composition) still runs and is still tested — see
[Testing](#testing) — because those code paths only require
`numpy`/`cv2`/plain Python and synthetic in-memory arrays, never a real
video file or the ffprobe/ffmpeg binaries.

## Running it

### CLI

```bash
python -m ingestion_inspection_service.cli.main \
  --asset-id my-asset-123 \
  --source-file /path/to/mezzanine.mov \
  --output-json /path/to/structural_map.json \
  --scene-detection-strategy structural_similarity \
  --analysis-fps 1.0
```

### HTTP service (for the Go orchestrator)

```bash
uvicorn ingestion_inspection_service.api.app:app --host 0.0.0.0 --port 8080
```

```bash
curl -X POST http://localhost:8080/assets/my-asset-123/inspect \
  -H "Content-Type: application/json" \
  -d '{
        "source_file_path": "/mnt/mezzanine/my-asset-123.mov",
        "scene_detection_strategy": "structural_similarity",
        "analysis_frames_per_second": 1.0
      }'
```

```bash
curl -X POST http://localhost:8080/assets/my-asset-123/duplicates \
  -H "Content-Type: application/json" \
  -d '{"source_file_path": "/mnt/mezzanine/my-asset-123.mov"}'
```

```bash
curl http://localhost:8080/metrics
```

## Testing

```bash
cd services/ingestion-inspection-service
.venv/bin/python -m pytest -v
```

**Verified in this environment: 141 passed, 0 failed** (67 from the
original four-capability baseline + 74 new tests covering the six
extended capabilities). The suite is split into two tiers:

1. **Pure algorithmic tests** (`test_domain_models.py`,
   `test_scene_detection.py`, `test_shot_type_classification.py`,
   `test_perceptual_hashing.py`, `test_black_frame_detection.py`,
   `test_loudness_analysis.py`, `test_pipeline.py`, plus the
   non-ffmpeg-dependent classes in `test_subtitle_extraction.py` and
   `test_thumbnail_sprite_generation.py`) — construct small synthetic
   NumPy frame arrays and PCM arrays directly (checkerboards, solid
   colors, sine waves, hand-built WebVTT text) and exercise the real
   scene-cut, black-frame, shot-type, perceptual-hash, sprite-coordinate,
   loudness, and pipeline-composition logic with **no** video file,
   `ffprobe`, or `ffmpeg` dependency. These run in any environment with
   `numpy`, `opencv-python-headless`, and `pytest` installed.

2. **Integration tests** (`test_ffprobe_and_frame_source_integration.py`,
   `test_cli_and_api_integration.py`, `test_loudness_remediation.py`, the
   ffmpeg-dependent classes in `test_subtitle_extraction.py` and
   `test_thumbnail_sprite_generation.py`, and the duplicate-detection
   endpoint class in `test_metrics_and_duplicate_detection_api.py`) —
   generate a tiny real 2-second, 64×64 H.264/AAC MP4 via `ffmpeg` (a
   hard red→blue cut plus a 440Hz sine tone) and run the real `ffprobe`
   adapter, OpenCV frame decode, CLI, and FastAPI endpoints against it
   end-to-end. These are automatically `skip`ped (not failed) when
   `ffmpeg`/`ffprobe` are not on `PATH`, per the task's documented
   fallback requirement. The `/metrics` endpoint and pipeline-stage
   timing-histogram tests have no ffmpeg dependency and always run.

## Performance targets & the sampling tradeoff

**Target: inspect a 2-hour 4K mezzanine asset in well under 90 seconds of
analysis-side compute**, using 1 fps frame-sampled analysis rather than a
full-frame-rate (e.g. 24/25/30 fps) decode.

**Why sampling, and the tradeoff it makes:**

- At native frame rate, a 2-hour asset at 24fps is ~172,800 frames. Even
  a cheap per-frame op costs real time at that count, and SSIM's
  Gaussian-blur-based windowed computation is not free.
- At 1 fps analysis sampling, the same asset is ~7,200 sampled frames —
  a **24x** reduction in frames actually pushed through
  histogram/SSIM/Canny — which is what makes the sub-90s target
  achievable for the *analysis* math itself.
- **The tradeoff:** temporal resolution is capped at 1 second. A shot
  shorter than ~1 second (rapid-cut trailers, strobing effects, some
  action-sequence editing) may be under-sampled or missed entirely by
  the scene-cut detector, and a black-frame flash shorter than 1 frame
  at 1fps sampling could be skipped between samples. This is a
  deliberate, documented precision-for-speed tradeoff appropriate for a
  **pre-transcode triage/parameterization** pass, not a frame-accurate
  QC tool. Increase `analysis_frames_per_second` (e.g. to 4-8 fps) for
  content classes where sub-second shot changes matter, at a
  proportional cost increase.
- **A second, less obvious cost:** this implementation decodes
  *sequentially* and only converts every Nth frame to a NumPy array
  (rather than doing per-sample keyframe seeks), because sequential
  reads are more robust across arbitrary mezzanine containers than
  timestamp-based seeking. This means OpenCV must still demux every
  frame it steps past — the *decode* cost does not shrink linearly with
  the sampling rate the way the *analysis* cost does. On typical
  hardware this is still comfortably within budget for the 1fps default,
  but it is the main reason the target is stated as "well under 90s" for
  the CPU-bound analysis math specifically, not as a hard guarantee
  across all storage/decode-throughput environments.
- ffprobe's container/stream metadata extraction (`-show_format
  -show_streams`) does not decode frame payloads at all and is
  effectively O(1) relative to asset duration — it is not a contributor
  to the sampling tradeoff.

**Complexity summary:**

| Stage | Per-sample cost | Total (n = sampled frames) |
|---|---|---|
| Black-frame detection | O(H·W) grayscale + reduce | O(n·H·W) |
| Histogram-diff scene cut | O(H·W) histogram build, O(1) compare | O(n·H·W) |
| SSIM scene cut | O(H·W) × ~5 Gaussian blur passes | O(n·H·W), larger constant |
| Motion complexity (Canny) | O(H·W) | O(n·H·W) |
| Color-space extraction | O(1) (ffprobe, no frame decode) | O(1) |
| Loudness analysis | O(audio_samples) | O(duration × sample_rate) |

## Known limitations

- HDR format classification (`HdrFormat`) is a documented **heuristic**
  over ffprobe's exposed metadata fields, not a full Dolby Vision
  RPU/HDR10+ dynamic-metadata parser. See the [Color space / HDR
  detection](#color-space--hdr-detection) section above.
- The loudness pass is an **approximation** of EBU R128/BS.1770, missing
  K-weighting and true multi-channel handling. See [Loudness](#loudness-approximate-ebu-r128--itu-r-bs1770-methodology)
  above for the full list of simplifications and their practical impact.
- True-peak (dBTP) reporting is a sample-peak approximation without
  oversampling, and can under-report inter-sample peaks by ~0.5-3 dB.
- `motion_complexity_score` is a relative, asset-internal heuristic
  (edge density + intensity variance), not a calibrated encoding-cost
  predictor; it is intended to inform relative bitrate-ladder decisions
  across scenes of the *same* asset, not cross-asset comparisons.
- 1 fps analysis sampling (the default) will under-resolve shots shorter
  than roughly one second; see the sampling tradeoff section above.
- All dependencies (`numpy`, `opencv-python-headless`, `fastapi`,
  `uvicorn`, `pydantic`, `pytest`, `httpx`) installed successfully via
  `pip` into a local virtualenv in this environment, and `ffmpeg`/
  `ffprobe` were already present on `PATH`, so no pip-install or
  binary-availability limitations were actually encountered while
  building this service. The code and test suite are nonetheless
  structured so that the pure-Python/NumPy/OpenCV computational logic
  remains testable even in an environment lacking `ffmpeg`/`ffprobe`
  (integration tests self-skip in that case).
- **Shot-type classification** (`ShotType`) is a coarse motion/edge-
  density heuristic with no face/person/framing detection at all; see
  [Extended capabilities #1](#extended-capabilities) for the exact
  decision list and its practical failure modes.
- **Perceptual hashing** uses dHash (gradient-sign based), not pHash
  (DCT-based); dHash is more sensitive to rotation/flips than pHash and
  the asset-level hash (mean-of-representative-frames) blurs
  frame-to-frame variation within one asset. Near-duplicate search is an
  O(n) linear scan appropriate only for a small in-memory registry, not
  a production-scale duplicate index.
- **Subtitle extraction**'s forced-narrative detection is a sparse/short
  cue-count heuristic only — it has no language/semantic awareness and
  cannot distinguish "forced narrative" from any other genuinely sparse,
  short-cued caption track.
- **Sprite-sheet thumbnails** are extracted at ffmpeg's nearest
  preceding keyframe to each scene's start timestamp (fast input-side
  seek), not frame-exact — acceptable for a scrubbing preview, not for
  frame-accurate QC.
- **Loudness remediation** uses ffmpeg's single-pass `loudnorm` mode,
  which is measurably less precise than its two-pass mode; see
  [Extended capabilities #5](#extended-capabilities).
- **Prometheus metrics** measure this process's own request/stage
  timings only; there is no cross-instance aggregation, and the
  in-process metric registry (like the perceptual-hash registry) resets
  on restart.
