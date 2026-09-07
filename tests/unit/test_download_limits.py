from __future__ import annotations

import asyncio
from io import BytesIO

import pytest

from tt_scrap.errors import AssetTooLargeError
from tt_scrap.media import AssetDownloader, DownloadedAsset
from tt_scrap.models import AssetFetchContext, AuxiliaryAssetFetchContext
from tt_scrap.proxy import ProxyManager


@pytest.mark.asyncio
async def test_oversized_declared_video_is_rejected_before_consuming_body(settings, monkeypatch):
    reads = 0
    closed = False

    class Response:
        status_code = 200
        quit_now = None

        def __init__(self):
            self.headers = {"content-length": "100", "content-type": "video/mp4"}

        async def aiter_content(self, chunk_size):
            nonlocal reads
            reads += 1
            yield b"x" * 100

        async def aclose(self):
            nonlocal closed
            closed = True

    class Session:
        async def get(self, *args, **kwargs):
            return Response()

    downloader = AssetDownloader(settings, ProxyManager())
    monkeypatch.setattr(downloader, "_curl_session", lambda proxy: Session())
    context = AssetFetchContext(
        platform="tiktok", upstream_url="https://cdn.test/video", filename="video.mp4", kind="video"
    )
    try:
        with pytest.raises(AssetTooLargeError):
            await downloader.download(context, max_bytes=50)
        assert reads == 0
        assert closed
        # A delivery-specific limit must not mutate the global asset policy.
        downloaded = await downloader.download(context)
        downloaded.file.close()
        assert reads == 1
    finally:
        await downloader.close()


@pytest.mark.asyncio
async def test_retry_releases_previous_attempt_byte_reservation(settings, monkeypatch):
    attempts = 0

    class Response:
        status_code = 200
        quit_now = None

        def __init__(self, payload):
            self.payload = payload
            self.headers = {"content-length": "40"}

        async def aiter_content(self, chunk_size):
            yield self.payload

        async def aclose(self):
            pass

    class Session:
        async def get(self, *args, **kwargs):
            nonlocal attempts
            attempts += 1
            return Response(b"x" * (20 if attempts == 1 else 40))

    downloader = AssetDownloader(settings, ProxyManager())
    monkeypatch.setattr(downloader, "_curl_session", lambda proxy: Session())
    context = AssetFetchContext(
        platform="tiktok", upstream_url="https://cdn.test/video", filename="video.mp4", kind="video"
    )
    try:
        downloaded = await downloader.download(context, max_bytes=40)
        assert downloaded.size == 40
        assert attempts == 2
        downloaded.file.close()
    finally:
        await downloader.close()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "known_lengths,output_overhead", [(True, False), (False, False), (True, True)]
)
async def test_separate_tracks_share_budget_and_cancel_sibling(
    settings, monkeypatch, known_lengths, output_overhead
):
    both_open = asyncio.Event()
    opened = 0
    closed = 0
    remuxed = False
    track_size = 20 if output_overhead else 40
    output = BytesIO(b"x" * 65)

    class Response:
        status_code = 200
        quit_now = None

        def __init__(self):
            self.headers = {"content-length": str(track_size)} if known_lengths else {}

        async def aiter_content(self, chunk_size):
            await both_open.wait()
            yield b"x" * track_size

        async def aclose(self):
            nonlocal closed
            closed += 1

    class Session:
        async def get(self, *args, **kwargs):
            nonlocal opened
            opened += 1
            if opened == 2:
                both_open.set()
            return Response()

    async def remux(*args, **kwargs):
        nonlocal remuxed
        remuxed = True
        assert output_overhead, "Over-budget tracks must never be remuxed"
        return DownloadedAsset(output, 65, None, "video/mp4")

    downloader = AssetDownloader(settings, ProxyManager())
    monkeypatch.setattr(downloader, "_curl_session", lambda proxy: Session())
    monkeypatch.setattr(downloader, "_remux_copy", remux)
    context = AssetFetchContext(
        platform="tiktok",
        upstream_url="https://cdn.test/video",
        filename="video.mp4",
        kind="video",
        audio=AuxiliaryAssetFetchContext(upstream_url="https://cdn.test/audio"),
    )
    try:
        with pytest.raises(AssetTooLargeError):
            await asyncio.wait_for(downloader.download(context, max_bytes=60), timeout=1)
        assert closed == 2
        assert remuxed == output_overhead
        if output_overhead:
            assert output.closed
    finally:
        await downloader.close()
