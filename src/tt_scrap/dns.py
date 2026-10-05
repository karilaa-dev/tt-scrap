"""Recover event-loop DNS stalls without replaying HTTP requests."""

from __future__ import annotations

import asyncio
import logging
import socket
from concurrent.futures import ThreadPoolExecutor
from functools import partial
from types import TracebackType

from .logging import log_event

logger = logging.getLogger(__name__)

type AddressInfo = tuple[
    socket.AddressFamily,
    socket.SocketKind,
    int,
    str,
    tuple[str, int] | tuple[str, int, int, int] | tuple[int, bytes],
]
type Lookup = tuple[bytes | str | None, bytes | str | int | None, int, int, int, int]


class DNSRecovery:
    def __init__(
        self, *, fallback_after: float = 1.0, cooldown: float = 60.0, workers: int = 4
    ) -> None:
        self._loop = asyncio.get_running_loop()
        self._native = self._loop.getaddrinfo
        self._fallback_after = fallback_after
        self._cooldown = cooldown
        self._fallback_until = 0.0
        self._reported_success = False
        self._workers = workers
        self._executor = ThreadPoolExecutor(max_workers=workers, thread_name_prefix="dns-recovery")
        self._slots = asyncio.Semaphore(workers)
        self._lookups: dict[Lookup, asyncio.Future[list[AddressInfo]]] = {}
        self._closed = False

    def __enter__(self) -> DNSRecovery:
        # Both AnyIO/httpx and aiohttp's ThreadedResolver call this public loop
        # method. Keep hostnames in HTTP requests so proxy routing and TLS SNI
        # remain intact. Restore the hook when the application lifespan ends.
        self._loop.getaddrinfo = self.getaddrinfo  # type: ignore[method-assign]
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self._closed = True
        if self._loop.getaddrinfo == self.getaddrinfo:
            self._loop.getaddrinfo = self._native  # type: ignore[method-assign]
        self._executor.shutdown(wait=False, cancel_futures=True)

    async def getaddrinfo(
        self,
        host: bytes | str | None,
        port: bytes | str | int | None,
        *,
        family: int = 0,
        type: int = 0,
        proto: int = 0,
        flags: int = 0,
    ) -> list[AddressInfo]:
        if self._closed:
            raise RuntimeError("DNS recovery is closed")
        episode = self._fallback_until
        if self._loop.time() >= episode:
            try:
                async with asyncio.timeout(self._fallback_after):
                    result = await self._native(
                        host, port, family=family, type=type, proto=proto, flags=flags
                    )
            except TimeoutError:
                if self._loop.time() >= self._fallback_until:
                    self._fallback_until = self._loop.time() + self._cooldown
                    self._reported_success = False
                    log_event(
                        logger,
                        "runtime.dns.fallback_started",
                        level=logging.WARNING,
                        message="Event-loop DNS stalled; using dedicated resolver workers",
                        wait_seconds=self._fallback_after,
                        worker_count=self._workers,
                    )
            else:
                if episode and self._fallback_until == episode:
                    self._fallback_until = 0.0
                    log_event(logger, "runtime.dns.native_recovered", success=True)
                return result

        result = await self._fallback((host, port, family, type, proto, flags))
        if not self._reported_success:
            self._reported_success = True
            log_event(logger, "runtime.dns.fallback_succeeded", success=True)
        return result

    async def _fallback(self, key: Lookup) -> list[AddressInfo]:
        future = self._lookups.get(key)
        if future is None:
            await self._slots.acquire()
            # Another caller may have submitted the same lookup while we waited.
            future = self._lookups.get(key)
            if future is not None:
                self._slots.release()
            else:
                try:
                    if self._closed:
                        raise RuntimeError("DNS recovery is closed")
                    future = self._loop.run_in_executor(self._executor, socket.getaddrinfo, *key)
                except BaseException:
                    self._slots.release()
                    raise
                self._lookups[key] = future

                future.add_done_callback(partial(self._finished, key))
        # A caller timing out cannot stop libc or free its occupied worker slot.
        # Keep identical lookups shared until the actual worker completes.
        try:
            return await asyncio.shield(future)
        finally:
            # Already-complete futures can raise without yielding to their done
            # callback. Do not let the next request inherit that completed error.
            if future.done():
                self._finished(key, future)

    def _finished(self, key: Lookup, future: asyncio.Future[list[AddressInfo]]) -> None:
        if self._lookups.get(key) is future:
            del self._lookups[key]
            self._slots.release()
        if not future.cancelled():
            future.exception()
