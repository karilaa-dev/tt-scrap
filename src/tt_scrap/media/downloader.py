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
from time import perf_counter
from typing import BinaryIO, cast

import httpx
from curl_cffi import CurlError
from curl_cffi.requests import AsyncSession as CurlAsyncSession
from curl_cffi.requests.models import Response as CurlResponse

from ..config import Settings
from ..errors import AssetTooLargeError, NetworkError, UpstreamTimeoutError
from ..logging import elapsed_ms, log_event
from ..models import AssetFetchContext
from ..proxy import ProxyChoice, ProxyManager

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
    "image/heif": ".heif",
    "image/avif": ".avif",
    "image/tiff": ".tiff",
    "image/bmp": ".bmp",
    "image/gif": ".gif",
}


@dataclass(slots=True)
class DownloadedAsset:
    file: BinaryIO
    size: int
    sha256: str | None
    content_type: str
    declared_content_type: str | None = None


def detect_content_type(prefix: bytes, declared: str | None) -> str:
    if prefix.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if prefix.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if prefix.startswith(b"RIFF") and prefix[8:12] == b"WEBP":
        return "image/webp"
    if prefix.startswith((b"II*\x00", b"MM\x00*")):
        return "image/tiff"
    if prefix.startswith(b"BM"):
        return "image/bmp"
    if prefix.startswith((b"GIF87a", b"GIF89a")):
        return "image/gif"
    if len(prefix) >= 12 and prefix[4:8] == b"ftyp":
        brand = prefix[8:12]
        if brand in {b"avif", b"avis"}:
            return "image/avif"
        if brand in {b"mif1", b"msf1"}:
            return "image/heif"
        if brand in {b"heic", b"heix", b"hevc", b"hevx", b"heim", b"heis"}:
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


async def _close_curl_response(response: CurlResponse) -> None:
    """Stop an unfinished stream and let its task release the curl handle once."""
    if response.quit_now is not None:
        response.quit_now.set()
    await response.aclose()


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
        self._remux_semaphore = asyncio.Semaphore(min(8, settings.download_concurrency))
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

    async def download(
        self, context: AssetFetchContext, *, compute_sha256: bool = True
    ) -> DownloadedAsset:
        started_at = perf_counter()
        try:
            async with self._semaphore, self._group_limit(context.extraction_id):
                queue_wait = elapsed_ms(started_at)
                if context.audio:
                    result = await self._download_and_remux(context, compute_sha256=compute_sha256)
                else:
                    result = await self._download_single(context, compute_sha256=compute_sha256)
        except Exception as exc:
            log_event(
                logger,
                "media.asset.failed",
                level=logging.WARNING,
                message="Media asset preparation failed",
                platform=context.platform,
                media_type=context.kind,
                uses_separate_audio=context.audio is not None,
                compute_sha256=compute_sha256,
                elapsed_ms=elapsed_ms(started_at),
                error_type=type(exc).__name__,
                success=False,
            )
            raise
        log_event(
            logger,
            "media.asset.completed",
            message="Media asset prepared",
            platform=context.platform,
            media_type=context.kind,
            uses_separate_audio=context.audio is not None,
            compute_sha256=compute_sha256,
            queue_wait_ms=queue_wait,
            output_bytes=result.size,
            content_type=result.content_type,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return result

    async def _download_single(
        self, context: AssetFetchContext, *, compute_sha256: bool = True
    ) -> DownloadedAsset:
        proxy = self._initial_proxy(context)
        last_error: Exception | None = None
        upstream_urls = [context.upstream_url, *context.alternate_upstream_urls]
        for attempt in range(1, self.settings.download_max_retries + 1):
            attempt_started_at = perf_counter()
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
                    compute_sha256=compute_sha256,
                )
                if size == 0:
                    raise _RetryableDownload("Upstream returned an empty asset")
                if expected_length is not None and size != expected_length:
                    raise _RetryableDownload(
                        f"Truncated asset: expected {expected_length} bytes, got {size}"
                    )
                spool.seek(0)
                content_type = detect_content_type(prefix, declared)
                result = DownloadedAsset(
                    file=binary_spool,
                    size=size,
                    sha256=digest,
                    content_type=content_type,
                    declared_content_type=declared,
                )
                log_event(
                    logger,
                    "media.upstream_download.completed",
                    message="Upstream media download completed",
                    platform=context.platform,
                    media_type=context.kind,
                    attempt=attempt,
                    proxy_used=proxy.url is not None,
                    output_bytes=size,
                    content_type=content_type,
                    elapsed_ms=elapsed_ms(attempt_started_at),
                    success=True,
                )
                return result
            except AssetTooLargeError:
                spool.close()
                raise
            except NetworkError as exc:
                spool.close()
                if len(upstream_urls) > 1 and attempt < self.settings.download_max_retries:
                    last_error = exc
                else:
                    log_event(
                        logger,
                        "media.upstream_download.failed",
                        level=logging.WARNING,
                        message="Upstream rejected the media download",
                        platform=context.platform,
                        media_type=context.kind,
                        attempt=attempt,
                        proxy_used=proxy.url is not None,
                        elapsed_ms=elapsed_ms(attempt_started_at),
                        error_type=type(exc).__name__,
                        retrying=False,
                        success=False,
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
                log_event(
                    logger,
                    "media.upstream_download.failed",
                    level=logging.WARNING,
                    message="Upstream media download failed; retrying",
                    platform=context.platform,
                    media_type=context.kind,
                    attempt=attempt,
                    proxy_used=proxy.url is not None,
                    elapsed_ms=elapsed_ms(attempt_started_at),
                    error_type=type(last_error).__name__,
                    retrying=True,
                    success=False,
                )
                await asyncio.sleep(delay)
        log_event(
            logger,
            "media.upstream_download.failed",
            level=logging.WARNING,
            message="Upstream media download exhausted its attempts",
            platform=context.platform,
            media_type=context.kind,
            attempt=self.settings.download_max_retries,
            proxy_used=proxy.url is not None,
            elapsed_ms=elapsed_ms(attempt_started_at),
            error_type=type(last_error).__name__,
            retrying=False,
            success=False,
        )
        if isinstance(last_error, (TimeoutError, httpx.TimeoutException)):
            raise UpstreamTimeoutError("Asset download timed out") from last_error
        raise NetworkError(
            f"Asset download failed after {self.settings.download_max_retries} attempts"
        ) from last_error

    async def _download_and_remux(
        self, context: AssetFetchContext, *, compute_sha256: bool
    ) -> DownloadedAsset:
        audio = context.audio
        if audio is None:
            return await self._download_single(context, compute_sha256=compute_sha256)
        video_context = context.model_copy(update={"audio": None})
        audio_context = AssetFetchContext(
            platform=context.platform,
            upstream_url=audio.upstream_url,
            alternate_upstream_urls=audio.alternate_upstream_urls,
            filename="audio.m4a",
            kind="audio",
            declared_content_type=audio.declared_content_type,
            referer=context.referer,
            cookies=audio.cookies,
            proxy_slot=context.proxy_slot,
            extraction_id=context.extraction_id,
        )
        tasks = [
            asyncio.create_task(self._download_single(video_context, compute_sha256=False)),
            asyncio.create_task(self._download_single(audio_context, compute_sha256=False)),
        ]
        download_started_at = perf_counter()
        try:
            video_asset, audio_asset = await asyncio.gather(*tasks)
        except BaseException as exc:
            for task in tasks:
                task.cancel()
            results = await asyncio.gather(*tasks, return_exceptions=True)
            for result in results:
                if isinstance(result, DownloadedAsset):
                    result.file.close()
            if isinstance(exc, Exception):
                log_event(
                    logger,
                    "media.separate_tracks.failed",
                    level=logging.WARNING,
                    message="Separate video and audio track downloads failed",
                    platform=context.platform,
                    elapsed_ms=elapsed_ms(download_started_at),
                    error_type=type(exc).__name__,
                    success=False,
                )
            raise
        log_event(
            logger,
            "media.separate_tracks.completed",
            message="Separate video and audio tracks downloaded",
            platform=context.platform,
            output_bytes=video_asset.size + audio_asset.size,
            elapsed_ms=elapsed_ms(download_started_at),
            success=True,
        )
        try:
            if (
                self.settings.max_asset_bytes
                and video_asset.size + audio_asset.size > self.settings.max_asset_bytes
            ):
                raise AssetTooLargeError("Remuxed asset exceeds MAX_ASSET_BYTES")
            remux_started_at = perf_counter()
            try:
                result = await self._remux_copy(
                    video_asset.file, audio_asset.file, compute_sha256=compute_sha256
                )
            except Exception as exc:
                log_event(
                    logger,
                    "media.remux.failed",
                    level=logging.WARNING,
                    message="FFmpeg stream-copy remux failed",
                    platform=context.platform,
                    elapsed_ms=elapsed_ms(remux_started_at),
                    error_type=type(exc).__name__,
                    success=False,
                )
                raise
            log_event(
                logger,
                "media.remux.completed",
                message="FFmpeg stream-copy remux completed",
                platform=context.platform,
                output_bytes=result.size,
                elapsed_ms=elapsed_ms(remux_started_at),
                success=True,
            )
            return result
        finally:
            video_asset.file.close()
            audio_asset.file.close()

    async def _remux_copy(
        self, video: BinaryIO, audio: BinaryIO, *, compute_sha256: bool = True
    ) -> DownloadedAsset:
        queue_started_at = perf_counter()
        async with self._remux_semaphore:
            log_event(
                logger,
                "media.remux.queue_acquired",
                message="FFmpeg remux worker acquired",
                queue_wait_ms=elapsed_ms(queue_started_at),
                success=True,
            )
            output = tempfile.SpooledTemporaryFile(
                max_size=self.settings.spool_threshold_bytes, mode="w+b"
            )
            binary_output = cast(BinaryIO, output)
            video.seek(0)
            audio.seek(0)
            video_fd = video.fileno()
            audio_fd = audio.fileno()
            output_fd = output.fileno()
            try:
                process = await asyncio.create_subprocess_exec(
                    "ffmpeg",
                    "-nostdin",
                    "-hide_banner",
                    "-loglevel",
                    "error",
                    "-y",
                    "-i",
                    f"/proc/self/fd/{video_fd}",
                    "-i",
                    f"/proc/self/fd/{audio_fd}",
                    "-map",
                    "0:v:0",
                    "-map",
                    "1:a:0",
                    "-c",
                    "copy",
                    "-shortest",
                    "-movflags",
                    "+faststart",
                    "-f",
                    "mp4",
                    f"/proc/self/fd/{output_fd}",
                    stdout=asyncio.subprocess.DEVNULL,
                    stderr=asyncio.subprocess.PIPE,
                    pass_fds=(video_fd, audio_fd, output_fd),
                )
            except FileNotFoundError as exc:
                output.close()
                raise NetworkError("Media remuxer is unavailable") from exc
            try:
                _, stderr = await asyncio.wait_for(
                    process.communicate(),
                    timeout=self.settings.upstream_download_timeout_seconds,
                )
            except TimeoutError as exc:
                process.kill()
                await process.communicate()
                output.close()
                raise UpstreamTimeoutError("Media remux timed out") from exc
            if process.returncode != 0:
                output.close()
                detail = stderr.decode("utf-8", "replace").strip().splitlines()
                message = detail[-1][:300] if detail else "unknown ffmpeg error"
                raise NetworkError(f"Media remux failed: {message}")

            output.seek(0, 2)
            size = output.tell()
            if self.settings.max_asset_bytes and size > self.settings.max_asset_bytes:
                output.close()
                raise AssetTooLargeError("Remuxed asset exceeds MAX_ASSET_BYTES")
            digest_value: str | None = None
            prefix = bytearray()
            if compute_sha256:
                digest = hashlib.sha256()
                output.seek(0)
                while chunk := output.read(self.settings.download_chunk_bytes):
                    if len(prefix) < 32:
                        prefix.extend(chunk[: 32 - len(prefix)])
                    digest.update(chunk)
                digest_value = digest.hexdigest()
            else:
                output.seek(0)
                prefix.extend(output.read(32))
            if size == 0:
                output.close()
                raise NetworkError("Media remux produced an empty asset")
            output.seek(0)
            return DownloadedAsset(
                file=binary_output,
                size=size,
                sha256=digest_value,
                content_type=detect_content_type(bytes(prefix), "video/mp4"),
                declared_content_type="video/mp4",
            )

    async def _download_once(
        self,
        context: AssetFetchContext,
        proxy: ProxyChoice,
        spool: BinaryIO,
        upstream_url: str,
        *,
        compute_sha256: bool,
    ) -> tuple[str | None, int | None, str | None, int, bytes]:
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
        digest = hashlib.sha256() if compute_sha256 else None
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
                if digest is not None:
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
                    await _close_curl_response(curl_response)
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
        return (
            declared,
            expected,
            digest.hexdigest() if digest is not None else None,
            size,
            bytes(prefix),
        )

    async def close(self) -> None:
        await self._http.aclose()
        with self._curl_lock:
            sessions = list(self._curl_sessions.values())
            self._curl_sessions.clear()
        for session in sessions:
            await session.close()
