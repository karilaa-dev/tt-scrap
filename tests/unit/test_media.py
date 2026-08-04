from __future__ import annotations

import pytest
import respx
from httpx import Response

from tt_scrap.errors import NetworkError
from tt_scrap.media import AssetDownloader
from tt_scrap.media.downloader import (
    _close_curl_response,
    detect_content_type,
    filename_for_type,
)
from tt_scrap.models import AssetFetchContext
from tt_scrap.proxy import ProxyManager


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
