"""Ingestion & Inspection Plane microservice.

This package implements the pre-transcode analysis stage of a hyperscale
video processing/streaming platform. Its job is to deeply inspect a raw
mezzanine (source) video asset *before* it is handed to the transcoding
fleet, so that downstream encoding jobs can be parameterized correctly
(e.g. shot-aware GOP placement at scene cuts, correct color space / HDR
tone-mapping pipelines, loudness normalization targets) and so that
obviously broken ingests (black frame storms, silent audio, corrupt
color metadata) can be quarantined early rather than burning expensive
GPU/CPU transcode capacity.

Sub-packages
------------
domain
    Pure dataclass domain model (StructuralMap, SceneBoundary,
    ColorSpaceProfile, LoudnessAnalysisResult, ...). No I/O, no
    third-party video libraries -- just data.
strategies
    Strategy-pattern implementations of pluggable scene-cut detection
    algorithms (histogram-difference vs. structural-similarity/SSIM).
pipeline
    Chain-of-Responsibility / Pipeline implementation of the ordered
    inspection stages: black-frame detection -> scene-cut detection ->
    color-space extraction -> loudness analysis.
adapters
    Boundary/integration code: the ffprobe subprocess client and the
    OpenCV-based frame sampling source. Everything that talks to the
    outside world (filesystem, subprocess, OS) lives here so the rest
    of the codebase can be tested without real video files.
api
    FastAPI HTTP surface (``POST /assets/{assetId}/inspect``) intended
    to be called by a Go orchestrator service.
cli
    Command-line entry point for local/batch invocation of the same
    inspection pipeline.

Naming convention
------------------
Per PEP 8, all Python identifiers in this package use snake_case
(functions, variables, module names) and PascalCase for classes. Where
the wider platform specification calls for descriptive "camelCase-like"
naming, we honor the *spirit* of that requirement -- long, descriptive,
unambiguous, contextual names -- while following Python's own
convention for the *casing*. For example, the scene-cut detector is
named ``detect_scene_boundaries_via_structural_similarity`` rather than
the terser ``detect_scenes``, matching the descriptiveness of a
hypothetical Go/Java sibling method such as
``detectSceneBoundariesViaStructuralSimilarity`` but spelled the
Pythonic way.
"""

__version__ = "0.1.0"
