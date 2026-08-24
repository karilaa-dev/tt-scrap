from __future__ import annotations

from typing import Any, cast

import pytest
import respx
import yt_dlp
from httpx import Response
from yt_dlp.utils import ExtractorError as YtdlpExtractorError
from yt_dlp.version import __version__ as ytdlp_version

from tt_scrap.errors import ContentDeletedError, ContentPrivateError, InvalidLinkError
from tt_scrap.platforms.tiktok.adapter import TikTokAdapter, YtdlpContext
from tt_scrap.proxy import ProxyManager, ProxySession


class FakeContext:
    def close(self) -> None:
        return None


class FakeCookie:
    def __init__(self, value: str) -> None:
        self.value = value


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
async def test_transient_metadata_failure_is_retried(settings, monkeypatch) -> None:
    adapter = TikTokAdapter(settings, ProxyManager())
    calls = 0

    def fake_extract(*args: Any):
        nonlocal calls
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
