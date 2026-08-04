"""Retrying, verified upstream downloads backed by ephemeral spools."""

from __future__ import annotations

import asyncio
import hashlib
import logging
import random
import tempfile
import threading
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import BinaryIO, cast

import httpx
from curl_cffi import CurlError
from curl_cffi.requests import AsyncSession as CurlAsyncSession

from ..config import Settings
from ..errors import AssetTooLargeError, NetworkError, UpstreamTimeoutError
from ..models import AssetFetchContext
from ..proxy import ProxyChoice, ProxyManager, strip_proxy_auth

logger = logging.getLogger(__name__)
# A CDN 404 can mean a stale signed variant or a region-specific edge miss even
# when the post itself still exists. Post deletion/private state is classified
# during metadata extraction, so asset-level 404s are safe to retry.
_RETRYABLE_STATUSES = {403, 404, 429, 500, 502, 503, 504}
_MIME_EXTENSIONS = {
    "video/mp4": ".mp4",
    "audio/mpeg": ".mp3",
    "audio/mp4": ".m4a",
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/webp": ".webp",
    "image/heic": ".heic",
}


@dataclass(slots=True)
class DownloadedAsset:
    file: BinaryIO
    size: int
    sha256: str
    content_type: str


def detect_content_type(prefix: bytes, declared: str | None) -> str:
    if prefix.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if prefix.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if prefix.startswith(b"RIFF") and prefix[8:12] == b"WEBP":
        return "image/webp"
    if len(prefix) >= 12 and prefix[4:12] in {b"ftypheic", b"ftypmif1"}:
        return "image/heic"
    if len(prefix) >= 12 and prefix[4:8] == b"ftyp":
        brand = prefix[8:12]
        if brand in {b"M4A ", b"M4B ", b"mp42"} and declared and declared.startswith("audio"):
            return "audio/mp4"
        return "video/mp4"
    if prefix.startswith(b"ID3") or prefix[:2] in {b"\xff\xfb", b"\xff\xf3", b"\xff\xf2"}:
        return "audio/mpeg"
    if declared:
        return declared.split(";", 1)[0].strip().lower()
    return "application/octet-stream"


def filename_for_type(filename: str, content_type: str) -> str:
    extension = _MIME_EXTENSIONS.get(content_type)
    if not extension:
        return filename
    stem = filename.rsplit(".", 1)[0] if "." in filename else filename
    return f"{stem}{extension}"


class _RetryableDownload(Exception):
    pass


class AssetDownloader:
    def __init__(self, settings: Settings, proxy_manager: ProxyManager) -> None:
        self.settings = settings
        self.proxy_manager = proxy_manager
        self._semaphore = asyncio.Semaphore(settings.download_concurrency)
        self._http = httpx.AsyncClient(
            follow_redirects=True,
            timeout=httpx.Timeout(settings.upstream_download_timeout_seconds),
            limits=httpx.Limits(
                max_connections=settings.http_max_connections,
                max_keepalive_connections=settings.http_max_connections,
            ),
            headers={"User-Agent": "Mozilla/5.0"},
        )
        self._curl_sessions: dict[str | None, CurlAsyncSession] = {}
        self._curl_lock = threading.Lock()
        self._group_lock = asyncio.Lock()
        self._group_limits: dict[str, tuple[asyncio.Semaphore, int]] = {}

    @asynccontextmanager
    async def _group_limit(self, extraction_id: str | None) -> AsyncIterator[None]:
        if extraction_id is None:
            yield
            return
        async with self._group_lock:
            semaphore, active = self._group_limits.get(
                extraction_id,
                (asyncio.Semaphore(self.settings.slideshow_concurrency), 0),
            )
            self._group_limits[extraction_id] = (semaphore, active + 1)
        await semaphore.acquire()
        try:
            yield
        finally:
            semaphore.release()
            async with self._group_lock:
                current, active = self._group_limits[extraction_id]
                if active == 1:
                    del self._group_limits[extraction_id]
                else:
                    self._group_limits[extraction_id] = (current, active - 1)

    def _curl_session(self, proxy: str | None) -> CurlAsyncSession:
        with self._curl_lock:
            if proxy not in self._curl_sessions:
                self._curl_sessions[proxy] = CurlAsyncSession(
                    impersonate="chrome120",
                    proxy=proxy,
                    max_clients=self.settings.download_concurrency,
                )
            return self._curl_sessions[proxy]

    def _initial_proxy(self, context: AssetFetchContext) -> ProxyChoice:
        if context.platform != "tiktok" or self.settings.proxy_data_only:
            return ProxyChoice(slot=None, url=None)
        return self.proxy_manager.from_slot(context.proxy_slot)

    async def download(self, context: AssetFetchContext) -> DownloadedAsset:
        async with self._semaphore, self._group_limit(context.extraction_id):
            proxy = self._initial_proxy(context)
            last_error: Exception | None = None
            upstream_urls = [context.upstream_url, *context.alternate_upstream_urls]
            for attempt in range(1, self.settings.download_max_retries + 1):
                spool = tempfile.SpooledTemporaryFile(
                    max_size=self.settings.spool_threshold_bytes, mode="w+b"
                )
                binary_spool = cast(BinaryIO, spool)
                try:
                    declared, expected_length, digest, size, prefix = await self._download_once(
                        context,
                        proxy,
                        binary_spool,
                        upstream_urls[(attempt - 1) % len(upstream_urls)],
                    )
                    if size == 0:
                        raise _RetryableDownload("Upstream returned an empty asset")
                    if expected_length is not None and size != expected_length:
                        raise _RetryableDownload(
                            f"Truncated asset: expected {expected_length} bytes, got {size}"
                        )
                    spool.seek(0)
                    return DownloadedAsset(
                        file=binary_spool,
                        size=size,
                        sha256=digest,
                        content_type=detect_content_type(prefix, declared),
                    )
                except AssetTooLargeError:
                    spool.close()
                    raise
                except NetworkError as exc:
                    spool.close()
                    if len(upstream_urls) > 1 and attempt < self.settings.download_max_retries:
                        last_error = exc
                    else:
                        logger.warning(
                            "Asset download rejected by upstream on attempt %d/%d: %s",
                            attempt,
                            self.settings.download_max_retries,
                            exc,
                        )
                        raise
                except (TimeoutError, httpx.TimeoutException) as exc:
                    spool.close()
                    last_error = exc
                except (CurlError, httpx.HTTPError, _RetryableDownload) as exc:
                    spool.close()
                    last_error = exc
                if attempt < self.settings.download_max_retries:
                    if context.platform == "tiktok" and not self.settings.proxy_data_only:
                        proxy = self.proxy_manager.rotate(proxy)
                    delay = self.settings.download_retry_base_delay * (2 ** (attempt - 1))
                    delay += random.random() * delay * 0.1
                    logger.warning(
                        "Asset attempt %d/%d failed via %s (%s: %s); retrying",
                        attempt,
                        self.settings.download_max_retries,
                        strip_proxy_auth(proxy.url),
                        type(last_error).__name__,
                        last_error,
                    )
                    await asyncio.sleep(delay)
            if isinstance(last_error, (TimeoutError, httpx.TimeoutException)):
                raise UpstreamTimeoutError("Asset download timed out") from last_error
            raise NetworkError(
                f"Asset download failed after {self.settings.download_max_retries} attempts"
            ) from last_error

    async def _download_once(
        self,
        context: AssetFetchContext,
        proxy: ProxyChoice,
        spool: BinaryIO,
        upstream_url: str,
    ) -> tuple[str | None, int | None, str, int, bytes]:
        headers = {"Accept": "*/*"}
        if context.referer:
            headers.update(
                {
                    "Referer": context.referer,
                    "Origin": "https://www.tiktok.com",
                    "User-Agent": (
                        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                        "AppleWebKit/537.36 (KHTML, like Gecko) "
                        "Chrome/120.0.0.0 Safari/537.36"
                    ),
                }
            )
        digest = hashlib.sha256()
        prefix = bytearray()
        size = 0

        async def consume(chunks: object) -> None:
            nonlocal size
            async for chunk in chunks:  # type: ignore[attr-defined]
                if not chunk:
                    continue
                size += len(chunk)
                if self.settings.max_asset_bytes and size > self.settings.max_asset_bytes:
                    raise AssetTooLargeError("Asset exceeds MAX_ASSET_BYTES")
                if len(prefix) < 32:
                    prefix.extend(chunk[: 32 - len(prefix)])
                digest.update(chunk)
                spool.write(chunk)

        if context.platform == "tiktok":
            curl_response = None
            try:
                curl_response = await self._curl_session(proxy.url).get(
                    upstream_url,
                    headers=headers,
                    cookies=context.cookies,
                    timeout=self.settings.upstream_download_timeout_seconds,
                    allow_redirects=True,
                    stream=True,
                )
                if curl_response.status_code not in {200, 206}:
                    if curl_response.status_code in _RETRYABLE_STATUSES:
                        raise _RetryableDownload(f"Retryable HTTP {curl_response.status_code}")
                    raise NetworkError(f"Upstream asset returned HTTP {curl_response.status_code}")
                await consume(curl_response.aiter_content(self.settings.download_chunk_bytes))
                declared = (
                    curl_response.headers.get("content-type") or context.declared_content_type
                )
                length = curl_response.headers.get("content-length")
            finally:
                if curl_response is not None:
                    curl_response.close()
        else:
            async with self._http.stream("GET", upstream_url, headers=headers) as http_response:
                if http_response.status_code not in {200, 206}:
                    if http_response.status_code in _RETRYABLE_STATUSES:
                        raise _RetryableDownload(f"Retryable HTTP {http_response.status_code}")
                    raise NetworkError(f"Upstream asset returned HTTP {http_response.status_code}")
                await consume(http_response.aiter_bytes(self.settings.download_chunk_bytes))
                declared = (
                    http_response.headers.get("content-type") or context.declared_content_type
                )
                length = http_response.headers.get("content-length")
        expected = int(length) if length and length.isdigit() else None
        return declared, expected, digest.hexdigest(), size, bytes(prefix)

    async def close(self) -> None:
        await self._http.aclose()
        with self._curl_lock:
            sessions = list(self._curl_sessions.values())
            self._curl_sessions.clear()
        for session in sessions:
            await session.close()
