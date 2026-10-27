"""FastAPI application exposing the inspection pipeline over HTTP.

Intended to be called by a Go orchestrator service as part of the
platform's pre-transcode ingestion workflow:

    POST /assets/{assetId}/inspect
    {
        "source_file_path": "/mnt/mezzanine/asset-123.mov",
        "scene_detection_strategy": "structural_similarity",
        "analysis_frames_per_second": 1.0,
        "skip_audio_loudness_analysis": false
    }

Run locally with::

    uvicorn ingestion_inspection_service.api.app:app --host 0.0.0.0 --port 8080

This module intentionally contains no business logic of its own beyond
request/response marshalling and HTTP status mapping; all analysis work
is delegated to :mod:`ingestion_inspection_service.pipeline`.
"""

from __future__ import annotations

import logging
import time
import typing

import fastapi
import fastapi.responses
import prometheus_client

from ingestion_inspection_service.adapters.ffprobe_client import FfprobeExecutionError
from ingestion_inspection_service.adapters.frame_source import (
    VideoDecodeError,
    sample_frames_at_fixed_rate,
)
from ingestion_inspection_service.api.schemas import (
    DuplicateDetectionRequestBody,
    DuplicateDetectionResponseBody,
    InspectAssetRequestBody,
    InspectAssetResponseBody,
    InspectionErrorResponseBody,
    NearDuplicateMatchBody,
)
from ingestion_inspection_service.config import FrameSamplingConfiguration
from ingestion_inspection_service.hashing.perceptual_hash import (
    InMemoryPerceptualHashRegistry,
    compute_asset_representative_perceptual_hash,
)
from ingestion_inspection_service.metrics import (
    HTTP_REQUEST_DURATION_SECONDS_HISTOGRAM,
    HTTP_REQUESTS_TOTAL_COUNTER,
)
from ingestion_inspection_service.pipeline.pipeline import (
    build_default_inspection_pipeline,
    create_inspection_context_from_source_file,
)
from ingestion_inspection_service.strategies.scene_detection import (
    HistogramDifferenceSceneDetectionStrategy,
    StructuralSimilaritySceneDetectionStrategy,
)

logger = logging.getLogger("ingestion_inspection_service.api")

app = fastapi.FastAPI(
    title="Ingestion & Inspection Plane",
    description=(
        "Pre-transcode deep inspection of raw mezzanine video assets: "
        "scene-cut detection, black-frame detection, color space/HDR "
        "metadata extraction, and approximate loudness analysis."
    ),
    version="0.1.0",
)

# Process-global in-memory perceptual-hash registry backing
# POST /assets/{assetId}/duplicates. Intentionally a simple in-process
# store (see hashing.perceptual_hash.InMemoryPerceptualHashRegistry's
# docstring) rather than a persistent index -- matches the task's "small
# in-memory hash registry" scope.
_PERCEPTUAL_HASH_REGISTRY = InMemoryPerceptualHashRegistry()


@app.middleware("http")
async def record_request_metrics_middleware(
    request: fastapi.Request, call_next: typing.Callable
) -> fastapi.Response:
    """Records Prometheus request-count and latency metrics for every request.

    Args:
        request: The incoming HTTP request.
        call_next: Calls the next middleware/route handler in the chain.

    Returns:
        fastapi.Response: The response produced downstream, unmodified.

    Algorithmic notes:
        O(1) overhead per request: one wall-clock timer plus two
        Prometheus client calls (``Counter.inc``, ``Histogram.observe``),
        which are themselves O(1) in-process counter updates (no I/O on
        the request path -- metrics are only serialized when ``/metrics``
        is scraped).
    """
    start_time = time.monotonic()
    response = await call_next(request)
    duration_seconds = time.monotonic() - start_time

    route_path = request.scope.get("route").path if request.scope.get("route") else request.url.path
    HTTP_REQUESTS_TOTAL_COUNTER.labels(
        method=request.method, path=route_path, status_code=str(response.status_code)
    ).inc()
    HTTP_REQUEST_DURATION_SECONDS_HISTOGRAM.labels(
        method=request.method, path=route_path
    ).observe(duration_seconds)
    return response


@app.get("/healthz", tags=["operations"])
def get_service_health_status() -> dict:
    """Liveness/readiness probe endpoint.

    Returns:
        dict: A static ``{"status": "ok"}`` payload; a 200 response is
        the entire signal orchestrators/load balancers should rely on.
    """
    return {"status": "ok"}


@app.get("/metrics", tags=["operations"])
def get_prometheus_metrics() -> fastapi.responses.Response:
    """Exposes process metrics in Prometheus text exposition format.

    Returns:
        fastapi.responses.Response: The current metric registry snapshot
        (request counts/latencies, pipeline-stage timing histograms),
        rendered via ``prometheus_client.generate_latest`` with the
        ``Content-Type`` Prometheus scrapers expect.
    """
    return fastapi.responses.Response(
        content=prometheus_client.generate_latest(),
        media_type=prometheus_client.CONTENT_TYPE_LATEST,
    )


@app.post(
    "/assets/{asset_id}/inspect",
    response_model=InspectAssetResponseBody,
    responses={
        404: {"model": InspectionErrorResponseBody},
        422: {"model": InspectionErrorResponseBody},
        500: {"model": InspectionErrorResponseBody},
    },
    tags=["inspection"],
)
def inspect_asset(
    asset_id: str, request_body: InspectAssetRequestBody
) -> InspectAssetResponseBody:
    """Runs the full inspection pipeline over one asset and returns its map.

    Args:
        asset_id: Path parameter identifying the asset; propagated into
            the resulting :class:`StructuralMap`'s ``asset_id`` field.
        request_body: Parsed :class:`InspectAssetRequestBody` request
            payload.

    Returns:
        InspectAssetResponseBody: Wraps the JSON-serializable
        structural map.

    Raises:
        fastapi.HTTPException: 404 if the source file cannot be
            opened/decoded, 500 for any other unexpected failure during
            analysis. ffprobe failures are treated as non-fatal (the
            color-space stage degrades gracefully), matching the CLI's
            behavior.
    """
    strategy = (
        HistogramDifferenceSceneDetectionStrategy()
        if request_body.scene_detection_strategy == "histogram_difference"
        else StructuralSimilaritySceneDetectionStrategy()
    )

    try:
        context = create_inspection_context_from_source_file(
            asset_id=asset_id,
            source_file_path=request_body.source_file_path,
            analysis_frames_per_second=request_body.analysis_frames_per_second,
            extract_audio_for_loudness_analysis=not request_body.skip_audio_loudness_analysis,
        )
    except VideoDecodeError as decode_error:
        raise fastapi.HTTPException(
            status_code=404, detail=str(decode_error)
        ) from decode_error
    except FfprobeExecutionError as ffprobe_error:
        raise fastapi.HTTPException(
            status_code=404, detail=str(ffprobe_error)
        ) from ffprobe_error

    try:
        pipeline = build_default_inspection_pipeline(strategy)
        pipeline.run(context)
        structural_map = pipeline.build_structural_map(context)
    except Exception as unexpected_error:  # noqa: BLE001 - HTTP boundary
        logger.exception(
            "Unexpected failure inspecting asset_id=%s source_file_path=%s",
            asset_id,
            request_body.source_file_path,
        )
        raise fastapi.HTTPException(
            status_code=500, detail=str(unexpected_error)
        ) from unexpected_error

    return InspectAssetResponseBody(
        structural_map=structural_map.to_json_serializable_dict()
    )


@app.post(
    "/assets/{asset_id}/duplicates",
    response_model=DuplicateDetectionResponseBody,
    responses={
        404: {"model": InspectionErrorResponseBody},
        422: {"model": InspectionErrorResponseBody},
    },
    tags=["duplicate-detection"],
)
def check_asset_for_near_duplicates(
    asset_id: str, request_body: DuplicateDetectionRequestBody
) -> DuplicateDetectionResponseBody:
    """Computes an asset's perceptual hash, registers it, and finds near-duplicates.

    Samples a small number of evenly-spaced frames from the source
    file, reduces them to a single dHash via
    :func:`~ingestion_inspection_service.hashing.perceptual_hash.compute_asset_representative_perceptual_hash`,
    registers the hash under ``asset_id`` in the process-global
    in-memory registry, and returns any other previously registered
    assets within the Hamming-distance threshold.

    Args:
        asset_id: Path parameter identifying the asset to hash/check.
        request_body: Parsed :class:`DuplicateDetectionRequestBody`.

    Returns:
        DuplicateDetectionResponseBody: The computed hash and any
        near-duplicate matches found against previously registered
        assets.

    Raises:
        fastapi.HTTPException: 404 if the source file cannot be
            opened/decoded by OpenCV.
    """
    try:
        sampled_frames = list(
            sample_frames_at_fixed_rate(
                request_body.source_file_path,
                FrameSamplingConfiguration(
                    target_analysis_frames_per_second=1.0,
                    maximum_frames_to_sample=request_body.representative_frame_sample_count,
                ),
            )
        )
    except VideoDecodeError as decode_error:
        raise fastapi.HTTPException(
            status_code=404, detail=str(decode_error)
        ) from decode_error

    if not sampled_frames:
        raise fastapi.HTTPException(
            status_code=404,
            detail=f"No frames could be sampled from '{request_body.source_file_path}'.",
        )

    perceptual_hash = compute_asset_representative_perceptual_hash(
        [frame.frame_bgr for frame in sampled_frames]
    )
    _PERCEPTUAL_HASH_REGISTRY.register_asset_hash(asset_id, perceptual_hash)

    matches = _PERCEPTUAL_HASH_REGISTRY.find_near_duplicate_assets(
        asset_id,
        hamming_distance_threshold=request_body.near_duplicate_hamming_distance_threshold,
    )

    return DuplicateDetectionResponseBody(
        asset_id=asset_id,
        perceptual_hash_hex=format(perceptual_hash, "016x"),
        near_duplicate_matches=[
            NearDuplicateMatchBody(
                asset_id=match.asset_id, hamming_distance=match.hamming_distance
            )
            for match in matches
        ],
    )
