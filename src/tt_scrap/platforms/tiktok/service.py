"""TikTok response normalization and cache/session creation."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any, Literal
from uuid import uuid4

from ...assets import AssetFactory
from ...cache import CacheStore
from ...config import Settings
from ...errors import ContentTooLongError, ExtractionError
from ...models import (
    AssetDescriptor,
    AssetFetchContext,
    AuxiliaryAssetFetchContext,
    TikTokExtractionResponse,
    TikTokMusicMetadata,
    TikTokMusicResponse,
)
from ...proxy import ProxyManager, ProxySession
from .adapter import TikTokAdapter, YtdlpContext


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


def select_video_source(video: dict[str, Any]) -> VideoSource | None:
    for raw_bitrate in video.get("bitrateInfo", []):
        if not isinstance(raw_bitrate, dict):
            continue
        address = raw_bitrate.get("PlayAddr") or raw_bitrate.get("play_addr")
        if not isinstance(address, dict):
            continue
        gear_name = str(raw_bitrate.get("GearName") or raw_bitrate.get("gear_name") or "").lower()
        url_key = str(address.get("UrlKey") or address.get("url_key") or "").lower()
        if gear_name != _BEST_VIDEO_GEAR and _BEST_VIDEO_URL_TAG not in url_key:
            continue
        urls = _address_urls(address)
        if not urls:
            break
        audio = _best_audio_source(video)
        has_separate_audio = bool(video.get("bitrateAudioInfo") or video.get("bit_rate_audio_info"))
        if has_separate_audio and not audio:
            break
        return VideoSource(
            url=urls[0],
            alternate_urls=urls[1:],
            width=_positive_int(address.get("Width") or address.get("width")),
            height=_positive_int(address.get("Height") or address.get("height")),
            audio_url=audio[0] if audio else None,
            alternate_audio_urls=audio[1] if audio else None,
        )
    return _regular_video_source(video)


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

    def _expires_at(self) -> datetime:
        return datetime.now(UTC) + timedelta(seconds=self.settings.cache_ttl_seconds)

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
        proxy_session = ProxySession(self.proxy_manager)
        resolved_url = await self.adapter.resolve_url(source_url, proxy_session)
        video_id = self.adapter.extract_id(resolved_url)
        cache_key = self.cache.metadata_key("tiktok", video_id)
        if not refresh:
            cached = await self.cache.get_model(cache_key, TikTokExtractionResponse)
            if cached:
                return cached.model_copy(
                    update={"source_url": source_url, "resolved_url": resolved_url}
                )

        extraction_url = f"https://www.tiktok.com/@_/video/{video_id}"
        data, context = await self.adapter.extract(extraction_url, video_id, proxy_session)
        try:
            response = await self._build_video_response(
                data, context, video_id, source_url, resolved_url
            )
        finally:
            context.close()
        await self.cache.set_model(cache_key, response)
        return response

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
        )

    async def extract_music(self, video_id: int) -> TikTokMusicResponse:
        identity = str(video_id)
        cache_key = self.cache.metadata_key("tiktok-music", identity)
        cached = await self.cache.get_model(cache_key, TikTokMusicResponse)
        if cached:
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
        await self.cache.set_model(cache_key, response)
        return response

    async def close(self) -> None:
        await self.adapter.close()
