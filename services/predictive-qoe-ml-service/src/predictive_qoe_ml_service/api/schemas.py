"""Pydantic request/response schemas for the public HTTP API.

Naming convention note: field names on these schemas are deliberately
camelCase (via each field's ``alias``) to match this platform's general
JSON API camelCase philosophy, even though the Python attribute names
themselves are snake_case per PEP 8 -- see the README's "Naming
Convention" section.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class NetworkQualitySampleRequestModel(BaseModel):
    """One raw network-quality sample as received over the HTTP API.

    Attributes:
        timestamp_ms: Epoch milliseconds at which the sample was measured.
        measured_throughput_kbps: Instantaneous measured throughput in
            kilobits per second.
        round_trip_time_ms: Round-trip time in milliseconds.
    """

    model_config = ConfigDict(populate_by_name=True)

    timestamp_ms: int = Field(..., alias="timestampMs")
    measured_throughput_kbps: float = Field(..., alias="measuredThroughputKbps")
    round_trip_time_ms: float = Field(..., alias="roundTripTimeMs")


class BandwidthDegradationPredictionRequestModel(BaseModel):
    """Request body for ``POST /predict/bandwidth-degradation``.

    Attributes:
        session_id: The streaming session's unique identifier, used to
            route the WebSocket downshift command if degradation is
            predicted and a client socket is currently registered.
        recent_network_quality_samples: The session's recent
            network-quality samples, ascending by timestamp. Must contain
            at least one sample; more samples (recommended: 10+) produce
            more meaningful rolling-window features.
        current_player_buffer_seconds: The client's current playback
            buffer level in seconds, if known. Enables
            ``secondsBeforeBufferEmptyEstimate`` in the response.
        current_encoded_bitrate_kbps: The client's current playback
            bitrate in kbps, if known. Improves the recommended downshift
            bitrate and the buffer-empty estimate.
    """

    model_config = ConfigDict(populate_by_name=True)

    session_id: str = Field(..., alias="sessionId")
    recent_network_quality_samples: list[NetworkQualitySampleRequestModel] = Field(
        ..., alias="recentNetworkQualitySamples", min_length=1
    )
    current_player_buffer_seconds: float | None = Field(
        default=None, alias="currentPlayerBufferSeconds"
    )
    current_encoded_bitrate_kbps: float | None = Field(
        default=None, alias="currentEncodedBitrateKbps"
    )


class BandwidthDegradationPredictionResponseModel(BaseModel):
    """Response body for ``POST /predict/bandwidth-degradation``.

    Attributes:
        is_degradation_imminent: Whether the model predicts an imminent
            bandwidth-degradation event for this session.
        degradation_confidence_score: Confidence in [0, 1].
        recommended_downshift_bitrate_kbps: Recommended target bitrate, in
            kbps, for the client to proactively switch to.
        seconds_before_buffer_empty_estimate: Estimated seconds until the
            client's buffer empties, or ``None`` if not computable.
    """

    model_config = ConfigDict(populate_by_name=True)

    is_degradation_imminent: bool = Field(..., alias="isDegradationImminent")
    degradation_confidence_score: float = Field(..., alias="degradationConfidenceScore")
    recommended_downshift_bitrate_kbps: float = Field(
        ..., alias="recommendedDownshiftBitrateKbps"
    )
    seconds_before_buffer_empty_estimate: float | None = Field(
        default=None, alias="secondsBeforeBufferEmptyEstimate"
    )
