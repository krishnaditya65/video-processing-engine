"""Tests for the FastAPI prediction endpoint, using a mocked model/pipeline.

The prediction pipeline dependency is overridden with a lightweight fake
so these tests exercise request validation, response shaping (including
the camelCase wire schema), and the WebSocket-push side effect without
depending on a real trained Isolation Forest.
"""

from __future__ import annotations

from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient

from predictive_qoe_ml_service.api.main import (
    app,
    get_downshift_command_emitter_dependency,
    get_prediction_pipeline_dependency,
)
from predictive_qoe_ml_service.pipeline.bandwidth_degradation_prediction_pipeline import (
    BandwidthDegradationPredictionDecision,
)


class _FakePredictionPipeline:
    """Test double standing in for BandwidthDegradationPredictionPipeline."""

    def __init__(self, decision: BandwidthDegradationPredictionDecision) -> None:
        self._decision = decision
        self.last_call_kwargs: dict | None = None

    def predict_imminent_bandwidth_degradation_from_network_samples(self, **kwargs):
        self.last_call_kwargs = kwargs
        return self._decision


@pytest.fixture()
def sample_request_payload() -> dict:
    """A minimal, valid request payload for the prediction endpoint."""
    return {
        "sessionId": "session-abc-123",
        "recentNetworkQualitySamples": [
            {"timestampMs": 1000, "measuredThroughputKbps": 5000.0, "roundTripTimeMs": 30.0},
            {"timestampMs": 2000, "measuredThroughputKbps": 4800.0, "roundTripTimeMs": 32.0},
            {"timestampMs": 3000, "measuredThroughputKbps": 4600.0, "roundTripTimeMs": 35.0},
        ],
        "currentPlayerBufferSeconds": 12.0,
        "currentEncodedBitrateKbps": 4000.0,
    }


def test_health_endpoint_reports_ok():
    """The /health probe should respond 200 with a status field."""
    with TestClient(app) as client:
        response = client.get("/health")
    assert response.status_code == 200
    assert response.json()["status"] == "ok"


def test_predict_endpoint_returns_camelcase_fields_for_non_degrading_prediction(
    sample_request_payload,
):
    """A non-degradation decision should map straight through to the camelCase response."""
    fake_decision = BandwidthDegradationPredictionDecision(
        is_degradation_imminent=False,
        degradation_confidence_score=0.12,
        recommended_downshift_bitrate_kbps=4000.0,
        seconds_before_buffer_empty_estimate=None,
    )
    fake_pipeline = _FakePredictionPipeline(fake_decision)
    fake_emitter = AsyncMock()

    app.dependency_overrides[get_prediction_pipeline_dependency] = lambda: fake_pipeline
    app.dependency_overrides[get_downshift_command_emitter_dependency] = lambda: fake_emitter
    try:
        with TestClient(app) as client:
            response = client.post("/predict/bandwidth-degradation", json=sample_request_payload)
    finally:
        app.dependency_overrides.clear()

    assert response.status_code == 200
    body = response.json()
    assert body == {
        "isDegradationImminent": False,
        "degradationConfidenceScore": 0.12,
        "recommendedDownshiftBitrateKbps": 4000.0,
        "secondsBeforeBufferEmptyEstimate": None,
    }
    fake_emitter.emit_downshift_bitrate_command_for_session.assert_not_called()


def test_predict_endpoint_emits_downshift_command_when_degradation_is_imminent(
    sample_request_payload,
):
    """A positive degradation prediction should trigger the WebSocket command emitter."""
    fake_decision = BandwidthDegradationPredictionDecision(
        is_degradation_imminent=True,
        degradation_confidence_score=0.91,
        recommended_downshift_bitrate_kbps=1500.0,
        seconds_before_buffer_empty_estimate=8.5,
    )
    fake_pipeline = _FakePredictionPipeline(fake_decision)
    fake_emitter = AsyncMock()

    app.dependency_overrides[get_prediction_pipeline_dependency] = lambda: fake_pipeline
    app.dependency_overrides[get_downshift_command_emitter_dependency] = lambda: fake_emitter
    try:
        with TestClient(app) as client:
            response = client.post("/predict/bandwidth-degradation", json=sample_request_payload)
    finally:
        app.dependency_overrides.clear()

    assert response.status_code == 200
    body = response.json()
    assert body["isDegradationImminent"] is True
    assert body["degradationConfidenceScore"] == pytest.approx(0.91)
    assert body["recommendedDownshiftBitrateKbps"] == pytest.approx(1500.0)
    assert body["secondsBeforeBufferEmptyEstimate"] == pytest.approx(8.5)

    fake_emitter.emit_downshift_bitrate_command_for_session.assert_called_once()
    call_kwargs = fake_emitter.emit_downshift_bitrate_command_for_session.call_args.kwargs
    assert call_kwargs["session_id"] == "session-abc-123"
    assert call_kwargs["recommended_bitrate_kbps"] == pytest.approx(1500.0)


def test_predict_endpoint_rejects_request_with_no_samples():
    """The request schema requires at least one network-quality sample."""
    invalid_payload = {"sessionId": "s1", "recentNetworkQualitySamples": []}
    with TestClient(app) as client:
        response = client.post("/predict/bandwidth-degradation", json=invalid_payload)
    assert response.status_code == 422
