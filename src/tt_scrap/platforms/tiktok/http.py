"""Reusable HTTP clients that drain failed proxy pools without aborting other users."""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass

import httpx

from ...logging import log_event

logger = logging.getLogger(__name__)


@dataclass(eq=False, slots=True)
class _ClientLease:
    client: httpx.AsyncClient
    users: int = 0
    retired: bool = False


class ResolverClients:
    def __init__(self, factory: Callable[[str | None], httpx.AsyncClient]) -> None:
        self._factory = factory
        self._current: dict[str | None, _ClientLease] = {}
        self._leases: set[_ClientLease] = set()
        self._closing: set[asyncio.Task[None]] = set()
        self._closed = False

    async def _close(self, lease: _ClientLease) -> None:
        if lease not in self._leases:
            return
        self._leases.discard(lease)

        async def close_client() -> None:
            try:
                await lease.client.aclose()
            except Exception as exc:
                log_event(
                    logger,
                    "tiktok.resolver_client.close_failed",
                    level=logging.WARNING,
                    error_type=type(exc).__name__,
                    success=False,
                )

        task = asyncio.create_task(close_client())
        self._closing.add(task)
        task.add_done_callback(self._closing.discard)
        # A second caller cancellation must not interrupt socket cleanup.
        await asyncio.shield(task)

    @asynccontextmanager
    async def acquire(self, proxy: str | None) -> AsyncIterator[httpx.AsyncClient]:
        if self._closed:
            raise RuntimeError("Resolver clients are closed")
        lease = self._current.get(proxy)
        if lease is None:
            lease = _ClientLease(self._factory(proxy))
            self._current[proxy] = lease
            self._leases.add(lease)
        lease.users += 1
        try:
            yield lease.client
        except (httpx.TransportError, asyncio.CancelledError) as exc:
            # httpcore can retain an ACTIVE CONNECT tunnel after a failed TLS
            # handshake. New requests must not reuse that pool. Existing users
            # keep their lease, including successful concurrent requests.
            if not lease.retired:
                log_event(
                    logger,
                    "tiktok.resolver_client.retired",
                    level=logging.WARNING,
                    message="Failed resolver pool retired; active requests will drain",
                    error_type=type(exc).__name__,
                    proxy_used=proxy is not None,
                    inflight_requests=lease.users,
                    success=False,
                )
                lease.retired = True
            if self._current.get(proxy) is lease:
                del self._current[proxy]
            raise
        finally:
            lease.users -= 1
            if lease.retired and lease.users == 0 and lease in self._leases:
                await self._close(lease)

    async def close(self) -> None:
        self._closed = True
        self._current.clear()
        await asyncio.gather(*(self._close(lease) for lease in list(self._leases)))
        if self._closing:
            await asyncio.gather(*self._closing)
