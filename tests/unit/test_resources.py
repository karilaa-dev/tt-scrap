from __future__ import annotations

import asyncio
import io
import threading

import pytest

from tt_scrap.resources import close_files, ordered_results, run_io


async def test_cancelled_io_finishes_before_cleanup_even_with_repeated_cancellation():
    started, release = threading.Event(), threading.Event()
    order = []

    def write():
        started.set()
        assert release.wait(2)
        order.append("write finished")

    async def operation():
        try:
            await run_io(write)
        finally:
            order.append("cleanup")

    task = asyncio.create_task(operation())
    try:
        assert await asyncio.to_thread(started.wait, 1)
        task.cancel()
        await asyncio.sleep(0)
        task.cancel()
        await asyncio.sleep(0)
        assert order == []
        assert not task.done()
    finally:
        release.set()
        with pytest.raises(asyncio.CancelledError):
            await task
    assert order == ["write finished", "cleanup"]


async def test_cleanup_is_unique_and_does_not_replace_an_outcome(log_records):
    class File(io.BytesIO):
        calls = 0

        def close(self):
            self.calls += 1
            super().close()
            raise ValueError("I/O operation on closed file")

    file = File(b"media")
    await close_files([file, file])
    await close_files([file])
    assert file.calls == 1
    assert [record.event for record in log_records] == ["media.cleanup.failed"]


async def test_ordered_collection_preserves_error_order_and_cancels_unneeded_work():
    second_failed, cancelled, release = asyncio.Event(), asyncio.Event(), asyncio.Event()

    async def first():
        await release.wait()
        raise ValueError("first input")

    async def second():
        second_failed.set()
        raise RuntimeError("second input")

    async def third():
        try:
            await asyncio.Future()
        finally:
            cancelled.set()

    async def dispose(values):
        assert not values

    task = asyncio.create_task(ordered_results([first(), second(), third()], dispose=dispose))
    await second_failed.wait()
    assert not task.done()
    release.set()
    results = await asyncio.wait_for(task, 0.2)
    assert str(results[0]) == "first input"
    assert str(results[1]) == "second input"
    assert isinstance(results[2], asyncio.CancelledError)
    assert cancelled.is_set()


async def test_cancelled_collection_disposes_completed_results():
    completed = asyncio.Event()
    file = io.BytesIO(b"media")

    async def first():
        completed.set()
        return file

    async def second():
        await asyncio.Future()

    task = asyncio.create_task(ordered_results([first(), second()], dispose=close_files))
    await completed.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert file.closed
