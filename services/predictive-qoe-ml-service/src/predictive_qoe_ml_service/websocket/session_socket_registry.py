"""In-memory session-socket registry (Repository/Registry pattern).

Design pattern -- Repository/Registry:
    This module defines a single narrow abstraction --
    ``InMemorySessionSocketRegistry`` -- responsible only for mapping a
    ``session_id`` to its currently-connected client player WebSocket
    connection, and nothing else. Callers (the FastAPI WebSocket route,
    the downshift-command emitter) depend only on this abstraction's
    register/unregister/lookup interface, not on how connections are
    stored. That isolation is what lets the storage backend be swapped
    later (e.g. to a Redis-backed registry for multi-process/multi-node
    deployments) without changing any calling code -- exactly the
    Repository pattern's purpose.

Concurrency note: this registry is a plain in-process dict guarded by an
``asyncio.Lock``, appropriate for a single-process FastAPI/uvicorn worker.
In a horizontally scaled, multi-process/multi-node deployment (the
realistic Netflix-tier target), the session-to-socket mapping only holds
within one process, so a real deployment would back this same interface
with a shared store (e.g. Redis) or route degradation commands through a
message bus keyed by session_id so any node holding that session's socket
can deliver them. That production concern is intentionally out of scope
for this in-memory reference implementation, and is called out again in
the README.
"""

from __future__ import annotations

import asyncio
from typing import Protocol


class SupportsSendingTextMessages(Protocol):
    """Structural type for any socket-like object this registry can hold.

    Both FastAPI's ``starlette.websockets.WebSocket`` and the third-party
    ``websockets`` library's connection objects satisfy this protocol via
    an async ``send`` or ``send_text`` method, and test doubles can
    trivially implement it too -- callers should depend on this Protocol,
    not on a concrete WebSocket class.
    """

    async def send_text(self, data: str) -> None:
        """Send a text frame to the connected client."""
        ...


class InMemorySessionSocketRegistry:
    """A process-local Repository mapping session_id -> connected client socket.

    Thread/async-safety: all mutating operations acquire an internal
    ``asyncio.Lock`` so concurrent connect/disconnect events from
    different asyncio tasks cannot corrupt the underlying dict.
    """

    def __init__(self) -> None:
        """Initialize an empty registry."""
        self._session_id_to_socket: dict[str, SupportsSendingTextMessages] = {}
        self._lock = asyncio.Lock()

    async def register_session_socket(
        self, session_id: str, socket: SupportsSendingTextMessages
    ) -> None:
        """Register (or replace) the connected socket for ``session_id``.

        Args:
            session_id: The unique streaming-session identifier the
                client player authenticated/announced itself with.
            socket: The connected socket-like object to associate with
                this session. If a socket is already registered for this
                session_id, it is silently replaced (e.g. on client
                reconnect).
        """
        async with self._lock:
            self._session_id_to_socket[session_id] = socket

    async def unregister_session_socket(self, session_id: str) -> None:
        """Remove any registered socket for ``session_id``, if present.

        Safe to call even if no socket is currently registered for this
        session_id (e.g. duplicate disconnect events); this is a no-op in
        that case.

        Args:
            session_id: The streaming-session identifier to unregister.
        """
        async with self._lock:
            self._session_id_to_socket.pop(session_id, None)

    async def get_registered_socket_for_session(
        self, session_id: str
    ) -> SupportsSendingTextMessages | None:
        """Look up the currently registered socket for ``session_id``.

        Args:
            session_id: The streaming-session identifier to look up.

        Returns:
            The registered socket, or ``None`` if no socket is currently
            registered for this session (e.g. the client disconnected or
            never connected).
        """
        async with self._lock:
            return self._session_id_to_socket.get(session_id)

    async def get_registered_session_count(self) -> int:
        """Return how many sessions currently have a registered socket.

        Returns:
            The count of currently registered session sockets.
        """
        async with self._lock:
            return len(self._session_id_to_socket)


#: A single process-wide registry instance shared by the FastAPI
#: WebSocket route and the downshift-command emitter. Constructed once at
#: import time; see ``api/main.py`` for how it is wired into the app.
GLOBAL_SESSION_SOCKET_REGISTRY = InMemorySessionSocketRegistry()
