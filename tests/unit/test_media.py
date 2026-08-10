from __future__ import annotations

import asyncio
import threading
import time
from io import BytesIO

import pytest
import respx
from httpx import Response

from tt_scrap.errors import NetworkError
from tt_scrap.media import AssetDownloader
from tt_scrap.media.downloader import (
    DownloadedAsset,
    _close_curl_response,
    _RetryableDownload,
    detect_content_type,
    filename_for_type,
)
from tt_scrap.models import AssetFetchContext, AuxiliaryAssetFetchContext
from tt_scrap.proxy import ProxyChoice, ProxyManager


def test_content_type_detection_and_filename() -> None:
    assert detect_content_type(b"\xff\xd8\xffpayload", None) == "image/jpeg"
    assert detect_content_type(b"\x00\x00\x00\x18ftypisom", "video/mp4") == "video/mp4"
    assert filename_for_type("asset.bin", "image/jpeg") == "asset.jpg"


@pytest.mark.asyncio
async def test_curl_stream_cleanup_aborts_and_awaits_once() -> None:
    class QuitSignal:
        called = False

        def set(self) -> None:
            self.called = True

    class FakeResponse:
        def __init__(self) -> None:
            self.quit_now = QuitSignal()
            self.closed = 0

        async def aclose(self) -> None:
            self.closed += 1

    response = FakeResponse()
    await _close_curl_response(response)  # type: ignore[arg-type]

    assert response.quit_now.called
    assert response.closed == 1


@pytest.mark.asyncio
@respx.mock
async def test_instagram_asset_retries_and_verifies(settings) -> None:
    route = respx.get("https://cdn.test/image").mock(
        side_effect=[
            Response(503),
            Response(
                200,
                content=b"\x89PNG\r\n\x1a\nvalid",
                headers={"Content-Type": "image/png", "Content-Length": "13"},
            ),
        ]
    )
    downloader = AssetDownloader(settings, ProxyManager())
    try:
        result = await downloader.download(
            AssetFetchContext(
                platform="instagram",
                upstream_url="https://cdn.test/image",
                filename="image.jpg",
                kind="image",
            )
        )
        assert route.call_count == 2
        assert result.size == 13
        assert result.content_type == "image/png"
        assert result.file.read() == b"\x89PNG\r\n\x1a\nvalid"
        result.file.close()
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_length_delimited_asset_can_be_relayed_without_spooling(settings) -> None:
    payload = b"\x00\x00\x00\x18ftypisomstreamed-video"
    route = respx.get("https://cdn.test/video").mock(
        return_value=Response(
            200,
            content=payload,
            headers={"Content-Type": "video/mp4", "Content-Length": str(len(payload))},
        )
    )
    downloader = AssetDownloader(settings, ProxyManager())
    try:
        async with downloader.stream(
            AssetFetchContext(
                platform="instagram",
                upstream_url="https://cdn.test/video",
                filename="video.mp4",
                kind="video",
            )
        ) as streamed:
            assert streamed is not None
            received = b"".join([chunk async for chunk in streamed.chunks])

        assert route.call_count == 1
        assert streamed.size == len(payload)
        assert streamed.content_type == "video/mp4"
        assert received == payload
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_truncated_relay_records_failure_for_verified_fallback(settings) -> None:
    payload = b"short"
    respx.get("https://cdn.test/truncated-relay").mock(
        return_value=Response(
            200,
            content=payload,
            headers={"Content-Type": "video/mp4", "Content-Length": "10"},
        )
    )
    downloader = AssetDownloader(settings, ProxyManager())
    context = AssetFetchContext(
        platform="instagram",
        upstream_url="https://cdn.test/truncated-relay",
        filename="video.mp4",
        kind="video",
    )
    try:
        async with downloader.stream(context) as streamed:
            assert streamed is not None
            with pytest.raises(_RetryableDownload, match="Truncated asset"):
                _ = b"".join([chunk async for chunk in streamed.chunks])
        assert streamed.failure is not None
        assert not streamed.completed
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_idle_relay_releases_download_limits_for_cover_preparation(settings) -> None:
    video_payload = b"video-payload"
    cover_payload = b"\xff\xd8\xffcover"
    respx.get("https://cdn.test/relay-video").mock(
        return_value=Response(
            200,
            content=video_payload,
            headers={
                "Content-Type": "video/mp4",
                "Content-Length": str(len(video_payload)),
            },
        )
    )
    respx.get("https://cdn.test/relay-cover").mock(
        return_value=Response(
            200,
            content=cover_payload,
            headers={
                "Content-Type": "image/jpeg",
                "Content-Length": str(len(cover_payload)),
            },
        )
    )
    settings.download_concurrency = 1
    settings.slideshow_concurrency = 1
    downloader = AssetDownloader(settings, ProxyManager())
    video = AssetFetchContext(
        platform="instagram",
        upstream_url="https://cdn.test/relay-video",
        filename="video.mp4",
        kind="video",
        extraction_id="same-extraction",
    )
    cover = AssetFetchContext(
        platform="instagram",
        upstream_url="https://cdn.test/relay-cover",
        filename="cover.jpg",
        kind="cover",
        extraction_id="same-extraction",
    )
    try:
        async with downloader.stream(video) as streamed:
            assert streamed is not None
            downloaded_cover = await asyncio.wait_for(downloader.download(cover), timeout=0.2)
            downloaded_cover.file.close()
            received = b"".join([chunk async for chunk in streamed.chunks])
        assert received == video_payload
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_rolled_spool_writes_do_not_block_the_event_loop(settings) -> None:
    payload = b"large-enough-to-roll"
    respx.get("https://cdn.test/large").mock(
        return_value=Response(
            200,
            content=payload,
            headers={"Content-Type": "video/mp4", "Content-Length": str(len(payload))},
        )
    )
    settings.spool_threshold_bytes = 1
    downloader = AssetDownloader(settings, ProxyManager())
    main_thread = threading.get_ident()

    class SlowSpool(BytesIO):
        write_thread: int | None = None

        def write(self, data: bytes) -> int:
            self.write_thread = threading.get_ident()
            time.sleep(0.03)
            return super().write(data)

    spool = SlowSpool()
    context = AssetFetchContext(
        platform="instagram",
        upstream_url="https://cdn.test/large",
        filename="large.mp4",
        kind="video",
    )
    download_task = asyncio.create_task(
        downloader._download_once(
            context,
            ProxyChoice(slot=None, url=None),
            spool,
            context.upstream_url,
            compute_sha256=False,
        )
    )
    try:
        await asyncio.sleep(0.005)
        assert not download_task.done()
        await download_task
        assert spool.write_thread != main_thread
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_truncated_asset_is_retried_then_rejected(settings) -> None:
    settings.download_max_retries = 2
    route = respx.get("https://cdn.test/truncated").mock(
        return_value=Response(
            200,
            content=b"short",
            headers={"Content-Type": "video/mp4", "Content-Length": "99"},
        )
    )
    downloader = AssetDownloader(settings, ProxyManager())
    try:
        with pytest.raises(NetworkError):
            await downloader.download(
                AssetFetchContext(
                    platform="instagram",
                    upstream_url="https://cdn.test/truncated",
                    filename="video.mp4",
                    kind="video",
                )
            )
        assert route.call_count == 2
    finally:
        await downloader.close()


@pytest.mark.asyncio
@respx.mock
async def test_asset_uses_encrypted_alternate_url_after_primary_expires(settings) -> None:
    primary = respx.get("https://cdn.test/expired").mock(return_value=Response(404))
    alternate = respx.get("https://cdn.test/fresh").mock(
        return_value=Response(
            200,
            content=b"\xff\xd8\xffvalid",
            headers={"Content-Type": "image/jpeg", "Content-Length": "8"},
        )
    )
    downloader = AssetDownloader(settings, ProxyManager())
    try:
        result = await downloader.download(
            AssetFetchContext(
                platform="instagram",
                upstream_url="https://cdn.test/expired",
                alternate_upstream_urls=["https://cdn.test/fresh"],
                filename="image.jpg",
                kind="image",
            )
        )
        assert primary.call_count == 1
        assert alternate.call_count == 1
        assert result.file.read() == b"\xff\xd8\xffvalid"
        result.file.close()
    finally:
        await downloader.close()


@pytest.mark.asyncio
async def test_adaptive_video_and_audio_download_concurrently_then_remux(
    settings, monkeypatch
) -> None:
    downloader = AssetDownloader(settings, ProxyManager())
    calls: list[AssetFetchContext] = []
    both_started = asyncio.Event()

    async def fake_download_single(
        context: AssetFetchContext, *, compute_sha256: bool = True
    ) -> DownloadedAsset:
        assert not compute_sha256
        calls.append(context)
        if len(calls) == 2:
            both_started.set()
        await asyncio.wait_for(both_started.wait(), timeout=1)
        payload = b"video" if context.kind == "video" else b"audio"
        return DownloadedAsset(BytesIO(payload), len(payload), "input-digest", "video/mp4")

    async def fake_remux(video, audio, *, compute_sha256=True) -> DownloadedAsset:
        assert compute_sha256
        assert video.read() == b"video"
        assert audio.read() == b"audio"
        return DownloadedAsset(BytesIO(b"muxed"), 5, "output-digest", "video/mp4")

    monkeypatch.setattr(downloader, "_download_single", fake_download_single)
    monkeypatch.setattr(downloader, "_remux_copy", fake_remux)
    try:
        result = await downloader.download(
            AssetFetchContext(
                platform="tiktok",
                upstream_url="https://cdn.test/video",
                filename="video.mp4",
                kind="video",
                audio=AuxiliaryAssetFetchContext(
                    upstream_url="https://cdn.test/audio",
                    declared_content_type="audio/mp4",
                ),
            )
        )

        assert {call.kind for call in calls} == {"video", "audio"}
        assert result.file.read() == b"muxed"
        result.file.close()
    finally:
        await downloader.close()


@pytest.mark.asyncio
async def test_adaptive_tracks_share_the_global_transfer_limit(settings, monkeypatch) -> None:
    settings.download_concurrency = 2
    downloader = AssetDownloader(settings, ProxyManager())
    active = 0
    peak = 0
    payload = b"track"

    async def fake_download_once(
        context,
        proxy,
        spool,
        upstream_url,
        *,
        compute_sha256,
    ):
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.02)
        spool.write(payload)
        active -= 1
        content_type = "audio/mp4" if context.kind == "audio" else "video/mp4"
        return content_type, len(payload), None, len(payload), payload

    async def fake_remux(video, audio, *, compute_sha256=True):
        return DownloadedAsset(BytesIO(b"muxed"), 5, None, "video/mp4")

    monkeypatch.setattr(downloader, "_download_once", fake_download_once)
    monkeypatch.setattr(downloader, "_remux_copy", fake_remux)
    contexts = [
        AssetFetchContext(
            platform="tiktok",
            upstream_url=f"https://cdn.test/video-{index}",
            filename="video.mp4",
            kind="video",
            audio=AuxiliaryAssetFetchContext(
                upstream_url=f"https://cdn.test/audio-{index}",
                declared_content_type="audio/mp4",
            ),
        )
        for index in range(2)
    ]
    try:
        results = await asyncio.gather(
            *(downloader.download(context, compute_sha256=False) for context in contexts)
        )
        assert peak == 2
        for result in results:
            result.file.close()
    finally:
        await downloader.close()
