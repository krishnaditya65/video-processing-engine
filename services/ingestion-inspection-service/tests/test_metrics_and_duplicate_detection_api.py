"""Tests for the Prometheus /metrics endpoint and the duplicate-detection API.

The /metrics endpoint and request-count/latency middleware are pure
FastAPI/prometheus_client wiring, testable via FastAPI's in-process
TestClient without any real video file. Pipeline-stage timing histogram
recording is exercised directly against the synthetic pipeline context
already used by ``test_pipeline.py``. The duplicate-detection endpoint
needs a real decodable video file, so that test class is skipped
automatically when ffmpeg/ffprobe are unavailable, per the existing
project convention.
"""

from __future__ import annotations

import numpy as np
import prometheus_client
import pytest
from fastapi.testclient import TestClient

from ingestion_inspection_service.api.app import app
from ingestion_inspection_service.metrics import (
    PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM,
)
from ingestion_inspection_service.pipeline.context import (
    InspectionContext,
    SampledFrameRecord,
)
from ingestion_inspection_service.pipeline.stages import BlackFrameDetectionStage
from tests.conftest import ffmpeg_and_ffprobe_are_available, make_solid_color_frame


class TestMetricsEndpoint:
    """Tests GET /metrics."""

    def test_metrics_endpoint_returns_200_with_prometheus_content_type(self) -> None:
        """The endpoint returns 200 with the Prometheus text exposition content type."""
        client = TestClient(app)
        response = client.get("/metrics")
        assert response.status_code == 200
        assert "text/plain" in response.headers["content-type"]

    def test_metrics_body_includes_http_request_metric_names(self) -> None:
        """After at least one request, the http_requests_total metric is present."""
        client = TestClient(app)
        client.get("/healthz")
        response = client.get("/metrics")
        assert "http_requests_total" in response.text

    def test_metrics_body_includes_pipeline_stage_duration_metric_name(self) -> None:
        """The pipeline_stage_duration_seconds histogram is registered/exposed."""
        client = TestClient(app)
        response = client.get("/metrics")
        assert "pipeline_stage_duration_seconds" in response.text

    def test_healthz_request_increments_request_counter(self) -> None:
        """Calling /healthz is reflected as a labeled sample in /metrics."""
        client = TestClient(app)
        client.get("/healthz")
        response = client.get("/metrics")
        assert 'path="/healthz"' in response.text


def _read_histogram_sample_count(
    histogram: "prometheus_client.Histogram", stage_name: str
) -> float:
    """Reads the current ``_count`` sample value for one label combination.

    Args:
        histogram: The Prometheus Histogram metric to inspect.
        stage_name: The ``stage_name`` label value to filter samples by.

    Returns:
        float: The current observation count for that label combination,
        or 0.0 if no samples exist yet.
    """
    for metric_family in histogram.collect():
        for sample in metric_family.samples:
            if (
                sample.name.endswith("_count")
                and sample.labels.get("stage_name") == stage_name
            ):
                return sample.value
    return 0.0


class TestPipelineStageDurationMetricRecording:
    """Tests that running a real stage records an observation on the histogram."""

    def test_running_a_stage_increments_its_histogram_sample_count(self) -> None:
        """handle() records a wall-clock observation labeled by stage_name."""
        before_count = _read_histogram_sample_count(
            PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM, "black_frame_detection"
        )

        frame = make_solid_color_frame(16, 16, (0, 0, 0))
        context = InspectionContext(
            asset_id="metrics-test-asset",
            source_file_path="/synthetic/does-not-exist.mov",
            sampled_frames=[SampledFrameRecord(frame, 0.0)],
            duration_seconds=1.0,
        )
        BlackFrameDetectionStage().handle(context)

        after_count = _read_histogram_sample_count(
            PIPELINE_STAGE_DURATION_SECONDS_HISTOGRAM, "black_frame_detection"
        )
        assert after_count == before_count + 1


@pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)
class TestDuplicateDetectionEndpoint:
    """Integration tests for POST /assets/{assetId}/duplicates."""

    def test_registering_the_same_file_twice_reports_itself_as_a_near_duplicate(
        self, synthetic_test_video_path: str
    ) -> None:
        """Hashing the same source file under two asset IDs finds a duplicate match."""
        client = TestClient(app)

        first_response = client.post(
            "/assets/dup-asset-a/duplicates",
            json={"source_file_path": synthetic_test_video_path},
        )
        second_response = client.post(
            "/assets/dup-asset-b/duplicates",
            json={"source_file_path": synthetic_test_video_path},
        )

        assert first_response.status_code == 200
        assert second_response.status_code == 200
        matches = second_response.json()["near_duplicate_matches"]
        assert any(match["asset_id"] == "dup-asset-a" for match in matches)

    def test_response_includes_a_hex_perceptual_hash(
        self, synthetic_test_video_path: str
    ) -> None:
        """The response includes a nonempty hex-encoded perceptual hash string."""
        client = TestClient(app)
        response = client.post(
            "/assets/dup-hash-format-asset/duplicates",
            json={"source_file_path": synthetic_test_video_path},
        )
        assert response.status_code == 200
        hash_hex = response.json()["perceptual_hash_hex"]
        assert len(hash_hex) == 16
        int(hash_hex, 16)  # must be valid hex

    def test_nonexistent_source_file_returns_404(self) -> None:
        """A source file that cannot be decoded returns a 404, not a 500."""
        client = TestClient(app)
        response = client.post(
            "/assets/dup-missing-file-asset/duplicates",
            json={"source_file_path": "/no/such/file/anywhere.mov"},
        )
        assert response.status_code == 404
