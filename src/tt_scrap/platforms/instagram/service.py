"""Instagram RapidAPI extraction and response normalization."""

from __future__ import annotations

import asyncio
import logging
import random
import re
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from time import perf_counter
from typing import Any, Literal
from urllib.parse import urlparse
from uuid import uuid4

import httpx

from ...assets import AssetFactory
from ...cache import CacheStore
from ...config import Settings
from ...errors import (
    ContentDeletedError,
    ExtractionError,
    ExtractionExpiredError,
    InvalidLinkError,
    NetworkError,
    RateLimitError,
)
from ...logging import bind_request_context, elapsed_ms, log_event, record_recovery
from ...models import (
    AssetFetchContext,
    InstagramExtractionResponse,
    InstagramMediaItem,
)

_PATH_RE = re.compile(r"^/(?:p|reels?|tv|stories)/[\w-]+", re.IGNORECASE)
_RAPIDAPI_HOST = "instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com"

logger = logging.getLogger(__name__)


def validate_instagram_url(url: str) -> None:
    parsed = urlparse(url)
    host = (parsed.hostname or "").rstrip(".").lower()
    if parsed.scheme != "https" or host not in {"instagram.com", "www.instagram.com"}:
        raise InvalidLinkError("Only HTTPS Instagram URLs are accepted")
    if not _PATH_RE.match(parsed.path):
        raise InvalidLinkError("Unsupported Instagram URL")


def normalize_instagram_url(url: str) -> str:
    validate_instagram_url(url)
    parsed = urlparse(url)
    return f"https://www.instagram.com{parsed.path.rstrip('/')}/"


def extract_instagram_media_id(url: str) -> str:
    """Return the post shortcode or story ID used in an Instagram URL."""
    validate_instagram_url(url)
    path_parts = [part for part in urlparse(url).path.split("/") if part]
    if path_parts[0].lower() == "stories" and len(path_parts) >= 3:
        return path_parts[2]
    return path_parts[1]


def extract_creator_username(payload: dict[str, Any]) -> str | None:
    """Read known provider shapes without deriving identity from the URL."""
    candidates: list[Any] = [payload.get("username")]
    for key in ("owner", "author", "user"):
        nested = payload.get(key)
        if isinstance(nested, dict):
            candidates.append(nested.get("username"))
    for value in candidates:
        if isinstance(value, str) and value.strip():
            return value.strip()
    return None


class InstagramService:
    def __init__(self, settings: Settings, cache: CacheStore) -> None:
        self.settings = settings
        self.cache = cache
        self.assets = AssetFactory(cache)
        # The upstream currently accepts four simultaneous conversions. A
        # dedicated limit prevents larger TikTok-oriented worker settings from
        # creating immediate, wasteful Instagram 429 responses.
        self._semaphore = asyncio.Semaphore(
            min(settings.instagram_concurrency, settings.extraction_concurrency)
        )
        self._key_lock_guard = asyncio.Lock()
        self._key_locks: dict[str, tuple[asyncio.Lock, int]] = {}
        self._http = httpx.AsyncClient(
            follow_redirects=True,
            timeout=httpx.Timeout(settings.instagram_request_timeout_seconds),
            limits=httpx.Limits(
                max_connections=settings.http_max_connections,
                max_keepalive_connections=settings.http_max_connections,
            ),
            headers={"User-Agent": "Mozilla/5.0"},
        )

    def _expires_at(self) -> datetime:
        return datetime.now(UTC) + timedelta(seconds=self.settings.cache_ttl_seconds)

    @asynccontextmanager
    async def _key_lock(self, key: str) -> AsyncIterator[bool]:
        """Serialize one post and report whether this call joined existing work."""
        async with self._key_lock_guard:
            lock, users = self._key_locks.get(key, (asyncio.Lock(), 0))
            joined = users > 0
            self._key_locks[key] = (lock, users + 1)
        acquired = False
        try:
            await lock.acquire()
            acquired = True
            yield joined
        finally:
            if acquired:
                lock.release()
            async with self._key_lock_guard:
                current, users = self._key_locks[key]
                if users == 1:
                    del self._key_locks[key]
                else:
                    self._key_locks[key] = (current, users - 1)

    async def _rapidapi(self, source_url: str) -> dict[str, Any]:
        key = self.settings.rapidapi_key.get_secret_value()
        if not key:
            raise ExtractionError("RAPIDAPI_KEY is not configured")

        async def attempt_loop() -> dict[str, Any]:
            last_error: Exception | None = None
            last_status: int | None = None
            retry_budget = min(5.0, self.settings.instagram_request_timeout_seconds)
            backoff_spent = 0.0
            for attempt in range(1, self.settings.instagram_max_attempts + 1):
                if attempt > 1:
                    record_recovery("retry_count")
                attempt_started_at = perf_counter()
                attempt_status: int | None = None
                attempt_retry_after: float | None = None
                try:
                    async with self._semaphore:
                        response = await self._http.get(
                            f"https://{_RAPIDAPI_HOST}/convert",
                            params={"url": source_url},
                            headers={
                                "X-Rapidapi-Key": key,
                                "X-Rapidapi-Host": _RAPIDAPI_HOST,
                            },
                        )
                    last_status = response.status_code
                    attempt_status = response.status_code
                    if response.status_code == 404:
                        raise ContentDeletedError("Instagram post was not found or is private")
                    if response.status_code == 429:
                        try:
                            parsed_retry_after = float(response.headers.get("Retry-After", ""))
                            if parsed_retry_after >= 0:
                                attempt_retry_after = min(parsed_retry_after, retry_budget)
                        except ValueError:
                            pass
                        raise RateLimitError("Instagram API rate limit exceeded")
                    if response.status_code >= 500:
                        raise NetworkError("Instagram API is unavailable")
                    if response.status_code != 200:
                        raise NetworkError(f"Instagram API returned HTTP {response.status_code}")
                    payload = response.json()
                    if not isinstance(payload, dict):
                        raise ExtractionError("Instagram API returned an invalid payload")
                    log_event(
                        logger,
                        "instagram.upstream.completed",
                        level=logging.DEBUG,
                        message="Instagram metadata request completed",
                        attempt=attempt,
                        status_code=response.status_code,
                        elapsed_ms=elapsed_ms(attempt_started_at),
                        success=True,
                    )
                    return payload
                except ContentDeletedError as exc:
                    log_event(
                        logger,
                        "instagram.upstream.failed",
                        level=logging.DEBUG,
                        message="Instagram content was not available",
                        attempt=attempt,
                        status_code=attempt_status,
                        elapsed_ms=elapsed_ms(attempt_started_at),
                        error_type=type(exc).__name__,
                        retrying=False,
                        success=False,
                    )
                    raise
                except ExtractionError as exc:
                    log_event(
                        logger,
                        "instagram.upstream.failed",
                        level=logging.DEBUG,
                        message="Instagram API returned an invalid payload",
                        attempt=attempt,
                        status_code=attempt_status,
                        elapsed_ms=elapsed_ms(attempt_started_at),
                        error_type=type(exc).__name__,
                        retrying=False,
                        success=False,
                    )
                    raise
                except (RateLimitError, NetworkError, httpx.HTTPError, ValueError) as exc:
                    last_error = exc
                    log_event(
                        logger,
                        "instagram.upstream.failed",
                        level=logging.DEBUG,
                        message="Instagram metadata request failed",
                        attempt=attempt,
                        status_code=attempt_status,
                        elapsed_ms=elapsed_ms(attempt_started_at),
                        error_type=type(exc).__name__,
                        retrying=attempt < self.settings.instagram_max_attempts,
                        success=False,
                    )
                if attempt < self.settings.instagram_max_attempts:
                    delay = self.settings.instagram_retry_delay_seconds * (2 ** (attempt - 1))
                    if attempt_status == 429:
                        # RapidAPI does not consistently return Retry-After for
                        # this provider. Give active conversions time to finish
                        # instead of burning every retry in a few milliseconds.
                        delay = max(delay, attempt_retry_after or 1.0)
                    delay += random.random() * min(delay * 0.25, 0.25)
                    remaining_backoff = retry_budget - backoff_spent
                    if remaining_backoff <= 0:
                        break
                    delay = min(delay, remaining_backoff)
                    await asyncio.sleep(delay)
                    backoff_spent += delay
            if last_status == 429:
                raise RateLimitError("Instagram API rate limit exceeded") from last_error
            raise NetworkError("Instagram extraction failed after retries") from last_error

        return await attempt_loop()

    async def extract_url(
        self, source_url: str, *, refresh: bool = False
    ) -> InstagramExtractionResponse:
        started_at = perf_counter()
        normalized_url = normalize_instagram_url(source_url)
        media_id = extract_instagram_media_id(normalized_url)
        bind_request_context(platform="instagram", source_id=media_id)
        cache_key = self.cache.metadata_key("instagram", normalized_url)
        baseline_generation = await self.cache.get_generation(cache_key) if refresh else None
        if not refresh:
            cached = await self.cache.get_model(cache_key, InstagramExtractionResponse)
            if cached:
                response = self._cached_response(
                    cached,
                    source_url,
                    media_id,
                    started_at,
                    cache_scope="url",
                )
                return response

        async with self._key_lock(cache_key) as joined:
            # Recheck after joining in-flight work. This also coalesces concurrent
            # refresh requests while ensuring a later, non-overlapping refresh
            # still performs a new provider call.
            refreshed_by_joined_request = (
                joined
                and refresh
                and await self.cache.get_generation(cache_key) != baseline_generation
            )
            if not refresh or refreshed_by_joined_request:
                cached = await self.cache.get_model(cache_key, InstagramExtractionResponse)
                if cached:
                    return self._cached_response(
                        cached,
                        source_url,
                        media_id,
                        started_at,
                        cache_scope="url_coalesced",
                    )
            return await self._extract_uncached(
                normalized_url,
                source_url,
                media_id,
                cache_key,
                started_at,
            )

    @staticmethod
    def _cached_response(
        cached: InstagramExtractionResponse,
        source_url: str,
        media_id: str,
        started_at: float,
        *,
        cache_scope: str,
    ) -> InstagramExtractionResponse:
        response = cached.model_copy(update={"source_url": source_url})
        bind_request_context(
            platform="instagram",
            source_id=media_id,
            cache_hit=True,
            cache_scope=cache_scope,
            media_count=len(response.media),
        )
        log_event(
            logger,
            "instagram.extraction.completed",
            message="Instagram extraction served from cache",
            platform="instagram",
            source_id=media_id,
            cache_hit=True,
            cache_scope=cache_scope,
            media_count=len(response.media),
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return response

    async def _extract_uncached(
        self,
        normalized_url: str,
        source_url: str,
        media_id: str,
        cache_key: str,
        started_at: float,
    ) -> InstagramExtractionResponse:
        payload = await self._rapidapi(normalized_url)
        raw_media = payload.get("media") or []
        if not isinstance(raw_media, list) or not raw_media:
            raise ContentDeletedError("Instagram response contained no media")

        expires_at = self._expires_at()
        extraction_id = str(uuid4())
        media: list[InstagramMediaItem] = []
        usable_media: list[dict[str, Any]] = []
        for raw in raw_media:
            if not isinstance(raw, dict) or not raw.get("url"):
                continue
            usable_media.append(raw)
        if not usable_media:
            raise ContentDeletedError("Instagram response contained no usable media")

        is_carousel = len(usable_media) > 1
        for position, raw in enumerate(usable_media):
            media_type: Literal["video", "image"] = (
                "video" if raw.get("type") == "video" else "image"
            )
            extension = "mp4" if media_type == "video" else "jpg"
            filename_stem = f"{media_id}_{position + 1}" if is_carousel else media_id
            asset = await self.assets.create(
                AssetFetchContext(
                    platform="instagram",
                    upstream_url=str(raw["url"]),
                    filename=f"{filename_stem}.{extension}",
                    kind=media_type,
                    extraction_id=extraction_id,
                    declared_content_type=("video/mp4" if media_type == "video" else None),
                ),
                position=position,
                expires_at=expires_at,
            )
            thumbnail = None
            if raw.get("thumbnail"):
                thumbnail = await self.assets.create(
                    AssetFetchContext(
                        platform="instagram",
                        upstream_url=str(raw["thumbnail"]),
                        filename=f"{filename_stem}_thumbnail.jpg",
                        kind="thumbnail",
                        extraction_id=extraction_id,
                    ),
                    position=position,
                    expires_at=expires_at,
                )
            media.append(
                InstagramMediaItem(
                    position=position,
                    media_type=media_type,
                    quality=str(raw["quality"]) if raw.get("quality") else None,
                    asset=asset,
                    thumbnail=thumbnail,
                )
            )
        if len(media) > 1:
            content_type: Literal["video", "image", "carousel"] = "carousel"
        else:
            content_type = "video" if media[0].media_type == "video" else "image"
        response = InstagramExtractionResponse(
            extraction_id=extraction_id,
            source_id=media_id,
            source_url=source_url,
            creator_username=extract_creator_username(payload),
            content_type=content_type,
            media=media,
            expires_at=expires_at,
        )
        await self.cache.set_model(cache_key, response)
        await self.cache.set_model(
            self.cache.metadata_key("instagram-extraction", extraction_id),
            response,
        )
        bind_request_context(
            platform="instagram",
            source_id=media_id,
            cache_hit=False,
            cache_scope=None,
            media_count=len(response.media),
        )
        log_event(
            logger,
            "instagram.extraction.completed",
            message="Instagram extraction completed",
            platform="instagram",
            source_id=media_id,
            cache_hit=False,
            media_count=len(response.media),
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return response

    async def get_extraction(self, extraction_id: str) -> InstagramExtractionResponse:
        started_at = perf_counter()
        cached = await self.cache.get_model(
            self.cache.metadata_key("instagram-extraction", extraction_id),
            InstagramExtractionResponse,
        )
        if cached is None:
            bind_request_context(
                platform="instagram",
                cache_hit=False,
                cache_scope="extraction_id",
            )
            log_event(
                logger,
                "instagram.extraction_cache.lookup",
                level=logging.DEBUG,
                message="Instagram extraction cache lookup missed",
                platform="instagram",
                cache_hit=False,
                cache_scope="extraction_id",
                elapsed_ms=elapsed_ms(started_at),
                success=False,
            )
            raise ExtractionExpiredError("Instagram extraction was not found or has expired")
        bind_request_context(
            platform="instagram",
            source_id=cached.source_id,
            cache_hit=True,
            cache_scope="extraction_id",
            media_count=len(cached.media),
        )
        log_event(
            logger,
            "instagram.extraction_cache.lookup",
            level=logging.DEBUG,
            message="Instagram extraction cache lookup completed",
            platform="instagram",
            cache_hit=True,
            cache_scope="extraction_id",
            media_count=len(cached.media),
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return cached

    async def close(self) -> None:
        await self._http.aclose()
