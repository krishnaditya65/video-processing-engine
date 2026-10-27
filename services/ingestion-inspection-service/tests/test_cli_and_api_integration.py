"""End-to-end integration tests for the CLI and FastAPI entry points.

Runs the exact same code path a real orchestrator/operator would invoke,
against the small real synthetic video fixture. Skipped automatically
when ffmpeg/ffprobe are unavailable.
"""

from __future__ import annotations

import json

import pytest

from tests.conftest import ffmpeg_and_ffprobe_are_available

pytestmark = pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)


class TestCliEndToEnd:
    """Tests the CLI entry point against the real synthetic video fixture."""

    def test_cli_writes_valid_structural_map_json(
        self, synthetic_test_video_path: str, tmp_path
    ) -> None:
        """Running the CLI produces a structural map JSON file with expected fields."""
        from ingestion_inspection_service.cli.main import run_cli_inspection

        output_path = tmp_path / "structural_map.json"

        exit_code = run_cli_inspection(
            [
                "--asset-id",
                "cli-integration-asset",
                "--source-file",
                synthetic_test_video_path,
                "--output-json",
                str(output_path),
                "--scene-detection-strategy",
                "histogram_difference",
                "--analysis-fps",
                "5.0",
            ]
        )

        assert exit_code == 0
        assert output_path.exists()

        payload = json.loads(output_path.read_text())
        assert payload["asset_id"] == "cli-integration-asset"
        assert payload["duration_seconds"] == pytest.approx(2.0, abs=0.3)
        assert payload["pipeline_stage_names_executed"] == [
            "black_frame_detection",
            "scene_cut_detection",
            "color_space_extraction",
            "loudness_analysis",
        ]
        assert len(payload["scene_boundaries"]) >= 1

    def test_cli_returns_nonzero_exit_code_for_missing_file(self, tmp_path) -> None:
        """The CLI reports failure (nonzero exit code) for a nonexistent source file."""
        from ingestion_inspection_service.cli.main import run_cli_inspection

        exit_code = run_cli_inspection(
            [
                "--asset-id",
                "missing-file-asset",
                "--source-file",
                "/no/such/file/anywhere.mov",
            ]
        )

        assert exit_code == 1


class TestFastApiEndToEnd:
    """Tests the FastAPI service against the real synthetic video fixture."""

    def test_inspect_endpoint_returns_structural_map(
        self, synthetic_test_video_path: str
    ) -> None:
        """POST /assets/{assetId}/inspect returns a 200 with a populated structural map."""
        from fastapi.testclient import TestClient

        from ingestion_inspection_service.api.app import app

        client = TestClient(app)

        response = client.post(
            "/assets/api-integration-asset/inspect",
            json={
                "source_file_path": synthetic_test_video_path,
                "scene_detection_strategy": "histogram_difference",
                "analysis_frames_per_second": 5.0,
            },
        )

        assert response.status_code == 200
        body = response.json()
        structural_map = body["structural_map"]
        assert structural_map["asset_id"] == "api-integration-asset"
        assert structural_map["duration_seconds"] == pytest.approx(2.0, abs=0.3)
        assert len(structural_map["scene_boundaries"]) >= 1

    def test_inspect_endpoint_returns_404_for_missing_file(self) -> None:
        """POST .../inspect returns 404 when the source file cannot be decoded."""
        from fastapi.testclient import TestClient

        from ingestion_inspection_service.api.app import app

        client = TestClient(app)

        response = client.post(
            "/assets/missing-file-asset/inspect",
            json={"source_file_path": "/no/such/file/anywhere.mov"},
        )

        assert response.status_code == 404

    def test_health_endpoint_returns_ok(self) -> None:
        """GET /healthz returns a 200 with status ok."""
        from fastapi.testclient import TestClient

        from ingestion_inspection_service.api.app import app

        client = TestClient(app)
        response = client.get("/healthz")

        assert response.status_code == 200
        assert response.json() == {"status": "ok"}

    def test_inspect_endpoint_rejects_invalid_strategy_choice(
        self, synthetic_test_video_path: str
    ) -> None:
        """An invalid scene_detection_strategy value is rejected with 422."""
        from fastapi.testclient import TestClient

        from ingestion_inspection_service.api.app import app

        client = TestClient(app)

        response = client.post(
            "/assets/asset-x/inspect",
            json={
                "source_file_path": synthetic_test_video_path,
                "scene_detection_strategy": "not_a_real_strategy",
            },
        )

        assert response.status_code == 422
