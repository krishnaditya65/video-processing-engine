"""FastAPI application: the predictive-buffer ML microservice's HTTP + WebSocket API.

Exposes:
    POST /predict/bandwidth-degradation
        Runs the full prediction pipeline (feature extraction -> Isolation
        Forest inference -> decisioning) on a session's recent
        network-quality samples, pushes a WebSocket downshift command to
        that session's connected client player socket if degradation is
        predicted, and returns the prediction as JSON.

    WS /ws/session/{session_id}
        The client player connects here to (a) register itself in the
        in-memory session-socket registry so it can receive pushed
        downshift commands, and (b) optionally keep the connection open
        as a heartbeat channel.

    GET /health
        Trivial liveness/readiness probe.

On startup, the app loads a previously trained model from
``config.TRAINED_MODEL_FILE_PATH`` if present; otherwise (e.g. first run
in a fresh checkout before the training harness has been run) it trains
one on the fly using the same synthetic-data harness the training script
uses, so the service is runnable out of the box. Production deployments
should always ship a pre-trained, versioned model artifact rather than
relying on the on-the-fly fallback.
"""

from __future__ import annotations

import logging
import os
from contextlib import asynccontextmanager

from fastapi import Depends, FastAPI, WebSocket, WebSocketDisconnect

from predictive_qoe_ml_service import config
from predictive_qoe_ml_service.api.schemas import (
    BandwidthDegradationPredictionRequestModel,
    BandwidthDegradationPredictionResponseModel,
)
from predictive_qoe_ml_service.feature_engineering.rolling_window_feature_strategies import (
    NetworkQualitySample,
)
from predictive_qoe_ml_service.ml.isolation_forest_bandwidth_degradation_model import (
    IsolationForestBandwidthDegradationModel,
)
from predictive_qoe_ml_service.pipeline.bandwidth_degradation_prediction_pipeline import (
    BandwidthDegradationPredictionPipeline,
)
from predictive_qoe_ml_service.websocket.downshift_command_emitter import (
    BandwidthDegradationDownshiftCommandEmitter,
)
from predictive_qoe_ml_service.websocket.session_socket_registry import (
    GLOBAL_SESSION_SOCKET_REGISTRY,
)

logger = logging.getLogger("predictive_qoe_ml_service")


def _load_or_train_fallback_bandwidth_degradation_model() -> IsolationForestBandwidthDegradationModel:
    """Load the persisted trained model, or train a fallback one on the fly.

    Returns:
        A trained ``IsolationForestBandwidthDegradationModel``.
    """
    if os.path.exists(config.TRAINED_MODEL_FILE_PATH):
        logger.info("Loading trained model from %s", config.TRAINED_MODEL_FILE_PATH)
        return IsolationForestBandwidthDegradationModel.load_trained_model_from_disk(
            config.TRAINED_MODEL_FILE_PATH
        )

    logger.warning(
        "No persisted model found at %s; training a fallback model on the fly "
        "from synthetic data. Run scripts/run_training_harness.py to persist a "
        "model artifact for production use.",
        config.TRAINED_MODEL_FILE_PATH,
    )
    # Imported lazily so a missing scikit-learn/training-only dependency
    # never blocks importing the API module itself in constrained
    # environments; here it is required since we're about to train.
    from predictive_qoe_ml_service.ml.training_evaluation_harness import (
        run_isolation_forest_training_and_evaluation_harness,
    )

    report = run_isolation_forest_training_and_evaluation_harness()
    logger.info(
        "Fallback model trained: precision=%.3f recall=%.3f f1=%.3f",
        report.precision,
        report.recall,
        report.f1_score_value,
    )
    return report.trained_model


@asynccontextmanager
async def _application_lifespan(app: FastAPI):
    """FastAPI lifespan hook: build the prediction pipeline once at startup.

    Args:
        app: The FastAPI application instance.

    Yields:
        Control back to FastAPI while the app serves requests; the
        pipeline built here lives on ``app.state.prediction_pipeline``
        for the lifetime of the process.
    """
    model = _load_or_train_fallback_bandwidth_degradation_model()
    app.state.prediction_pipeline = BandwidthDegradationPredictionPipeline(model=model)
    app.state.session_socket_registry = GLOBAL_SESSION_SOCKET_REGISTRY
    app.state.downshift_command_emitter = BandwidthDegradationDownshiftCommandEmitter(
        session_socket_registry=GLOBAL_SESSION_SOCKET_REGISTRY
    )
    yield


app = FastAPI(
    title="Predictive QoE ML Service",
    description=(
        "Predicts imminent client-side bandwidth-degradation events from "
        "rolling-window network-quality features using an Isolation Forest "
        "anomaly-detection model, and proactively pushes downshift-bitrate "
        "commands to connected client players."
    ),
    version="0.1.0",
    lifespan=_application_lifespan,
)


def get_prediction_pipeline_dependency() -> BandwidthDegradationPredictionPipeline:
    """FastAPI dependency returning the app-wide prediction pipeline.

    Kept as its own function (rather than inlined) specifically so tests
    can override it via ``app.dependency_overrides`` to inject a pipeline
    backed by a mocked model, without spinning up a real Isolation Forest.

    Returns:
        The ``BandwidthDegradationPredictionPipeline`` built at startup.
    """
    return app.state.prediction_pipeline


def get_downshift_command_emitter_dependency() -> BandwidthDegradationDownshiftCommandEmitter:
    """FastAPI dependency returning the app-wide downshift-command emitter.

    Returns:
        The ``BandwidthDegradationDownshiftCommandEmitter`` built at
        startup.
    """
    return app.state.downshift_command_emitter


@app.get("/health")
def get_service_health_status() -> dict:
    """Trivial liveness/readiness probe endpoint.

    Returns:
        A small JSON payload confirming the service is up.
    """
    return {"status": "ok", "service": "predictive-qoe-ml-service"}


@app.post(
    "/predict/bandwidth-degradation",
    response_model=BandwidthDegradationPredictionResponseModel,
    response_model_by_alias=True,
)
async def predict_bandwidth_degradation_endpoint(
    request_body: BandwidthDegradationPredictionRequestModel,
    prediction_pipeline: BandwidthDegradationPredictionPipeline = Depends(
        get_prediction_pipeline_dependency
    ),
    downshift_command_emitter: BandwidthDegradationDownshiftCommandEmitter = Depends(
        get_downshift_command_emitter_dependency
    ),
) -> BandwidthDegradationPredictionResponseModel:
    """Predict imminent bandwidth degradation for one session.

    Runs the full raw-samples -> features -> model -> decision pipeline,
    and, when degradation is predicted, pushes a
    ``DOWNSHIFT_BITRATE_COMMAND`` WebSocket message to this session's
    connected client player socket (if one is currently registered; a
    missing socket is a normal no-op, not an error).

    Args:
        request_body: The session id, recent network-quality samples, and
            optional current buffer/bitrate context.
        prediction_pipeline: Injected prediction pipeline (overridable in
            tests).
        downshift_command_emitter: Injected WebSocket command emitter
            (overridable in tests).

    Returns:
        The prediction, serialized with camelCase field aliases.
    """
    samples = [
        NetworkQualitySample(
            timestamp_ms=sample.timestamp_ms,
            measured_throughput_kbps=sample.measured_throughput_kbps,
            round_trip_time_ms=sample.round_trip_time_ms,
        )
        for sample in request_body.recent_network_quality_samples
    ]

    decision = prediction_pipeline.predict_imminent_bandwidth_degradation_from_network_samples(
        samples=samples,
        current_player_buffer_seconds=request_body.current_player_buffer_seconds,
        current_encoded_bitrate_kbps=request_body.current_encoded_bitrate_kbps,
    )

    if decision.is_degradation_imminent:
        await downshift_command_emitter.emit_downshift_bitrate_command_for_session(
            session_id=request_body.session_id,
            recommended_bitrate_kbps=decision.recommended_downshift_bitrate_kbps,
            degradation_confidence_score=decision.degradation_confidence_score,
            seconds_before_buffer_empty_estimate=decision.seconds_before_buffer_empty_estimate,
        )

    return BandwidthDegradationPredictionResponseModel(
        is_degradation_imminent=decision.is_degradation_imminent,
        degradation_confidence_score=decision.degradation_confidence_score,
        recommended_downshift_bitrate_kbps=decision.recommended_downshift_bitrate_kbps,
        seconds_before_buffer_empty_estimate=decision.seconds_before_buffer_empty_estimate,
    )


@app.websocket("/ws/session/{session_id}")
async def session_player_websocket_endpoint(websocket: WebSocket, session_id: str) -> None:
    """WebSocket endpoint client players connect to for pushed downshift commands.

    On connect, registers this socket in the in-memory session-socket
    registry under ``session_id`` (Repository/Registry pattern). On
    disconnect, unregisters it. The connection is otherwise passive from
    the server's perspective -- commands are pushed to it asynchronously
    by ``predict_bandwidth_degradation_endpoint`` via the downshift-command
    emitter, not in response to any message the client sends here.

    Args:
        websocket: The Starlette WebSocket connection.
        session_id: The streaming session identifier this socket belongs
            to, taken from the URL path.
    """
    await websocket.accept()
    registry = app.state.session_socket_registry
    await registry.register_session_socket(session_id, websocket)
    try:
        while True:
            # The endpoint is push-only from the server's side; we still
            # await incoming frames so we notice a client-initiated close.
            await websocket.receive_text()
    except WebSocketDisconnect:
        pass
    finally:
        await registry.unregister_session_socket(session_id)
