from __future__ import annotations

from typing import Any, cast

import pytest
import respx
import yt_dlp
from httpx import Response
from yt_dlp.networking._curlcffi import BROWSER_TARGETS

from tt_scrap.errors import ContentDeletedError, InvalidLinkError
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


def test_pinned_ytdlp_has_required_private_api() -> None:
    with yt_dlp.YoutubeDL({"quiet": True, "no_warnings": True}) as ydl:
        extractor = ydl.get_info_extractor("TikTok")
        assert hasattr(extractor, "_extract_web_data_and_status")
    available_targets = {
        target_name
        for version_targets in BROWSER_TARGETS.values()
        for target_name in version_targets
    }
    assert "chrome120" in available_targets


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
