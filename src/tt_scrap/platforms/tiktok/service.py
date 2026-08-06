"""TikTok response normalization and cache/session creation."""

from __future__ import annotations

import asyncio
import json
import logging
import math
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from time import perf_counter
from typing import Any, Literal
from uuid import uuid4

from ...assets import AssetFactory
from ...cache import CacheStore
from ...config import Settings
from ...errors import ContentTooLongError, ExtractionError, ExtractionExpiredError
from ...logging import elapsed_ms, log_event
from ...models import (
    AssetDescriptor,
    AssetFetchContext,
    AuxiliaryAssetFetchContext,
    TikTokExtractionResponse,
    TikTokMusicMetadata,
    TikTokMusicResponse,
)
from ...proxy import ProxyManager, ProxySession
from .adapter import TikTokAdapter, YtdlpContext, validate_tiktok_url

logger = logging.getLogger(__name__)


def _first(value: Any) -> str | None:
    if isinstance(value, str) and value:
        return value
    if isinstance(value, list):
        return next((item for item in value if isinstance(item, str) and item), None)
    return None


@dataclass(frozen=True, slots=True)
class VideoSource:
    url: str
    alternate_urls: list[str]
    width: int | None = None
    height: int | None = None
    audio_url: str | None = None
    alternate_audio_urls: list[str] | None = None


@dataclass(frozen=True, slots=True)
class _BitrateVideoCandidate:
    source: VideoSource
    mvmaf: float | None
    known_quality: bool
    quality_type: int
    bitrate: int


def _positive_int(value: Any) -> int | None:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed > 0 else None


def _address_urls(address: Any) -> list[str]:
    if isinstance(address, str):
        return [address] if address else []
    if isinstance(address, list):
        return [item for item in address if isinstance(item, str) and item]
    if not isinstance(address, dict):
        return []
    for key in ("UrlList", "urlList", "url_list"):
        urls = address.get(key)
        if isinstance(urls, list):
            return [item for item in urls if isinstance(item, str) and item]
    mirror_urls: list[str] = []
    for key in (
        "MainUrl",
        "main_url",
        "BackupUrl",
        "backup_url",
        "FallbackUrl",
        "fallback_url",
    ):
        value = address.get(key)
        if isinstance(value, str) and value:
            mirror_urls.append(value)
    if mirror_urls:
        return mirror_urls
    return [url for key in ("src", "url", "download") if (url := _first(address.get(key)))]


_BEST_VIDEO_GEAR = "adapt_lowest_1080_1"
_BEST_VIDEO_URL_TAG = "bytevc1_1080p"


def _best_audio_source(video: dict[str, Any]) -> tuple[str, list[str]] | None:
    tracks = video.get("bitrateAudioInfo") or video.get("bit_rate_audio_info") or []
    candidates: list[tuple[tuple[int, int], list[str]]] = []
    for track in tracks:
        if not isinstance(track, dict):
            continue
        urls = _address_urls(track.get("UrlList") or track.get("url_list"))
        if not urls:
            continue
        rank = (
            _positive_int(track.get("Bitrate") or track.get("bit_rate")) or 0,
            _positive_int(track.get("AudioQuality") or track.get("audio_quality")) or 0,
        )
        candidates.append((rank, urls))
    if not candidates:
        return None
    urls = max(candidates, key=lambda candidate: candidate[0])[1]
    return urls[0], urls[1:]


def _regular_video_source(video: dict[str, Any]) -> VideoSource | None:
    for key in ("playAddr", "downloadAddr"):
        urls = _address_urls(video.get(key))
        if urls:
            return VideoSource(
                url=urls[0],
                alternate_urls=urls[1:],
                width=_positive_int(video.get("width")),
                height=_positive_int(video.get("height")),
            )
    return None


def _pixel_area(source: VideoSource | None) -> int:
    if source is None or source.width is None or source.height is None:
        return 0
    return source.width * source.height


def _mvmaf_score(raw_bitrate: dict[str, Any], source: VideoSource) -> float | None:
    """Return TikTok's precomputed original-reference score for this resolution."""
    payload = raw_bitrate.get("MVMAF")
    if payload is None:
        payload = raw_bitrate.get("mvmaf")
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except (json.JSONDecodeError, TypeError):
            return None
    if not isinstance(payload, dict):
        return None

    version = payload.get("v2.0")
    if not isinstance(version, dict):
        version = payload
    scores = version.get("ori")
    if not isinstance(scores, dict):
        # Some responses omit the original-reference branch. srv1 remains a
        # useful precomputed perceptual score and is preferable to bitrate.
        scores = version.get("srv1")
    if not isinstance(scores, dict):
        return None

    parsed_scores: dict[int, float] = {}
    for raw_target, raw_score in scores.items():
        target = str(raw_target).lower()
        if not target.startswith("v"):
            continue
        try:
            target_pixels = int(target[1:])
            score = float(raw_score)
        except (TypeError, ValueError):
            continue
        if target_pixels > 0 and math.isfinite(score):
            parsed_scores[target_pixels] = score
    if not parsed_scores or source.width is None or source.height is None:
        return None

    short_edge = min(source.width, source.height)
    closest_target = min(
        parsed_scores,
        key=lambda target: (abs(target - short_edge), -target),
    )
    return parsed_scores[closest_target]


def select_video_source(video: dict[str, Any]) -> VideoSource | None:
    regular = _regular_video_source(video)
    audio = _best_audio_source(video)
    has_separate_audio = bool(video.get("bitrateAudioInfo") or video.get("bit_rate_audio_info"))
    candidates: list[_BitrateVideoCandidate] = []
    for raw_bitrate in video.get("bitrateInfo", []):
        if not isinstance(raw_bitrate, dict):
            continue
        address = raw_bitrate.get("PlayAddr") or raw_bitrate.get("play_addr")
        if not isinstance(address, dict):
            continue
        gear_name = str(raw_bitrate.get("GearName") or raw_bitrate.get("gear_name") or "").lower()
        url_key = str(address.get("UrlKey") or address.get("url_key") or "").lower()
        urls = _address_urls(address)
        if not urls:
            continue
        known_muxed = gear_name.startswith("normal_")
        # An adaptive file accompanied by malformed separate-audio metadata is
        # not a complete delivery candidate. A normal_* representation is a
        # known muxed file and remains usable without the separate track.
        if has_separate_audio and not known_muxed and not audio:
            continue
        needs_separate_audio = has_separate_audio and not known_muxed
        source = VideoSource(
            url=urls[0],
            alternate_urls=urls[1:],
            width=_positive_int(address.get("Width") or address.get("width")),
            height=_positive_int(address.get("Height") or address.get("height")),
            audio_url=audio[0] if audio and needs_separate_audio else None,
            alternate_audio_urls=audio[1] if audio and needs_separate_audio else None,
        )
        candidates.append(
            _BitrateVideoCandidate(
                source=source,
                mvmaf=_mvmaf_score(raw_bitrate, source),
                known_quality=(gear_name == _BEST_VIDEO_GEAR or _BEST_VIDEO_URL_TAG in url_key),
                quality_type=(
                    _positive_int(raw_bitrate.get("QualityType") or raw_bitrate.get("quality_type"))
                    or 10_000
                ),
                bitrate=(
                    _positive_int(raw_bitrate.get("Bitrate") or raw_bitrate.get("bit_rate")) or 0
                ),
            )
        )

    if not candidates:
        return regular
    maximum_candidate_area = max(_pixel_area(candidate.source) for candidate in candidates)
    regular_area = _pixel_area(regular)
    if regular is not None and regular_area > maximum_candidate_area:
        return regular
    maximum_resolution = [
        candidate
        for candidate in candidates
        if _pixel_area(candidate.source) == maximum_candidate_area
    ]
    # The top-level playAddr has no per-option MVMAF metadata. Retain it as the
    # same-resolution fallback only when TikTok supplied no usable MVMAF score.
    if (
        regular is not None
        and regular_area == maximum_candidate_area
        and not any(candidate.mvmaf is not None for candidate in maximum_resolution)
    ):
        return regular
    selected = max(
        maximum_resolution,
        key=lambda candidate: (
            candidate.mvmaf is not None,
            candidate.mvmaf if candidate.mvmaf is not None else -math.inf,
            candidate.known_quality,
            -candidate.quality_type,
            candidate.bitrate,
        ),
    )
    return selected.source


def extract_video_url(video: dict[str, Any]) -> str | None:
    source = select_video_source(video)
    return source.url if source else None


class TikTokService:
    def __init__(
        self,
        settings: Settings,
        cache: CacheStore,
        proxy_manager: ProxyManager,
    ) -> None:
        self.settings = settings
        self.cache = cache
        self.assets = AssetFactory(cache)
        self.adapter = TikTokAdapter(settings, proxy_manager)
        self.proxy_manager = proxy_manager
        self._key_lock_guard = asyncio.Lock()
        self._key_locks: dict[str, tuple[asyncio.Lock, int]] = {}

    def _ensure_key_locks(self) -> None:
        # A few unit tests construct the service without __init__ to inject a
        # fake adapter. Keep the synchronization state lazy for those callers.
        if not hasattr(self, "_key_lock_guard"):
            self._key_lock_guard = asyncio.Lock()
            self._key_locks = {}

    @asynccontextmanager
    async def _key_lock(self, key: str) -> AsyncIterator[None]:
        self._ensure_key_locks()
        async with self._key_lock_guard:
            lock, users = self._key_locks.get(key, (asyncio.Lock(), 0))
            self._key_locks[key] = (lock, users + 1)
        acquired = False
        try:
            await lock.acquire()
            acquired = True
            yield
        finally:
            if acquired:
                lock.release()
            async with self._key_lock_guard:
                current, users = self._key_locks[key]
                if users == 1:
                    del self._key_locks[key]
                else:
                    self._key_locks[key] = (current, users - 1)

    def _expires_at(self) -> datetime:
        return datetime.now(UTC) + timedelta(seconds=self.settings.cache_ttl_seconds)

    @staticmethod
    def _log_extraction(
        response: TikTokExtractionResponse,
        started_at: float,
        *,
        cache_hit: bool,
        cache_scope: str | None = None,
    ) -> None:
        log_event(
            logger,
            "tiktok.extraction.completed",
            message=(
                "TikTok extraction served from cache"
                if cache_hit
                else "TikTok extraction completed"
            ),
            platform="tiktok",
            source_id=response.source_id,
            cache_hit=cache_hit,
            cache_scope=cache_scope,
            media_count=len(response.media),
            media_type=response.content_type,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )

    async def _asset(
        self,
        *,
        url: str,
        alternate_urls: list[str] | None = None,
        context: YtdlpContext,
        kind: str,
        filename: str,
        position: int,
        expires_at: datetime,
        extraction_id: str,
        declared_content_type: str | None = None,
        duration: int | None = None,
        audio_url: str | None = None,
        alternate_audio_urls: list[str] | None = None,
    ) -> AssetDescriptor:
        audio = None
        if audio_url:
            audio = AuxiliaryAssetFetchContext(
                upstream_url=audio_url,
                alternate_upstream_urls=alternate_audio_urls or [],
                declared_content_type="audio/mp4",
                cookies=context.cookies_for(audio_url),
            )
        fetch = AssetFetchContext(
            platform="tiktok",
            upstream_url=url,
            alternate_upstream_urls=alternate_urls or [],
            filename=filename,
            kind=kind,  # type: ignore[arg-type]
            declared_content_type=declared_content_type,
            referer=context.referer_url,
            cookies=context.cookies_for(url),
            proxy_slot=context.proxy_slot,
            duration_seconds=duration,
            extraction_id=extraction_id,
            audio=audio,
        )
        return await self.assets.create(fetch, position=position, expires_at=expires_at)

    async def extract_url(
        self, source_url: str, *, refresh: bool = False
    ) -> TikTokExtractionResponse:
        started_at = perf_counter()
        source_url = source_url.strip()
        validate_tiktok_url(source_url)
        url_cache_key = self.cache.metadata_key("tiktok-url", source_url)
        if not refresh:
            cached = await self.cache.get_model(url_cache_key, TikTokExtractionResponse)
            if cached:
                self._log_extraction(cached, started_at, cache_hit=True, cache_scope="url")
                return cached

        async with self._key_lock(url_cache_key):
            if not refresh:
                cached = await self.cache.get_model(url_cache_key, TikTokExtractionResponse)
                if cached:
                    self._log_extraction(
                        cached,
                        started_at,
                        cache_hit=True,
                        cache_scope="url_coalesced",
                    )
                    return cached

            proxy_session = ProxySession(self.proxy_manager)
            resolved_url = await self.adapter.resolve_url(source_url, proxy_session)
            video_id = self.adapter.extract_id(resolved_url)
            video_cache_key = self.cache.metadata_key("tiktok", video_id)
            if not refresh:
                cached, remaining_ttl = await self.cache.get_model_with_ttl(
                    video_cache_key, TikTokExtractionResponse
                )
                if cached and remaining_ttl:
                    response = cached.model_copy(
                        update={"source_url": source_url, "resolved_url": resolved_url}
                    )
                    await self.cache.set_model(
                        url_cache_key,
                        response,
                        ttl_seconds=remaining_ttl,
                    )
                    self._log_extraction(
                        response,
                        started_at,
                        cache_hit=True,
                        cache_scope="video_id",
                    )
                    return response

            async with self._key_lock(video_cache_key):
                if not refresh:
                    cached, remaining_ttl = await self.cache.get_model_with_ttl(
                        video_cache_key, TikTokExtractionResponse
                    )
                    if cached and remaining_ttl:
                        response = cached.model_copy(
                            update={"source_url": source_url, "resolved_url": resolved_url}
                        )
                        await self.cache.set_model(
                            url_cache_key,
                            response,
                            ttl_seconds=remaining_ttl,
                        )
                        self._log_extraction(
                            response,
                            started_at,
                            cache_hit=True,
                            cache_scope="video_id_coalesced",
                        )
                        return response

                extraction_url = f"https://www.tiktok.com/@_/video/{video_id}"
                data, context = await self.adapter.extract(extraction_url, video_id, proxy_session)
                try:
                    response = await self._build_video_response(
                        data, context, video_id, source_url, resolved_url
                    )
                finally:
                    context.close()
                ttl = self.settings.tiktok_info_cache_ttl_seconds
                await self.cache.set_model(video_cache_key, response, ttl_seconds=ttl)
                await self.cache.set_model(url_cache_key, response, ttl_seconds=ttl)
                await self.cache.set_model(
                    self.cache.metadata_key("tiktok-extraction", response.extraction_id),
                    response,
                    ttl_seconds=ttl,
                )
                self._log_extraction(response, started_at, cache_hit=False)
                return response

    async def get_extraction(self, extraction_id: str) -> TikTokExtractionResponse:
        started_at = perf_counter()
        cached = await self.cache.get_model(
            self.cache.metadata_key("tiktok-extraction", extraction_id),
            TikTokExtractionResponse,
        )
        if cached is None:
            log_event(
                logger,
                "tiktok.extraction_cache.lookup",
                level=logging.WARNING,
                message="TikTok extraction cache lookup missed",
                platform="tiktok",
                cache_hit=False,
                cache_scope="extraction_id",
                elapsed_ms=elapsed_ms(started_at),
                success=False,
            )
            raise ExtractionExpiredError("TikTok extraction was not found or has expired")
        log_event(
            logger,
            "tiktok.extraction_cache.lookup",
            message="TikTok extraction cache lookup completed",
            platform="tiktok",
            source_id=cached.source_id,
            cache_hit=True,
            cache_scope="extraction_id",
            media_count=len(cached.media),
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return cached

    async def get_cached_video(self, video_id: int | str) -> TikTokExtractionResponse | None:
        started_at = perf_counter()
        cached = await self.cache.get_model(
            self.cache.metadata_key("tiktok", str(video_id)), TikTokExtractionResponse
        )
        log_event(
            logger,
            "tiktok.video_cache.lookup",
            message="TikTok video cache lookup completed",
            platform="tiktok",
            source_id=str(video_id),
            cache_hit=cached is not None,
            cache_scope="video_id",
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return cached

    async def _build_video_response(
        self,
        data: dict[str, Any],
        context: YtdlpContext,
        video_id: str,
        source_url: str,
        resolved_url: str,
    ) -> TikTokExtractionResponse:
        expires_at = self._expires_at()
        extraction_id = str(uuid4())
        stats = data.get("stats", {})
        music_data = data.get("music") or {}
        music = await self._music_metadata(music_data, context, video_id, expires_at, extraction_id)
        image_post = data.get("imagePost")
        media: list[AssetDescriptor] = []
        cover: AssetDescriptor | None = None
        width: int | None = None
        height: int | None = None
        duration: int | None = None

        if image_post:
            for position, image in enumerate(image_post.get("images", [])):
                url_list = [
                    item
                    for item in image.get("imageURL", {}).get("urlList", [])
                    if isinstance(item, str) and item
                ]
                url = _first(url_list)
                if url:
                    media.append(
                        await self._asset(
                            url=url,
                            alternate_urls=url_list[1:],
                            context=context,
                            kind="image",
                            filename=f"{video_id}_{position + 1}.jpg",
                            position=position,
                            expires_at=expires_at,
                            extraction_id=extraction_id,
                        )
                    )
            if not media:
                raise ExtractionError("TikTok slideshow has no image assets")
            content_type: Literal["video", "slideshow"] = "slideshow"
        else:
            video = data.get("video") or {}
            selection_started_at = perf_counter()
            video_source = select_video_source(video)
            if not video_source:
                raise ExtractionError("TikTok response has no video asset")
            duration = int(video["duration"]) if video.get("duration") else None
            if (
                self.settings.max_video_duration
                and duration
                and duration > self.settings.max_video_duration
            ):
                raise ContentTooLongError("TikTok video exceeds MAX_VIDEO_DURATION")
            width = video_source.width
            height = video_source.height
            log_event(
                logger,
                "tiktok.video_selection.completed",
                message="TikTok video quality selection completed",
                platform="tiktok",
                source_id=video_id,
                width=width,
                height=height,
                uses_separate_audio=video_source.audio_url is not None,
                elapsed_ms=elapsed_ms(selection_started_at),
                success=True,
            )
            media.append(
                await self._asset(
                    url=video_source.url,
                    alternate_urls=video_source.alternate_urls,
                    context=context,
                    kind="video",
                    filename=f"{video_id}.mp4",
                    position=0,
                    expires_at=expires_at,
                    extraction_id=extraction_id,
                    declared_content_type="video/mp4",
                    duration=duration,
                    audio_url=video_source.audio_url,
                    alternate_audio_urls=video_source.alternate_audio_urls,
                )
            )
            cover_url = _first(video.get("cover")) or _first(video.get("originCover"))
            if cover_url:
                cover = await self._asset(
                    url=cover_url,
                    context=context,
                    kind="cover",
                    filename=f"{video_id}_cover.jpg",
                    position=0,
                    expires_at=expires_at,
                    extraction_id=extraction_id,
                )
            content_type = "video"

        return TikTokExtractionResponse(
            extraction_id=extraction_id,
            source_id=video_id,
            source_url=source_url,
            resolved_url=resolved_url,
            content_type=content_type,
            cover=cover,
            width=width,
            height=height,
            duration_seconds=duration,
            likes=stats.get("diggCount"),
            views=stats.get("playCount"),
            media=media,
            music=music,
            expires_at=expires_at,
        )

    async def _music_metadata(
        self,
        music: dict[str, Any],
        context: YtdlpContext,
        video_id: str,
        expires_at: datetime,
        extraction_id: str,
    ) -> TikTokMusicMetadata | None:
        if not music:
            return None
        audio_url = _first(music.get("playUrl"))
        audio = None
        if audio_url:
            audio = await self._asset(
                url=audio_url,
                context=context,
                kind="audio",
                filename=f"{video_id}.mp3",
                position=0,
                expires_at=expires_at,
                extraction_id=extraction_id,
                declared_content_type="audio/mpeg",
            )
        cover_url = _first(music.get("coverLarge")) or _first(music.get("coverMedium"))
        cover_url = cover_url or _first(music.get("coverThumb"))
        cover = None
        if cover_url:
            cover = await self._asset(
                url=cover_url,
                context=context,
                kind="cover",
                filename=f"{video_id}_music_cover.jpg",
                position=0,
                expires_at=expires_at,
                extraction_id=extraction_id,
            )
        return TikTokMusicMetadata(
            title=str(music.get("title", "")),
            author=str(music.get("authorName", "")),
            duration_seconds=int(music.get("duration", 0)),
            cover=cover,
            audio=audio,
        )

    async def extract_music(self, video_id: int, *, refresh: bool = False) -> TikTokMusicResponse:
        started_at = perf_counter()
        identity = str(video_id)
        cache_key = self.cache.metadata_key("tiktok-music", identity)
        if not refresh:
            cached = await self.cache.get_model(cache_key, TikTokMusicResponse)
            if cached:
                log_event(
                    logger,
                    "tiktok.music_extraction.completed",
                    message="TikTok music extraction served from cache",
                    platform="tiktok",
                    source_id=identity,
                    cache_hit=True,
                    cache_scope="video_id",
                    elapsed_ms=elapsed_ms(started_at),
                    success=True,
                )
                return cached
        async with self._key_lock(cache_key):
            if not refresh:
                cached = await self.cache.get_model(cache_key, TikTokMusicResponse)
                if cached:
                    log_event(
                        logger,
                        "tiktok.music_extraction.completed",
                        message="TikTok music extraction served from coalesced cache",
                        platform="tiktok",
                        source_id=identity,
                        cache_hit=True,
                        cache_scope="video_id_coalesced",
                        elapsed_ms=elapsed_ms(started_at),
                        success=True,
                    )
                    return cached
            proxy_session = ProxySession(self.proxy_manager)
            url = f"https://www.tiktok.com/@_/video/{video_id}"
            data, context = await self.adapter.extract(url, identity, proxy_session)
            try:
                music = data.get("music") or {}
                audio_url = _first(music.get("playUrl"))
                if not audio_url:
                    raise ExtractionError("TikTok response has no music asset")
                expires_at = self._expires_at()
                extraction_id = str(uuid4())
                audio = await self._asset(
                    url=audio_url,
                    context=context,
                    kind="audio",
                    filename=f"{video_id}.mp3",
                    position=0,
                    expires_at=expires_at,
                    extraction_id=extraction_id,
                    declared_content_type="audio/mpeg",
                )
                cover_url = _first(music.get("coverLarge")) or _first(music.get("coverMedium"))
                cover_url = cover_url or _first(music.get("coverThumb"))
                cover = None
                if cover_url:
                    cover = await self._asset(
                        url=cover_url,
                        context=context,
                        kind="cover",
                        filename=f"{video_id}_music_cover.jpg",
                        position=0,
                        expires_at=expires_at,
                        extraction_id=extraction_id,
                    )
                response = TikTokMusicResponse(
                    extraction_id=extraction_id,
                    source_id=identity,
                    title=str(music.get("title", "")),
                    author=str(music.get("authorName", "")),
                    duration_seconds=int(music.get("duration", 0)),
                    cover=cover,
                    audio=audio,
                    expires_at=expires_at,
                )
            finally:
                context.close()
            await self.cache.set_model(
                cache_key,
                response,
                ttl_seconds=self.settings.tiktok_info_cache_ttl_seconds,
            )
            log_event(
                logger,
                "tiktok.music_extraction.completed",
                message="TikTok music extraction completed",
                platform="tiktok",
                source_id=identity,
                cache_hit=False,
                elapsed_ms=elapsed_ms(started_at),
                success=True,
            )
            return response

    async def close(self) -> None:
        await self.adapter.close()
