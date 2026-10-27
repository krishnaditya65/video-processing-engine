"""Centralized Prometheus metric objects (`prometheus_client`).

Design note: metric objects are process-global singletons by
construction (the ``prometheus_client`` registry is itself a
module-level global), so -- exactly like ``config.py``'s
``DEFAULT_*`` constants -- this module exists to give every metric a
single, documented, importable home rather than scattering
``Counter("...")``/``Histogram("...")`` calls (and the risk of
double-registration errors) across the pipeline and API layers.

Metrics defined here:

  * ``http_requests_total`` (Counter) -- total HTTP requests handled by
    the FastAPI app, labeled by ``method``, ``path``, and
    ``status_code``.
  * ``http_request_duration_seconds`` (Histogram) -- wall-clock latency
    of HTTP requests, labeled by ``method`` and ``path``.
  * ``pipeline_stage_duration_seconds`` (Histogram) -- wall-clock
    duration of each inspection pipeline stage's ``process()`` call,
    labeled by ``stage_name``. Recorded generically by
    ``pipeline.stages.InspectionStage.handle`` so every current and
    future stage (including the new
    :class:`~ingestion_inspection_service.pipeline.stages.ShotTypeClassificationStage`)
    is instrumented automatically with no per-stage boilerplate.

Exposed at ``GET /metrics`` in the standard Prometheus text exposition
format via ``prometheus_client.generate_latest`` (see ``api.app``).
"""

from __future__ import annotations

import prometheus_client

HTTP_REQUESTS_TOTAL_COUNTER = prometheus_client.Counter(
    "http_requests_total",
    "Total number of HTTP requests handled by the ingestion-inspection-service API.",
    labelnames=("method", "path", "status_code"),
)

HTTP_REQUEST_DURATION_SECONDS_HISTOGRAM = prometheus_client.Histogram(
    "http_request_duration_seconds",
    "Wall-clock latency, in seconds, of HTTP requests handled by the API.",
    labelnames=("method", "path"),
)

PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM = prometheus_client.Histogram(
    "pipeline_stage_duration_seconds",
    "Wall-clock duration, in seconds, of each inspection pipeline stage's process() call.",
    labelnames=("stage_name",),
)
