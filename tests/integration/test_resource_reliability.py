from __future__ import annotations

import asyncio
import tempfile
from contextlib import asynccontextmanager

import pytest
from aiohttp import web
from pydantic import SecretStr

from tt_scrap.cache import CacheStore
from tt_scrap.errors import NetworkError, UpstreamTimeoutError
from tt_scrap.media import AssetDownloader
from tt_scrap.models import AssetFetchContext
from tt_scrap.platforms.instagram import InstagramService
from tt_scrap.platforms.tiktok.http import ResolverClients
from tt_scrap.platforms.tiktok.service import TikTokService
from tt_scrap.proxy import ProxyManager
from tt_scrap.telegram import TelegramClient, TelegramUpload


@asynccontextmanager
async def telegram_server():
    received = []

    async def handler(request):
        parts = {}
        reader = await request.multipart()
        while part := await reader.next():
            parts[part.name] = bytes(await part.read())
        received.append(parts)
        return web.Response(body=b'{"ok":true,"result":[]}', content_type="application/json")

    app = web.Application()
    app.router.add_post("/botlocal-test/sendDocument", handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    try:
        yield f"http://127.0.0.1:{port}", received
    finally:
        await runner.cleanup()


@pytest.mark.parametrize("disk", [False, True])
async def test_multipart_borrows_spool_without_rollover_or_close(settings, disk, monkeypatch):
    data = b"media-data" * 100_000
    spool = tempfile.SpooledTemporaryFile(max_size=1024 if disk else 1024 * 1024, mode="w+b")
    spool.write(data)
    spool.seek(0)
    read_sizes = []
    original_read = tempfile.SpooledTemporaryFile.read

    def read(file, amount=-1):
        read_sizes.append(amount)
        return original_read(file, amount)

    monkeypatch.setattr(tempfile.SpooledTemporaryFile, "read", read)
    async with telegram_server() as (url, received):
        client = TelegramClient(
            settings.model_copy(
                update={
                    "telegram_api_base_url": url,
                    "telegram_bot_token": SecretStr("local-test"),
                }
            )
        )
        try:
            result = await client.call(
                "sendDocument",
                {"chat_id": 123},
                [
                    TelegramUpload("document", spool, "test.bin", "application/octet-stream"),
                ],
            )
            assert result.body == b'{"ok":true,"result":[]}'
            assert received[0]["document"] == data
            assert spool._rolled is disk
            assert not spool.closed, "the delivery service owns this spool"
            assert max(read_sizes) == 64 * 1024
            assert all(0 < size <= 64 * 1024 for size in read_sizes)
        finally:
            await client.close()
            spool.close()


async def test_album_waiter_does_not_reserve_unrelated_download_capacity(settings):
    settings.download_concurrency = 2
    settings.slideshow_concurrency = 1
    downloader = AssetDownloader(settings, ProxyManager())
    album_started, other_started, release = asyncio.Event(), asyncio.Event(), asyncio.Event()

    async def download_once(context, proxy, spool, upstream_url, **kwargs):
        if context.extraction_id == "album":
            album_started.set()
            await release.wait()
        else:
            other_started.set()
        spool.write(b"media")
        return "image/jpeg", 5, None, 5, b"media"

    downloader._download_once = download_once

    def context(index, group):
        return AssetFetchContext(
            platform="instagram",
            upstream_url=f"https://cdn.test/{index}",
            filename="media.jpg",
            kind="image",
            extraction_id=group,
        )

    tasks = []
    try:
        tasks.append(asyncio.create_task(downloader.download(context(1, "album"))))
        await album_started.wait()
        tasks.append(asyncio.create_task(downloader.download(context(2, "album"))))
        await asyncio.sleep(0)
        tasks.append(asyncio.create_task(downloader.download(context(3, "other"))))
        await asyncio.wait_for(other_started.wait(), 0.2)
    finally:
        release.set()
        results = await asyncio.gather(*tasks, return_exceptions=True)
        for result in results:
            if hasattr(result, "file"):
                result.file.close()
        await downloader.close()


async def test_resolver_deadline_does_not_wait_for_pool_cleanup(settings):
    settings.url_resolve_timeout_seconds = 0.02
    cache = CacheStore(60, 100)
    service = TikTokService(settings, cache, ProxyManager())
    closing, release, closed = asyncio.Event(), asyncio.Event(), asyncio.Event()

    class SlowClient:
        async def aclose(self):
            closing.set()
            await release.wait()
            closed.set()

    service.adapter._clients = ResolverClients(lambda proxy: SlowClient())

    async def pending(*args):
        await asyncio.Future()

    service.adapter._follow_tiktok_redirects = pending
    request = asyncio.create_task(service.resolve_url("https://vt.tiktok.com/test/"))
    try:
        await asyncio.wait_for(closing.wait(), 0.2)
        done, _ = await asyncio.wait([request], timeout=0.1)
        assert request in done, "socket cleanup extended the resolver deadline"
        with pytest.raises(UpstreamTimeoutError):
            await request
        assert not closed.is_set()
    finally:
        release.set()
        await asyncio.gather(request, return_exceptions=True)
        await service.close()
    assert closed.is_set()


async def test_instagram_waiters_share_failure_but_later_request_retries(settings):
    import httpx

    service = InstagramService(settings, CacheStore(60, 100))
    started, release = asyncio.Event(), asyncio.Event()
    calls = 0

    async def provider(request):
        nonlocal calls
        calls += 1
        started.set()
        await release.wait()
        if calls <= settings.instagram_max_attempts:
            return httpx.Response(503)
        return httpx.Response(
            200, json={"media": [{"type": "image", "url": "https://cdn.test/photo"}]}
        )

    await service._http.aclose()
    service._http = httpx.AsyncClient(transport=httpx.MockTransport(provider))
    tasks = [asyncio.create_task(service.extract_url("https://www.instagram.com/p/test/"))]
    try:
        await started.wait()
        tasks += [
            asyncio.create_task(service.extract_url("https://www.instagram.com/p/test/"))
            for _ in range(5)
        ]
        await asyncio.sleep(0)
        release.set()
        results = await asyncio.gather(*tasks, return_exceptions=True)
        assert all(isinstance(result, NetworkError) for result in results)
        assert calls == settings.instagram_max_attempts
        result = await service.extract_url("https://www.instagram.com/p/test/")
        assert result.source_id == "test"
        assert len(result.media) == 1
        assert calls == settings.instagram_max_attempts + 1
        assert await service.extract_url("https://www.instagram.com/p/test/") == result
        assert calls == settings.instagram_max_attempts + 1
    finally:
        release.set()
        await asyncio.gather(*tasks, return_exceptions=True)
        await service.close()


@pytest.mark.parametrize("early_response", [False, True])
async def test_multipart_drains_slow_reader_before_return_or_cancellation(settings, early_response):
    import io
    import threading

    from tt_scrap.resources import close_files

    entered, release = threading.Event(), threading.Event()
    finished = threading.Event()

    class SlowFile(io.BytesIO):
        def read(self, size=-1):
            entered.set()
            assert release.wait(2)
            data = super().read(size)
            finished.set()
            return data

        def close(self):
            assert not entered.is_set() or finished.is_set(), "closed while reading"
            super().close()

    async def handler(request):
        if early_response:
            assert await asyncio.to_thread(entered.wait, 1)
            return web.Response(body=b'{"ok":false,"description":"rejected"}', status=400)
        await request.read()
        return web.Response(body=b'{"ok":true}')

    app = web.Application()
    app.router.add_post("/botlocal-test/sendDocument", handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    settings.telegram_api_base_url = f"http://127.0.0.1:{port}"
    settings.telegram_bot_token = SecretStr("local-test")
    client = TelegramClient(settings)
    file = SlowFile(b"media")

    async def upload():
        try:
            return await client.call(
                "sendDocument",
                {},
                [
                    TelegramUpload("document", file, "test.bin", "application/octet-stream", 5),
                ],
            )
        finally:
            await close_files([file])

    task = asyncio.create_task(upload())
    try:
        assert await asyncio.to_thread(entered.wait, 1)
        if not early_response:
            task.cancel()
            await asyncio.sleep(0)
            task.cancel()
        await asyncio.sleep(0.03)
        assert not file.closed
        assert not task.done()
        release.set()
        if early_response:
            response = await asyncio.wait_for(task, 1)
            assert response.status_code == 400
            assert response.body == b'{"ok":false,"description":"rejected"}'
        else:
            with pytest.raises(asyncio.CancelledError):
                await task
        assert file.closed
    finally:
        release.set()
        await asyncio.gather(task, return_exceptions=True)
        await client.close()
        await runner.cleanup()


@pytest.mark.parametrize("delay_headers", [True, False])
async def test_relay_album_waiter_leaves_capacity_for_other_request(settings, delay_headers):
    from tt_scrap.media.downloader import _OpenStream

    settings.download_concurrency = 2
    settings.slideshow_concurrency = 1
    downloader = AssetDownloader(settings, ProxyManager())
    started, other_started, release = asyncio.Event(), asyncio.Event(), asyncio.Event()
    active = peak = closed = 0

    async def open_stream(context, *args):
        async def chunks():
            nonlocal active, peak
            if not delay_headers:
                yield b"media"
            active += 1
            peak = max(peak, active)
            try:
                if context.extraction_id == "album":
                    started.set()
                    await release.wait()
                else:
                    other_started.set()
            finally:
                active -= 1
            yield b"media"

        async def close():
            nonlocal closed
            closed += 1

        return _OpenStream(
            chunks(),
            200,
            {"content-length": "5" if delay_headers else "10", "content-type": "video/mp4"},
            close,
        )

    downloader._open_stream = open_stream

    async def relay(group):
        context = AssetFetchContext(
            platform="instagram",
            upstream_url="https://cdn.test/video",
            filename="video.mp4",
            kind="video",
            extraction_id=group,
        )
        async with downloader.stream(context) as streamed:
            assert b"".join([chunk async for chunk in streamed.chunks]) == b"media" * (
                1 if delay_headers else 2
            )

    tasks = [asyncio.create_task(relay("album"))]
    try:
        await started.wait()
        tasks += [asyncio.create_task(relay("album")) for _ in range(3)]
        await asyncio.sleep(0)
        tasks.append(asyncio.create_task(relay("other")))
        await asyncio.wait_for(other_started.wait(), 0.2)
    finally:
        release.set()
        await asyncio.gather(*tasks)
        await downloader.close()
    assert peak <= 2
    assert active == 0
    assert closed == 5
