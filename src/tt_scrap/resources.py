"""Cancellation-safe blocking I/O and ownership of temporary media files."""

from __future__ import annotations

import asyncio
import io
import logging
import tempfile
from collections.abc import Awaitable, Callable, Iterable
from contextvars import copy_context
from functools import partial
from typing import Any, BinaryIO

from .logging import log_event

logger = logging.getLogger(__name__)


async def await_completion[T](future: asyncio.Future[T]) -> T:
    """Cancellation may stop the caller, but must not leave I/O using its files."""
    try:
        return await asyncio.shield(future)
    except asyncio.CancelledError:
        while not future.done():
            try:
                await asyncio.shield(future)
            except asyncio.CancelledError:
                continue
            except Exception:
                break
        if not future.cancelled():
            future.exception()
        raise


async def run_io[T](function: Callable[..., T], *args: Any, **kwargs: Any) -> T:
    # Submit directly: wrapping to_thread in another Task adds a scheduling turn
    # to every upload chunk. The Future still lets cancellation drain the worker.
    future = asyncio.get_running_loop().run_in_executor(
        None, copy_context().run, partial(function, *args, **kwargs)
    )
    return await await_completion(future)


def memory_file(file: object) -> bool:
    # Only built-in implementations can safely run inline. Subclasses may perform
    # blocking I/O even when they resemble a BytesIO or an unrolled spool.
    return type(file) is io.BytesIO or (
        type(file) is tempfile.SpooledTemporaryFile and not getattr(file, "_rolled", True)
    )


async def read_file(file: BinaryIO, size: int = -1, *, rewind: bool = False) -> bytes:
    def read() -> bytes:
        if rewind:
            file.seek(0)
        data = file.read(size)
        if rewind:
            file.seek(0)
        return data

    return read() if memory_file(file) else await run_io(read)


def _close_files(files: list[BinaryIO]) -> None:
    for file in files:
        try:
            if not file.closed:
                file.close()
        except Exception as exc:
            log_event(
                logger,
                "media.cleanup.failed",
                level=logging.WARNING,
                error_type=type(exc).__name__,
                success=False,
            )


async def close_files(files: Iterable[BinaryIO]) -> None:
    unique = list({id(file): file for file in files}.values())
    if all(memory_file(file) for file in unique):
        _close_files(unique)
    else:
        await run_io(_close_files, unique)


async def ordered_results[T](
    operations: Iterable[Awaitable[T]],
    *,
    dispose: Callable[[list[T]], Awaitable[None]],
) -> list[T | BaseException]:
    """Run concurrently; stop at the first failure in input order, not completion order."""
    tasks = [asyncio.ensure_future(operation) for operation in operations]
    try:
        for task in tasks:
            try:
                await asyncio.shield(task)
            except Exception:
                break
        for task in tasks:
            if not task.done():
                task.cancel()
        return await await_completion(asyncio.gather(*tasks, return_exceptions=True))
    except BaseException:
        await cancel_and_dispose(tasks, dispose=dispose)
        raise


async def cancel_and_dispose(
    tasks: Iterable[asyncio.Future[Any]], *, dispose: Callable[[list[Any]], Awaitable[None]]
) -> None:
    pending = list(tasks)
    # Cancel before yielding: otherwise an unused thumbnail/download can start
    # between entering cleanup and scheduling its worker.
    for task in pending:
        if not task.done():
            task.cancel()

    async def cleanup() -> None:
        results = await asyncio.gather(*pending, return_exceptions=True)
        await dispose([result for result in results if not isinstance(result, BaseException)])

    await await_completion(asyncio.create_task(cleanup()))
