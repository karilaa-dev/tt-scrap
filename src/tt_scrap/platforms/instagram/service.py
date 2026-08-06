"""Instagram RapidAPI extraction and response normalization."""

from __future__ import annotations

import asyncio
import re
from datetime import UTC, datetime, timedelta
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
from ...models import (
    AssetFetchContext,
    InstagramExtractionResponse,
    InstagramMediaItem,
)

_PATH_RE = re.compile(r"^/(?:p|reels?|tv|stories)/[\w-]+", re.IGNORECASE)
_RAPIDAPI_HOST = "instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com"


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


class InstagramService:
    def __init__(self, settings: Settings, cache: CacheStore) -> None:
        self.settings = settings
        self.cache = cache
        self.assets = AssetFactory(cache)
        self._semaphore = asyncio.Semaphore(settings.extraction_concurrency)
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

    async def _rapidapi(self, source_url: str) -> dict[str, Any]:
        key = self.settings.rapidapi_key.get_secret_value()
        if not key:
            raise ExtractionError("RAPIDAPI_KEY is not configured")
        last_error: Exception | None = None
        last_status: int | None = None
        async with self._semaphore:
            for attempt in range(1, self.settings.instagram_max_attempts + 1):
                try:
                    response = await self._http.get(
                        f"https://{_RAPIDAPI_HOST}/convert",
                        params={"url": source_url},
                        headers={
                            "X-Rapidapi-Key": key,
                            "X-Rapidapi-Host": _RAPIDAPI_HOST,
                        },
                    )
                    last_status = response.status_code
                    if response.status_code == 404:
                        raise ContentDeletedError("Instagram post was not found or is private")
                    if response.status_code == 429:
                        raise RateLimitError("Instagram API rate limit exceeded")
                    if response.status_code >= 500:
                        raise NetworkError("Instagram API is unavailable")
                    if response.status_code != 200:
                        raise NetworkError(f"Instagram API returned HTTP {response.status_code}")
                    payload = response.json()
                    if not isinstance(payload, dict):
                        raise ExtractionError("Instagram API returned an invalid payload")
                    return payload
                except ContentDeletedError:
                    raise
                except (RateLimitError, NetworkError, httpx.HTTPError, ValueError) as exc:
                    last_error = exc
                if attempt < self.settings.instagram_max_attempts:
                    await asyncio.sleep(self.settings.instagram_retry_delay_seconds)
        if last_status == 429:
            raise RateLimitError("Instagram API rate limit exceeded") from last_error
        raise NetworkError("Instagram extraction failed after retries") from last_error

    async def extract_url(
        self, source_url: str, *, refresh: bool = False
    ) -> InstagramExtractionResponse:
        normalized_url = normalize_instagram_url(source_url)
        media_id = extract_instagram_media_id(normalized_url)
        cache_key = self.cache.metadata_key("instagram", normalized_url)
        if not refresh:
            cached = await self.cache.get_model(cache_key, InstagramExtractionResponse)
            if cached:
                return cached.model_copy(update={"source_url": source_url})
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
            source_url=source_url,
            content_type=content_type,
            media=media,
            expires_at=expires_at,
        )
        await self.cache.set_model(cache_key, response)
        await self.cache.set_model(
            self.cache.metadata_key("instagram-extraction", extraction_id),
            response,
        )
        return response

    async def get_extraction(self, extraction_id: str) -> InstagramExtractionResponse:
        cached = await self.cache.get_model(
            self.cache.metadata_key("instagram-extraction", extraction_id),
            InstagramExtractionResponse,
        )
        if cached is None:
            raise ExtractionExpiredError("Instagram extraction was not found or has expired")
        return cached

    async def close(self) -> None:
        await self._http.aclose()
