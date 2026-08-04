from __future__ import annotations

import asyncio
import hashlib
import threading
import time
from typing import Any, BinaryIO

import pytest

from tt_scrap.media import AssetDownloader
from tt_scrap.models import AssetFetchContext
from tt_scrap.platforms.tiktok.adapter import TikTokAdapter
from tt_scrap.proxy import ProxyManager, ProxySession


class FakeContext:
    def close(self) -> None:
        return None


@pytest.mark.asyncio
async def test_metadata_limit_uses_32_workers_without_blocking_loop(settings, monkeypatch) -> None:
    settings.extraction_concurrency = 32
    settings.executor_workers = 32
    adapter = TikTokAdapter(settings, ProxyManager())
    lock = threading.Lock()
    active = 0
    peak = 0

    def fake_extract(*args: Any):
        nonlocal active, peak
        with lock:
            active += 1
            peak = max(peak, active)
        time.sleep(0.03)
        with lock:
            active -= 1
        return {"video": {}}, None, FakeContext()

    monkeypatch.setattr(adapter, "_extract_sync", fake_extract)
    ticker_ran = False

    async def ticker() -> None:
        nonlocal ticker_ran
        await asyncio.sleep(0.005)
        ticker_ran = True

    try:
        tasks = [
            adapter.extract(
                f"https://www.tiktok.com/@_/video/{index}",
                str(index),
                ProxySession(ProxyManager()),
            )
            for index in range(40)
        ]
        await asyncio.gather(ticker(), *tasks)
        assert 1 < peak <= 32
        assert ticker_ran
    finally:
        await adapter.close()


@pytest.mark.asyncio
async def test_asset_limit_caps_64_concurrent_spools(settings, monkeypatch) -> None:
    settings.download_concurrency = 64
    downloader = AssetDownloader(settings, ProxyManager())
    active = 0
    peak = 0
    payload = b"\xff\xd8\xffasset"

    async def fake_download_once(
        context: AssetFetchContext,
        proxy: object,
        spool: BinaryIO,
        upstream_url: str,
    ) -> tuple[str, int, str, int, bytes]:
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)
        spool.write(payload)
        active -= 1
        return (
            "image/jpeg",
            len(payload),
            hashlib.sha256(payload).hexdigest(),
            len(payload),
            payload,
        )

    monkeypatch.setattr(downloader, "_download_once", fake_download_once)
    contexts = [
        AssetFetchContext(
            platform="instagram",
            upstream_url=f"https://cdn.test/{index}",
            filename=f"{index}.jpg",
            kind="image",
        )
        for index in range(72)
    ]
    try:
        results = await asyncio.gather(*(downloader.download(context) for context in contexts))
        assert peak == 64
        for result in results:
            assert result.file.read() == payload
            result.file.close()
    finally:
        await downloader.close()
