"""Isolated adapter around yt-dlp's TikTok private extraction API."""

from __future__ import annotations

import asyncio
import logging
import re
from concurrent.futures import ThreadPoolExecutor
from contextvars import copy_context
from dataclasses import dataclass
from importlib.metadata import version
from pathlib import Path
from time import perf_counter
from typing import Any
from urllib.parse import urljoin, urlparse

import httpx
import yt_dlp

from ...config import Settings
from ...errors import (
    ContentDeletedError,
    ContentPrivateError,
    ExtractionError,
    InvalidLinkError,
    NetworkError,
    RateLimitError,
    RegionBlockedError,
    ScraperError,
    ServiceBusyError,
    UpstreamTimeoutError,
)
from ...logging import bind_request_context, elapsed_ms, log_event, record_recovery
from ...proxy import ProxyManager, ProxySession
from .http import ResolverClients

logger = logging.getLogger(__name__)

TIKTOK_USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/120.0.0.0 Safari/537.36"
)
_ID_RE = re.compile(r"/(?:video|photo)/(\d+)")
_HTTP_429_RE = re.compile(r"\b(?:http(?:\s+error)?|status(?:\s+code)?)\s*[:=]?\s*429\b")


def _classify_ytdlp_error(exc: Exception) -> str:
    message = str(exc).lower()
    if any(
        phrase in message
        for phrase in (
            "log in",
            "login",
            "private",
            "not be comfortable for some audiences",
        )
    ):
        return "private"
    if any(
        phrase in message for phrase in ("rate limit", "too many requests")
    ) or _HTTP_429_RE.search(message):
        return "rate_limit"
    if any(phrase in message for phrase in ("region", "geo", "country", "ip address is blocked")):
        return "region"
    if any(phrase in message for phrase in ("unavailable", "removed", "deleted")):
        return "deleted"
    return "extraction"


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
        self._url_resolution_semaphore = asyncio.Semaphore(settings.http_max_connections)
        self._clients = ResolverClients(lambda proxy: self._new_http_client(proxy))
        log_event(
            logger,
            "tiktok.resolver.configured",
            httpx_version=httpx.__version__,
            httpcore_version=version("httpcore"),
            http_max_connections=settings.http_max_connections,
            url_resolve_timeout_seconds=settings.url_resolve_timeout_seconds,
            url_resolve_pool_timeout_seconds=settings.url_resolve_pool_timeout_seconds,
            success=True,
        )
        self.cookies_path: str | None = None
        if settings.ytdlp_cookies:
            path = Path(settings.ytdlp_cookies).expanduser().resolve()
            if path.is_file():
                self.cookies_path = str(path)
            else:
                logger.warning("Configured yt-dlp cookie file does not exist")

    def _new_http_client(self, proxy: str | None) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            proxy=proxy,
            follow_redirects=False,
            timeout=httpx.Timeout(
                5,
                pool=self.settings.url_resolve_pool_timeout_seconds,
            ),
            limits=httpx.Limits(
                max_connections=self.settings.http_max_connections,
                max_keepalive_connections=self.settings.http_max_connections,
            ),
            headers={"User-Agent": TIKTOK_USER_AGENT},
        )

    async def resolve_url(self, url: str, proxy_session: ProxySession) -> str:
        bind_request_context(platform="tiktok")
        try:
            async with asyncio.timeout(self.settings.url_resolve_timeout_seconds):
                return await self._resolve_url(url, proxy_session)
        except TimeoutError as exc:
            raise UpstreamTimeoutError("TikTok URL resolution deadline exceeded") from exc

    async def _resolve_url(self, url: str, proxy_session: ProxySession) -> str:
        started_at = perf_counter()
        validate_tiktok_url(url)
        parsed = urlparse(url)
        short = parsed.hostname in {"vm.tiktok.com", "vt.tiktok.com"} or parsed.path.startswith(
            "/t/"
        )
        if not short:
            log_event(
                logger,
                "tiktok.url_resolution.completed",
                level=logging.DEBUG,
                message="TikTok URL did not require redirect resolution",
                platform="tiktok",
                fast_path=True,
                elapsed_ms=elapsed_ms(started_at),
                success=True,
            )
            return url
        last_error: Exception | None = None
        for attempt in range(1, self.settings.url_resolve_max_retries + 1):
            if attempt > 1:
                record_recovery("retry_count")
            attempt_started_at = perf_counter()
            choice = proxy_session.get()
            queue_wait = 0.0
            try:
                queue_started_at = perf_counter()
                async with self._url_resolution_semaphore:
                    queue_wait = elapsed_ms(queue_started_at)
                    async with self._clients.acquire(choice.url) as client:
                        resolved = await self._follow_tiktok_redirects(client, url)
                log_event(
                    logger,
                    "tiktok.url_resolution.completed",
                    level=logging.DEBUG,
                    message="TikTok short URL resolution completed",
                    platform="tiktok",
                    fast_path=False,
                    attempt=attempt,
                    proxy_used=choice.url is not None,
                    proxy_slot=choice.slot,
                    queue_wait_ms=queue_wait,
                    elapsed_ms=elapsed_ms(started_at),
                    success=True,
                )
                return resolved
            except (httpx.HTTPError, InvalidLinkError) as exc:
                last_error = exc
                permanent = isinstance(exc, InvalidLinkError) or (
                    isinstance(exc, httpx.HTTPStatusError)
                    and exc.response.status_code in {400, 404, 410}
                )
                retrying = not permanent and attempt < self.settings.url_resolve_max_retries
                log_event(
                    logger,
                    "tiktok.url_resolution.failed",
                    level=logging.DEBUG,
                    message="TikTok short URL resolution failed",
                    platform="tiktok",
                    attempt=attempt,
                    proxy_used=choice.url is not None,
                    proxy_slot=choice.slot,
                    queue_wait_ms=queue_wait,
                    elapsed_ms=elapsed_ms(attempt_started_at),
                    error_type=type(exc).__name__,
                    retrying=retrying,
                    success=False,
                )
                if retrying:
                    proxy_session.rotate()
                else:
                    break
        if isinstance(last_error, InvalidLinkError):
            raise last_error
        if isinstance(last_error, httpx.PoolTimeout):
            raise ServiceBusyError("TikTok URL resolver is busy") from last_error
        if isinstance(last_error, httpx.TimeoutException):
            raise UpstreamTimeoutError("TikTok URL resolution timed out") from last_error
        if isinstance(last_error, httpx.HTTPStatusError):
            status = last_error.response.status_code
            if status in {400, 404, 410}:
                raise InvalidLinkError("Invalid or expired TikTok link") from last_error
            if status == 429:
                raise RateLimitError("TikTok URL resolution rate limit exceeded") from last_error
        raise NetworkError("TikTok URL resolution failed") from last_error

    @staticmethod
    async def _follow_tiktok_redirects(client: httpx.AsyncClient, url: str) -> str:
        current = url
        for _ in range(6):
            validate_tiktok_url(current)
            # Read response headers only. The final request preserves status and
            # redirect validation without downloading TikTok's large HTML body.
            async with client.stream("GET", current, follow_redirects=False) as response:
                if response.is_redirect:
                    location = response.headers.get("location")
                    if not location:
                        raise InvalidLinkError("TikTok redirect did not include a destination")
                    destination = str(urljoin(current, location))
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
            if status == 10204:
                return None, "region", None
            if status in (10216, 10222):
                return None, "private", None
            if not data:
                return (
                    None,
                    f"status_{status}" if status not in (None, -1, 0) else "extraction",
                    None,
                )
            context = YtdlpContext(ydl, extractor, url, proxy_slot)
            ydl = None
            return data, status, context
        except (yt_dlp.utils.DownloadError, yt_dlp.utils.ExtractorError) as exc:
            return None, _classify_ytdlp_error(exc), None
        finally:
            if ydl is not None:
                ydl.close()

    async def extract(
        self, url: str, video_id: str, proxy_session: ProxySession
    ) -> tuple[dict[str, Any], YtdlpContext]:
        bind_request_context(platform="tiktok", source_id=video_id)
        started_at = perf_counter()
        async with self._semaphore:
            queue_wait = elapsed_ms(started_at)
            last_status: str | None = None
            last_error: Exception | None = None
            for attempt in range(1, self.settings.video_info_max_retries + 1):
                if attempt > 1:
                    record_recovery("retry_count")
                attempt_started_at = perf_counter()
                choice = proxy_session.get()
                context: YtdlpContext | None = None
                try:
                    loop = asyncio.get_running_loop()
                    data, status, context = await loop.run_in_executor(
                        self._executor,
                        copy_context().run,
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
                        log_event(
                            logger,
                            "tiktok.metadata.completed",
                            level=logging.DEBUG,
                            message="TikTok metadata extraction completed",
                            platform="tiktok",
                            source_id=video_id,
                            attempt=attempt,
                            proxy_used=choice.url is not None,
                            queue_wait_ms=queue_wait,
                            elapsed_ms=elapsed_ms(started_at),
                            success=True,
                        )
                        return data, context
                    if context:
                        context.close()
                    last_error = ExtractionError(f"TikTok extraction status: {status}")
                except (
                    ContentDeletedError,
                    ContentPrivateError,
                    RegionBlockedError,
                ) as exc:
                    if context:
                        context.close()
                    log_event(
                        logger,
                        "tiktok.metadata.failed",
                        level=logging.DEBUG,
                        message="TikTok metadata extraction returned a permanent content error",
                        platform="tiktok",
                        source_id=video_id,
                        attempt=attempt,
                        proxy_used=choice.url is not None,
                        queue_wait_ms=queue_wait,
                        elapsed_ms=elapsed_ms(started_at),
                        error_type=type(exc).__name__,
                        failure_reason=status,
                        retrying=False,
                        success=False,
                    )
                    raise
                except Exception as exc:
                    if context:
                        context.close()
                    last_error = exc
                retrying = attempt < self.settings.video_info_max_retries
                log_event(
                    logger,
                    "tiktok.metadata.failed",
                    level=logging.DEBUG,
                    message="TikTok metadata extraction attempt failed",
                    platform="tiktok",
                    source_id=video_id,
                    attempt=attempt,
                    proxy_used=choice.url is not None,
                    elapsed_ms=elapsed_ms(attempt_started_at),
                    error_type=type(last_error).__name__,
                    failure_reason=last_status or "exception",
                    retrying=retrying,
                    success=False,
                )
                if retrying:
                    proxy_session.rotate()
            if last_status == "rate_limit":
                raise RateLimitError("TikTok rate limit exceeded") from last_error
            if isinstance(last_error, ScraperError):
                raise last_error
            raise ExtractionError("TikTok metadata extraction failed") from last_error

    async def close(self) -> None:
        await self._clients.close()
        self._executor.shutdown(wait=False, cancel_futures=True)
