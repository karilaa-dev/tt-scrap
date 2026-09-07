from __future__ import annotations

from typing import Any, cast

import pytest
import respx
import yt_dlp
from httpx import ConnectTimeout, PoolTimeout, Response
from yt_dlp.utils import ExtractorError as YtdlpExtractorError
from yt_dlp.version import __version__ as ytdlp_version

from tt_scrap.errors import (
    ContentDeletedError,
    ContentPrivateError,
    InvalidLinkError,
    NetworkError,
    RateLimitError,
    ServiceBusyError,
    UpstreamTimeoutError,
)
from tt_scrap.logging import request_id_var
from tt_scrap.platforms.tiktok.adapter import (
    TikTokAdapter,
    YtdlpContext,
    _classify_ytdlp_error,
)
from tt_scrap.proxy import ProxyManager, ProxySession


class FakeContext:
    def close(self) -> None:
        return None


class FakeCookie:
    def __init__(self, value: str) -> None:
        self.value = value


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "error,expected", [(PoolTimeout, ServiceBusyError), (ConnectTimeout, UpstreamTimeoutError)]
)
async def test_resolution_preserves_transient_failure_type(settings, monkeypatch, error, expected):
    adapter = TikTokAdapter(settings, ProxyManager())
    attempts = 0

    async def fail(*args):
        nonlocal attempts
        attempts += 1
        raise error("upstream unavailable")

    monkeypatch.setattr(adapter, "_follow_tiktok_redirects", fail)
    try:
        with pytest.raises(expected):
            await adapter.resolve_url("https://vt.tiktok.com/FAIL/", ProxySession(ProxyManager()))
        assert attempts == settings.url_resolve_max_retries
    finally:
        await adapter.close()


@pytest.mark.asyncio
async def test_resolution_deadline_includes_wait_for_capacity(settings):
    import asyncio

    settings.url_resolve_timeout_seconds = 0.02
    settings.http_max_connections = 1
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        async with adapter._url_resolution_semaphore:
            with pytest.raises(UpstreamTimeoutError, match="deadline"):
                await asyncio.wait_for(
                    adapter.resolve_url(
                        "https://vt.tiktok.com/WAIT/", ProxySession(ProxyManager())
                    ),
                    timeout=0.2,
                )
    finally:
        await adapter.close()


@pytest.mark.asyncio
@respx.mock
@pytest.mark.parametrize(
    "status,error,attempts",
    [
        (404, InvalidLinkError, 1),
        (429, RateLimitError, 3),
        (503, NetworkError, 3),
    ],
)
async def test_resolution_status_retry_policy(
    settings, status, error, attempts, log_records, request_log_context
):
    route = respx.get("https://vt.tiktok.com/STATUS/").respond(status)
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        with pytest.raises(error):
            await adapter.resolve_url("https://vt.tiktok.com/STATUS/", ProxySession(ProxyManager()))
        assert route.call_count == attempts
    finally:
        await adapter.close()

    assert request_log_context.retry_count == attempts - 1
    assert all(record.levelno < 30 for record in log_records)


class FakeExtractor:
    def _get_cookies(self, url: str) -> dict[str, FakeCookie]:
        if url == "https://www.tiktok.com/":
            return {"sessionid": FakeCookie("account-cookie")}
        return {"cdn-token": FakeCookie("asset-cookie")}


def test_pinned_ytdlp_has_tiktok_webpage_fix_and_required_private_api() -> None:
    assert tuple(map(int, ytdlp_version.split("."))) >= (2026, 8, 19)
    with yt_dlp.YoutubeDL({"quiet": True, "no_warnings": True}) as ydl:
        extractor = ydl.get_info_extractor("TikTok")
        assert hasattr(extractor, "_extract_web_data_and_status")


@pytest.mark.parametrize(
    "message",
    (
        "HTTP Error 429: Too Many Requests",
        "Server returned status code: 429",
    ),
)
def test_ytdlp_http_429_errors_are_classified_as_rate_limits(message: str) -> None:
    assert _classify_ytdlp_error(Exception(message)) == "rate_limit"


def test_ytdlp_video_id_containing_429_is_not_classified_as_rate_limit() -> None:
    error = Exception("[TikTok] 123429456: Unable to extract webpage video data")

    assert _classify_ytdlp_error(error) == "extraction"


@pytest.mark.asyncio
async def test_adapter_defers_tiktok_impersonation_to_ytdlp(settings) -> None:
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        assert "impersonate" not in adapter._ydl_options(None)
    finally:
        await adapter.close()


@pytest.mark.asyncio
async def test_cookie_file_and_per_asset_cookies_are_loaded(settings, tmp_path) -> None:
    cookie_file = tmp_path / "cookies.txt"
    cookie_file.write_text(
        "# Netscape HTTP Cookie File\n.tiktok.com\tTRUE\t/\tTRUE\t0\tsessionid\tsecret\n",
        encoding="utf-8",
    )
    settings.ytdlp_cookies = str(cookie_file)
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        assert adapter.cookies_path == str(cookie_file.resolve())
        assert adapter._ydl_options(None)["cookiefile"] == str(cookie_file.resolve())
    finally:
        await adapter.close()

    context = YtdlpContext(
        ydl=cast(Any, object()),
        extractor=FakeExtractor(),
        referer_url="https://www.tiktok.com/@_/video/123",
        proxy_slot=0,
    )
    assert context.cookies_for("https://cdn.test/asset") == {
        "sessionid": "account-cookie",
        "cdn-token": "asset-cookie",
    }


@pytest.mark.asyncio
@respx.mock
async def test_short_url_rejects_redirect_to_non_tiktok_host(settings) -> None:
    respx.get("https://vm.tiktok.com/example/").mock(
        return_value=Response(302, headers={"Location": "http://169.254.169.254/latest"})
    )
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        with pytest.raises(InvalidLinkError):
            await adapter.resolve_url(
                "https://vm.tiktok.com/example/", ProxySession(ProxyManager())
            )
    finally:
        await adapter.close()


@pytest.mark.asyncio
@respx.mock
async def test_short_url_follows_redirect_to_full_tiktok_post(settings) -> None:
    short_url = "https://vt.tiktok.com/EXAMPLE/"
    full_url = "https://www.tiktok.com/@creator/video/1234567890123456789"
    short_route = respx.get(short_url).mock(
        return_value=Response(302, headers={"Location": full_url})
    )
    full_route = respx.get(full_url).mock(return_value=Response(200))
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        resolved = await adapter.resolve_url(short_url, ProxySession(ProxyManager()))
        assert resolved == full_url
        assert adapter.extract_id(resolved) == "1234567890123456789"
        assert short_route.call_count == 1
        assert full_route.call_count == 1
    finally:
        await adapter.close()


@pytest.mark.asyncio
@respx.mock
async def test_short_url_rejects_removed_canonical_post(settings) -> None:
    short_url = "https://vt.tiktok.com/REMOVED/"
    full_url = "https://www.tiktok.com/@creator/video/1234567890123456789"
    respx.get(short_url).mock(return_value=Response(302, headers={"Location": full_url}))
    respx.get(full_url).mock(return_value=Response(404))
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        with pytest.raises(InvalidLinkError):
            await adapter.resolve_url(short_url, ProxySession(ProxyManager()))
    finally:
        await adapter.close()


@pytest.mark.asyncio
async def test_transient_metadata_failure_is_retried(
    settings, monkeypatch, log_records, request_log_context
) -> None:
    adapter = TikTokAdapter(settings, ProxyManager())
    calls = 0

    def fake_extract(*args: Any):
        nonlocal calls
        assert request_id_var.get() == "unit-request"
        calls += 1
        if calls == 1:
            return None, "extraction", None
        return {"video": {}}, None, FakeContext()

    monkeypatch.setattr(adapter, "_extract_sync", fake_extract)
    try:
        data, _context = await adapter.extract(
            "https://www.tiktok.com/@_/video/123",
            "123",
            ProxySession(ProxyManager()),
        )
        assert data == {"video": {}}
        assert calls == 2
    finally:
        await adapter.close()

    assert request_log_context.retry_count == 1
    assert all(record.levelno < 30 for record in log_records)


@pytest.mark.asyncio
async def test_deleted_content_is_not_retried(settings, monkeypatch) -> None:
    adapter = TikTokAdapter(settings, ProxyManager())
    calls = 0

    def fake_extract(*args: Any):
        nonlocal calls
        calls += 1
        return None, "deleted", None

    monkeypatch.setattr(adapter, "_extract_sync", fake_extract)
    try:
        with pytest.raises(ContentDeletedError):
            await adapter.extract(
                "https://www.tiktok.com/@_/video/123",
                "123",
                ProxySession(ProxyManager()),
            )
        assert calls == 1
    finally:
        await adapter.close()


@pytest.mark.asyncio
async def test_sensitive_content_extractor_error_is_not_retried(settings, monkeypatch) -> None:
    calls = 0

    class SensitiveExtractor:
        def set_downloader(self, ydl: object) -> None:
            return None

        def _extract_web_data_and_status(self, url: str, video_id: str):
            raise YtdlpExtractorError(
                "This post may not be comfortable for some audiences. Log in for access"
            )

    class SensitiveYDL:
        def __init__(self, options: dict[str, Any]) -> None:
            nonlocal calls
            calls += 1

        def get_info_extractor(self, name: str) -> SensitiveExtractor:
            return SensitiveExtractor()

        def close(self) -> None:
            return None

    monkeypatch.setattr(yt_dlp, "YoutubeDL", SensitiveYDL)
    adapter = TikTokAdapter(settings, ProxyManager())
    try:
        with pytest.raises(ContentPrivateError):
            await adapter.extract(
                "https://www.tiktok.com/@_/video/123",
                "123",
                ProxySession(ProxyManager()),
            )
        assert calls == 1
    finally:
        await adapter.close()
