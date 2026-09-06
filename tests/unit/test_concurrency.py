from __future__ import annotations

import asyncio
import hashlib
import threading
import time
from typing import Any, BinaryIO

import pytest
from httpx import AsyncClient, PoolTimeout

from tt_scrap.config import Settings
from tt_scrap.media import AssetDownloader
from tt_scrap.models import AssetFetchContext
from tt_scrap.platforms.tiktok.adapter import TikTokAdapter
from tt_scrap.proxy import ProxyManager, ProxySession


class FakeContext:
    def close(self) -> None:
        return None


def test_production_concurrency_defaults() -> None:
    assert Settings.model_fields["instagram_concurrency"].default == 4
    assert Settings.model_fields["telegram_pipeline_concurrency"].default == 32
    assert Settings.model_fields["telegram_upload_concurrency"].default == 20
    assert Settings.model_fields["telegram_thumbnail_wait_seconds"].default == 1.5
    assert Settings.model_fields["image_conversion_workers"].default == 0


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
async def test_short_url_resolution_queues_before_http_pool_exhaustion(
    settings, monkeypatch
) -> None:
    settings.http_max_connections = 1
    settings.url_resolve_max_retries = 1
    adapter = TikTokAdapter(settings, ProxyManager())
    active = 0
    peak = 0

    async def fake_follow(client: AsyncClient, url: str) -> str:
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        try:
            await asyncio.sleep(0.01)
            if active > settings.http_max_connections:
                raise PoolTimeout("connection pool exhausted")
            return url
        finally:
            active -= 1

    monkeypatch.setattr(adapter, "_follow_tiktok_redirects", fake_follow)
    urls = ["https://vm.tiktok.com/one/", "https://vt.tiktok.com/two/"]
    try:
        assert (
            await asyncio.gather(
                *(adapter.resolve_url(url, ProxySession(ProxyManager())) for url in urls)
            )
            == urls
        )
        assert peak == 1
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
        *,
        compute_sha256: bool,
        budget=None,
    ) -> tuple[str, int, str | None, int, bytes]:
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)
        spool.write(payload)
        active -= 1
        return (
            "image/jpeg",
            len(payload),
            hashlib.sha256(payload).hexdigest() if compute_sha256 else None,
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
