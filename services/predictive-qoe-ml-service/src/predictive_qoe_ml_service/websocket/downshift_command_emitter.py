"""Server-side push mechanism for downshift-bitrate commands.

Upon a positive bandwidth-degradation prediction, the prediction service
needs to proactively push a structured command to the specific client
player socket for that session, telling it to preemptively downshift its
requested bitrate before its buffer empties. This module implements that
push mechanism using FastAPI/Starlette's native WebSocket support (the
``send_text`` interface both a Starlette ``WebSocket`` and a
``websockets``-library connection expose), looking up the destination
socket via the ``InMemorySessionSocketRegistry`` (Repository pattern; see
``session_socket_registry.py``).

Message schema (JSON, sent as a WebSocket text frame):

    {
        "messageType": "DOWNSHIFT_BITRATE_COMMAND",
        "sessionId": "<string>",
        "recommendedBitrateKbps": <number>,
        "degradationConfidenceScore": <number 0..1>,
        "secondsBeforeBufferEmptyEstimate": <number|null>,
        "issuedAtEpochMs": <integer>
    }

Note on field naming: the wire message uses camelCase field names
(``messageType``, ``sessionId``, ...) to match this platform's general
camelCase JSON/API convention, even though the Python code implementing
it is idiomatic snake_case -- see the README's "Naming Convention"
section for the full rationale.
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass

from predictive_qoe_ml_service.websocket.session_socket_registry import (
    InMemorySessionSocketRegistry,
)

#: The literal ``messageType`` value used for downshift-bitrate command
#: messages, distinguishing them from any other message types this
#: WebSocket channel might carry in the future.
DOWNSHIFT_BITRATE_COMMAND_MESSAGE_TYPE = "DOWNSHIFT_BITRATE_COMMAND"


@dataclass(frozen=True)
class DownshiftBitrateCommandMessage:
    """The structured message pushed to a client player upon predicted degradation.

    Attributes:
        session_id: The streaming session this command applies to.
        recommended_bitrate_kbps: The bitrate, in kbps, the client should
            proactively switch down to.
        degradation_confidence_score: The model's confidence, in [0, 1],
            that a degradation event is imminent.
        seconds_before_buffer_empty_estimate: Estimated seconds until the
            client's buffer would empty if it takes no action, or
            ``None`` if unavailable.
        issued_at_epoch_ms: Epoch milliseconds at which this command was
            generated, so the client can reason about message latency.
    """

    session_id: str
    recommended_bitrate_kbps: float
    degradation_confidence_score: float
    seconds_before_buffer_empty_estimate: float | None
    issued_at_epoch_ms: int

    def to_wire_json(self) -> str:
        """Serialize this message to the camelCase wire JSON schema.

        Returns:
            A JSON string matching the module-level documented schema.
        """
        payload = {
            "messageType": DOWNSHIFT_BITRATE_COMMAND_MESSAGE_TYPE,
            "sessionId": self.session_id,
            "recommendedBitrateKbps": self.recommended_bitrate_kbps,
            "degradationConfidenceScore": self.degradation_confidence_score,
            "secondsBeforeBufferEmptyEstimate": self.seconds_before_buffer_empty_estimate,
            "issuedAtEpochMs": self.issued_at_epoch_ms,
        }
        return json.dumps(payload)

    def to_dict(self) -> dict:
        """Return this message as a plain (snake_case) dict, for logging/tests.

        Returns:
            The dataclass fields as a dict, with Python-native snake_case
            keys (contrast with ``to_wire_json``'s camelCase keys).
        """
        return asdict(self)


class BandwidthDegradationDownshiftCommandEmitter:
    """Pushes ``DownshiftBitrateCommandMessage`` frames to connected client sockets.

    Looks up the destination socket for a session via an
    ``InMemorySessionSocketRegistry`` and sends the serialized command as
    a WebSocket text frame. Silently no-ops (rather than raising) when no
    socket is currently registered for the session, since a client may
    simply not have an active WebSocket connection at prediction time
    (e.g. it only polls the HTTP endpoint) -- that is an expected,
    non-exceptional condition, not a bug.
    """

    def __init__(self, session_socket_registry: InMemorySessionSocketRegistry) -> None:
        """Configure the emitter with the registry it should look up sockets in.

        Args:
            session_socket_registry: The Repository/Registry instance
                holding currently connected client sockets.
        """
        self._session_socket_registry = session_socket_registry

    async def emit_downshift_bitrate_command_for_session(
        self,
        session_id: str,
        recommended_bitrate_kbps: float,
        degradation_confidence_score: float,
        seconds_before_buffer_empty_estimate: float | None,
    ) -> bool:
        """Build and push a downshift command to ``session_id``'s socket, if connected.

        Args:
            session_id: The streaming session to push the command to.
            recommended_bitrate_kbps: The recommended downshift target
                bitrate, in kbps.
            degradation_confidence_score: The model's confidence in [0, 1].
            seconds_before_buffer_empty_estimate: Estimated seconds until
                buffer-empty, or ``None``.

        Returns:
            True if a socket was registered for this session and the
            message was sent to it; False if no socket was registered
            (no-op).
        """
        socket = await self._session_socket_registry.get_registered_socket_for_session(
            session_id
        )
        if socket is None:
            return False

        message = DownshiftBitrateCommandMessage(
            session_id=session_id,
            recommended_bitrate_kbps=recommended_bitrate_kbps,
            degradation_confidence_score=degradation_confidence_score,
            seconds_before_buffer_empty_estimate=seconds_before_buffer_empty_estimate,
            issued_at_epoch_ms=int(time.time() * 1000),
        )
        await socket.send_text(message.to_wire_json())
        return True
