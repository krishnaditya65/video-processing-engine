"""WebSocket package.

Contains the in-memory session-socket Registry/Repository
(``session_socket_registry``) and the downshift-command emitter
(``downshift_command_emitter``) that pushes structured "downshift
bitrate" commands to connected client player sockets upon a positive
degradation prediction.
"""

from predictive_qoe_ml_service.websocket.session_socket_registry import (
    InMemorySessionSocketRegistry,
)
from predictive_qoe_ml_service.websocket.downshift_command_emitter import (
    DownshiftBitrateCommandMessage,
    BandwidthDegradationDownshiftCommandEmitter,
)

__all__ = [
    "InMemorySessionSocketRegistry",
    "DownshiftBitrateCommandMessage",
    "BandwidthDegradationDownshiftCommandEmitter",
]
