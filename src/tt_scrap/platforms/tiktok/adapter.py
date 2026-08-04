"""Isolated adapter around yt-dlp's TikTok private extraction API."""

from __future__ import annotations

import asyncio
import logging
import re
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urljoin, urlparse

import httpx
import yt_dlp
from yt_dlp.networking.impersonate import ImpersonateTarget

from ...config import Settings
from ...errors import (
    ContentDeletedError,
    ContentPrivateError,
    ExtractionError,
    InvalidLinkError,
    RateLimitError,
    RegionBlockedError,
    ScraperError,
)
from ...proxy import ProxyManager, ProxySession, strip_proxy_auth

logger = logging.getLogger(__name__)

TIKTOK_USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/120.0.0.0 Safari/537.36"
)
_ID_RE = re.compile(r"/(?:video|photo)/(\d+)")


def is_tiktok_host(host: str | None) -> bool:
    if not host:
        return False
    normalized = host.rstrip(".").lower()
    return normalized == "tiktok.com" or normalized.endswith(".tiktok.com")


def validate_tiktok_url(url: str) -> None:
    parsed = urlparse(url)
    if parsed.scheme != "https" or not is_tiktok_host(parsed.hostname):
        raise InvalidLinkError("Only HTTPS TikTok URLs are accepted")


@dataclass(slots=True)
class YtdlpContext:
    ydl: yt_dlp.YoutubeDL
    extractor: Any
    referer_url: str
    proxy_slot: int | None

    def cookies_for(self, media_url: str) -> dict[str, str]:
        cookies: dict[str, str] = {}
        for candidate in ("https://www.tiktok.com/", media_url):
            try:
                for name, cookie in self.extractor._get_cookies(candidate).items():
                    cookies[name] = cookie.value
            except Exception:
                logger.debug("Could not read a scoped yt-dlp cookie jar")
        if not cookies and hasattr(self.ydl, "cookiejar"):
            for cookie in self.ydl.cookiejar:
                cookies[cookie.name] = cookie.value
        return cookies

    def close(self) -> None:
        try:
            self.ydl.close()
        except Exception:
            logger.debug("Failed to close yt-dlp context", exc_info=True)


class TikTokAdapter:
    def __init__(self, settings: Settings, proxy_manager: ProxyManager) -> None:
        self.settings = settings
        self.proxy_manager = proxy_manager
        self._executor = ThreadPoolExecutor(
            max_workers=settings.executor_workers, thread_name_prefix="tiktok-extract"
        )
        self._semaphore = asyncio.Semaphore(settings.extraction_concurrency)
        self._http = httpx.AsyncClient(
            follow_redirects=False,
            timeout=httpx.Timeout(15, connect=5, read=10),
            limits=httpx.Limits(
                max_connections=settings.http_max_connections,
                max_keepalive_connections=settings.http_max_connections,
            ),
            headers={"User-Agent": TIKTOK_USER_AGENT},
        )
        self.cookies_path: str | None = None
        if settings.ytdlp_cookies:
            path = Path(settings.ytdlp_cookies).expanduser().resolve()
            if path.is_file():
                self.cookies_path = str(path)
            else:
                logger.warning("Configured yt-dlp cookie file does not exist")

    async def resolve_url(self, url: str, proxy_session: ProxySession) -> str:
        validate_tiktok_url(url)
        short = any(part in url for part in ("vm.tiktok.com", "vt.tiktok.com", "/t/"))
        if not short:
            return url
        last_error: Exception | None = None
        for attempt in range(1, self.settings.url_resolve_max_retries + 1):
            choice = proxy_session.get()
            try:
                if choice.url:
                    async with httpx.AsyncClient(
                        proxy=choice.url,
                        follow_redirects=False,
                        timeout=httpx.Timeout(15, connect=5, read=10),
                        headers={"User-Agent": TIKTOK_USER_AGENT},
                    ) as client:
                        resolved = await self._follow_tiktok_redirects(client, url)
                else:
                    resolved = await self._follow_tiktok_redirects(self._http, url)
                return resolved
            except (httpx.HTTPError, InvalidLinkError) as exc:
                last_error = exc
                if attempt < self.settings.url_resolve_max_retries:
                    proxy_session.rotate()
                    logger.warning(
                        "TikTok URL resolution attempt %d/%d failed via %s",
                        attempt,
                        self.settings.url_resolve_max_retries,
                        strip_proxy_auth(choice.url),
                    )
        raise InvalidLinkError("Invalid or expired TikTok link") from last_error

    @staticmethod
    async def _follow_tiktok_redirects(client: httpx.AsyncClient, url: str) -> str:
        current = url
        for _ in range(6):
            validate_tiktok_url(current)
            response = await client.get(current, follow_redirects=False)
            if response.is_redirect:
                location = response.headers.get("location")
                if not location:
                    raise InvalidLinkError("TikTok redirect did not include a destination")
                destination = urljoin(current, location)
                validate_tiktok_url(destination)
                current = destination
                continue
            response.raise_for_status()
            validate_tiktok_url(current)
            return current
        raise InvalidLinkError("TikTok URL redirected too many times")

    @staticmethod
    def extract_id(url: str) -> str:
        match = _ID_RE.search(url)
        if not match:
            raise InvalidLinkError("TikTok video or photo ID was not found")
        return match.group(1)

    def _ydl_options(self, proxy: str | None) -> dict[str, Any]:
        options: dict[str, Any] = {
            "quiet": True,
            "no_warnings": True,
            "http_headers": {"User-Agent": TIKTOK_USER_AGENT},
            "impersonate": ImpersonateTarget("chrome", "120", "macos", None),
        }
        if proxy:
            options["proxy"] = proxy
        if self.cookies_path:
            options["cookiefile"] = self.cookies_path
        return options

    def _extract_sync(
        self, url: str, video_id: str, proxy: str | None, proxy_slot: int | None
    ) -> tuple[dict[str, Any] | None, str | None, YtdlpContext | None]:
        ydl: yt_dlp.YoutubeDL | None = None
        try:
            ydl = yt_dlp.YoutubeDL(self._ydl_options(proxy))
            extractor = ydl.get_info_extractor("TikTok")
            extractor.set_downloader(ydl)
            if not hasattr(extractor, "_extract_web_data_and_status"):
                raise ExtractionError(
                    "Installed yt-dlp is incompatible: TikTok private API is missing"
                )
            data, status = extractor._extract_web_data_and_status(url, video_id)
            if status in (10204, 10216):
                return None, "deleted", None
            if status == 10222:
                return None, "private", None
            if not data:
                return None, "extraction", None
            context = YtdlpContext(ydl, extractor, url, proxy_slot)
            ydl = None
            return data, status, context
        except yt_dlp.utils.DownloadError as exc:
            message = str(exc).lower()
            if any(word in message for word in ("unavailable", "removed", "deleted")):
                return None, "deleted", None
            if "private" in message:
                return None, "private", None
            if any(word in message for word in ("rate", "too many", "429")):
                return None, "rate_limit", None
            if any(word in message for word in ("region", "geo", "country")):
                return None, "region", None
            logger.warning("yt-dlp TikTok extraction failed: %s", type(exc).__name__)
            return None, "extraction", None
        finally:
            if ydl is not None:
                ydl.close()

    async def extract(
        self, url: str, video_id: str, proxy_session: ProxySession
    ) -> tuple[dict[str, Any], YtdlpContext]:
        async with self._semaphore:
            last_status: str | None = None
            last_error: Exception | None = None
            for attempt in range(1, self.settings.video_info_max_retries + 1):
                choice = proxy_session.get()
                context: YtdlpContext | None = None
                try:
                    loop = asyncio.get_running_loop()
                    data, status, context = await loop.run_in_executor(
                        self._executor,
                        self._extract_sync,
                        url,
                        video_id,
                        choice.url,
                        choice.slot,
                    )
                    last_status = str(status) if status is not None else None
                    if status == "deleted":
                        raise ContentDeletedError("TikTok content was deleted")
                    if status == "private":
                        raise ContentPrivateError("TikTok content is private")
                    if status == "region":
                        raise RegionBlockedError("TikTok content is region blocked")
                    if data is not None and context is not None and status in (None, "ok", 0):
                        return data, context
                    if context:
                        context.close()
                    last_error = ExtractionError(f"TikTok extraction status: {status}")
                except (ContentDeletedError, ContentPrivateError, RegionBlockedError):
                    if context:
                        context.close()
                    raise
                except Exception as exc:
                    if context:
                        context.close()
                    last_error = exc
                if attempt < self.settings.video_info_max_retries:
                    proxy_session.rotate()
                    logger.warning(
                        "TikTok metadata attempt %d/%d failed via %s",
                        attempt,
                        self.settings.video_info_max_retries,
                        strip_proxy_auth(choice.url),
                    )
            if last_status == "rate_limit":
                raise RateLimitError("TikTok rate limit exceeded") from last_error
            if isinstance(last_error, ScraperError):
                raise last_error
            raise ExtractionError("TikTok metadata extraction failed") from last_error

    async def close(self) -> None:
        await self._http.aclose()
        self._executor.shutdown(wait=False, cancel_futures=True)
