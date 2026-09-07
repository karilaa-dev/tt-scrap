"""TikTok and Instagram extraction-to-Telegram delivery orchestration."""

from __future__ import annotations

import asyncio
import io
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from time import perf_counter
from typing import Any, BinaryIO, Literal

from ..cache import CacheStore
from ..config import Settings
from ..errors import (
    ConfigurationError,
    ExtractionError,
    ImageConversionError,
    TelegramParameterError,
)
from ..logging import elapsed_ms, log_event
from ..media import AssetDownloader, DownloadedAsset, ImagePreparationService, StreamedAsset
from ..media.downloader import filename_for_type
from ..media.images import detect_image_format, is_native_telegram_photo
from ..models import (
    AssetDescriptor,
    InstagramExtractionResponse,
    InstagramMediaItem,
    InstagramTelegramDeliveryRequest,
    TelegramParameters,
    TikTokExtractionResponse,
    TikTokMusicMetadata,
    TikTokMusicResponse,
    TikTokTelegramDeliveryRequest,
)
from ..platforms.instagram import InstagramService
from ..platforms.tiktok import TikTokService
from .client import TelegramCallResponse, TelegramClient, TelegramUpload

logger = logging.getLogger(__name__)

_COMMON_SINGLE_FIELDS = {
    "allow_paid_broadcast",
    "business_connection_id",
    "callback_query_id",
    "caption",
    "caption_entities",
    "chat_id",
    "direct_messages_topic_id",
    "disable_notification",
    "message_effect_id",
    "message_thread_id",
    "parse_mode",
    "protect_content",
    "receiver_user_id",
    "reply_markup",
    "reply_parameters",
    "suggested_post_parameters",
}
_VIDEO_FIELDS = _COMMON_SINGLE_FIELDS | {
    "duration",
    "has_spoiler",
    "height",
    "show_caption_above_media",
    "start_timestamp",
    "supports_streaming",
    "width",
}
_DOCUMENT_FIELDS = _COMMON_SINGLE_FIELDS | {"disable_content_type_detection"}
_AUDIO_FIELDS = _COMMON_SINGLE_FIELDS | {"duration", "performer", "title"}
_PHOTO_FIELDS = _COMMON_SINGLE_FIELDS | {"has_spoiler", "show_caption_above_media"}
_MEDIA_GROUP_FIELDS = {
    "allow_paid_broadcast",
    "business_connection_id",
    "chat_id",
    "direct_messages_topic_id",
    "disable_notification",
    "message_effect_id",
    "message_thread_id",
    "protect_content",
    "reply_parameters",
}
_ALBUM_CAPTION_FIELDS = {"caption", "caption_entities", "parse_mode"}
_ALBUM_MEDIA_FIRST_FIELDS = {"show_caption_above_media"}
_ALBUM_MEDIA_ITEM_FIELDS = {"has_spoiler"}
_ALBUM_VIDEO_FIELDS = {"start_timestamp", "supports_streaming"}
_KNOWN_TELEGRAM_FIELDS = set(TelegramParameters.model_fields)


@dataclass(frozen=True, slots=True)
class TelegramDeliveryOutcome:
    calls: list[TelegramCallResponse]


@dataclass(slots=True)
class _PreparedUpload:
    file: BinaryIO
    filename: str
    content_type: str


@dataclass(slots=True)
class _PreparedInstagramItem:
    media_type: Literal["video", "image"]
    media: _PreparedUpload
    thumbnail: _PreparedUpload | None = None


def _close_files(files: list[BinaryIO]) -> None:
    for file in files:
        try:
            file.close()
        except OSError:
            pass


def _album_batches[AlbumItemT](items: list[AlbumItemT]) -> list[list[AlbumItemT]]:
    if len(items) <= 10:
        return [items]
    batches: list[list[AlbumItemT]] = []
    offset = 0
    while len(items) - offset > 10:
        remaining = len(items) - offset
        size = 9 if remaining == 11 else 10
        batches.append(items[offset : offset + size])
        offset += size
    batches.append(items[offset:])
    return batches


class TelegramDeliveryService:
    def __init__(
        self,
        settings: Settings,
        cache: CacheStore,
        tiktok: TikTokService,
        downloader: AssetDownloader,
        images: ImagePreparationService,
        client: TelegramClient,
        instagram: InstagramService | None = None,
    ) -> None:
        self._cache = cache
        self._tiktok = tiktok
        self._downloader = downloader
        self._images = images
        self._client = client
        self._instagram = instagram
        self._pipeline_limit = asyncio.Semaphore(settings.telegram_pipeline_concurrency)
        self._upload_limit = asyncio.Semaphore(settings.telegram_upload_concurrency)
        self._thumbnail_wait_seconds = settings.telegram_thumbnail_wait_seconds
        self._upload_max_bytes = settings.telegram_upload_max_mb * 1024 * 1024

    @asynccontextmanager
    async def _upload_slot(self) -> AsyncIterator[None]:
        async with self._upload_limit:
            yield

    async def _call(
        self,
        method: str,
        fields: dict[str, Any],
        uploads: list[TelegramUpload],
    ) -> TelegramCallResponse:
        async with self._upload_slot():
            return await self._client.call(method, fields, uploads)

    async def deliver(self, request: TikTokTelegramDeliveryRequest) -> TelegramDeliveryOutcome:
        if not self._client.configured:
            raise ConfigurationError("Telegram delivery is not configured")
        started_at = perf_counter()
        source_kind = next(
            name
            for name in ("extraction_id", "url", "video_id")
            if getattr(request.source, name) is not None
        )
        try:
            async with self._pipeline_limit:
                queue_wait = elapsed_ms(started_at)
                if request.delivery == "audio":
                    outcome = await self._deliver_audio(request)
                else:
                    extraction = await self._resolve_extraction(request)
                    if extraction.content_type == "slideshow":
                        outcome = await self._deliver_slideshow(request, extraction)
                    else:
                        outcome = await self._deliver_video(request, extraction)
        except Exception as exc:
            log_event(
                logger,
                "telegram.delivery.failed",
                level=logging.WARNING,
                message="TikTok Telegram delivery failed",
                platform="tiktok",
                delivery=request.delivery,
                source_kind=source_kind,
                elapsed_ms=elapsed_ms(started_at),
                error_type=type(exc).__name__,
                success=False,
            )
            raise
        log_event(
            logger,
            "telegram.delivery.completed",
            message="TikTok Telegram delivery completed",
            platform="tiktok",
            delivery=request.delivery,
            source_kind=source_kind,
            queue_wait_ms=queue_wait,
            call_count=len(outcome.calls),
            status_code=outcome.calls[-1].status_code if outcome.calls else None,
            elapsed_ms=elapsed_ms(started_at),
            success=bool(outcome.calls)
            and all(200 <= call.status_code < 300 for call in outcome.calls),
        )
        return outcome

    async def deliver_instagram(
        self, request: InstagramTelegramDeliveryRequest
    ) -> TelegramDeliveryOutcome:
        if not self._client.configured:
            raise ConfigurationError("Telegram delivery is not configured")
        if self._instagram is None:
            raise ConfigurationError("Instagram delivery is not configured")
        started_at = perf_counter()
        source_kind = "extraction_id" if request.source.extraction_id is not None else "url"
        try:
            async with self._pipeline_limit:
                queue_wait = elapsed_ms(started_at)
                extraction = await self._resolve_instagram_extraction(request)
                if len(extraction.media) == 1:
                    outcome = await self._deliver_instagram_single(request, extraction.media[0])
                else:
                    outcome = await self._deliver_instagram_carousel(request, extraction)
        except Exception as exc:
            log_event(
                logger,
                "telegram.delivery.failed",
                level=logging.WARNING,
                message="Instagram Telegram delivery failed",
                platform="instagram",
                delivery=request.delivery,
                source_kind=source_kind,
                elapsed_ms=elapsed_ms(started_at),
                error_type=type(exc).__name__,
                success=False,
            )
            raise
        log_event(
            logger,
            "telegram.delivery.completed",
            message="Instagram Telegram delivery completed",
            platform="instagram",
            delivery=request.delivery,
            source_kind=source_kind,
            queue_wait_ms=queue_wait,
            call_count=len(outcome.calls),
            status_code=outcome.calls[-1].status_code if outcome.calls else None,
            elapsed_ms=elapsed_ms(started_at),
            success=bool(outcome.calls)
            and all(200 <= call.status_code < 300 for call in outcome.calls),
        )
        return outcome

    async def _resolve_extraction(
        self, request: TikTokTelegramDeliveryRequest
    ) -> TikTokExtractionResponse:
        source = request.source
        if source.extraction_id is not None:
            return await self._tiktok.get_extraction(source.extraction_id)
        if source.url is None:
            raise ExtractionError("A TikTok URL or extraction_id is required")
        return await self._tiktok.extract_url(str(source.url), refresh=request.refresh)

    async def _resolve_instagram_extraction(
        self, request: InstagramTelegramDeliveryRequest
    ) -> InstagramExtractionResponse:
        instagram = self._instagram
        if instagram is None:
            raise ConfigurationError("Instagram delivery is not configured")
        source = request.source
        if source.extraction_id is not None:
            return await instagram.get_extraction(source.extraction_id)
        if source.url is None:
            raise ExtractionError("An Instagram URL or extraction_id is required")
        return await instagram.extract_url(str(source.url), refresh=request.refresh)

    async def _download(self, descriptor: AssetDescriptor) -> DownloadedAsset:
        context = await self._cache.get_asset(descriptor.asset_id)
        return await self._downloader.download(
            context,
            compute_sha256=False,
            # Photos can shrink during conversion; their final upload check
            # remains authoritative. Videos/audio are copied or remuxed.
            max_bytes=self._upload_max_bytes if context.kind in {"video", "audio"} else 0,
        )

    @asynccontextmanager
    async def _stream(self, descriptor: AssetDescriptor) -> AsyncIterator[StreamedAsset | None]:
        context = await self._cache.get_asset(descriptor.asset_id)
        async with self._downloader.stream(context, max_bytes=self._upload_max_bytes) as streamed:
            yield streamed

    async def _download_with_cover(
        self, media: AssetDescriptor, cover: AssetDescriptor | None
    ) -> tuple[DownloadedAsset, DownloadedAsset | None]:
        async def optional_cover() -> DownloadedAsset | None:
            if cover is None or self._thumbnail_wait_seconds == 0:
                return None
            try:
                async with asyncio.timeout(self._thumbnail_wait_seconds):
                    return await self._download(cover)
            except Exception as exc:
                log_event(
                    logger,
                    "telegram.thumbnail_download.failed",
                    level=logging.WARNING,
                    message="Media cover skipped; Telegram will generate a preview",
                    error_type=type(exc).__name__,
                    success=False,
                )
                return None

        media_task = asyncio.create_task(self._download(media))
        cover_task = asyncio.create_task(optional_cover())
        try:
            media_result = await media_task
            return media_result, await cover_task
        except BaseException:
            media_task.cancel()
            cover_task.cancel()
            results = await asyncio.gather(media_task, cover_task, return_exceptions=True)
            for result in results:
                if isinstance(result, DownloadedAsset):
                    result.file.close()
            raise

    async def _download_prepared_thumbnail(
        self,
        cover: AssetDescriptor | None,
        thumbnail_filename: str,
    ) -> tuple[DownloadedAsset | None, tuple[io.BytesIO, str] | None]:
        if cover is None:
            return None, None
        try:
            downloaded_cover = await self._download(cover)
        except Exception as exc:
            log_event(
                logger,
                "telegram.thumbnail_download.failed",
                level=logging.WARNING,
                message="Media cover download failed; Telegram will generate a preview",
                error_type=type(exc).__name__,
                success=False,
            )
            return None, None
        try:
            thumbnail = await self._thumbnail(
                downloaded_cover,
                thumbnail_filename,
            )
        except BaseException:
            downloaded_cover.file.close()
            raise
        return downloaded_cover, thumbnail

    async def _bounded_thumbnail(
        self,
        cover: AssetDescriptor | None,
        filename: str,
    ) -> tuple[DownloadedAsset | None, tuple[io.BytesIO, str] | None]:
        if cover is None or self._thumbnail_wait_seconds == 0:
            return None, None
        try:
            async with asyncio.timeout(self._thumbnail_wait_seconds):
                return await self._download_prepared_thumbnail(cover, filename)
        except TimeoutError:
            log_event(
                logger,
                "telegram.relay_thumbnail.skipped",
                message="Slow source cover skipped; Telegram will generate a preview",
                wait_seconds=self._thumbnail_wait_seconds,
                success=True,
            )
            return None, None

    @staticmethod
    def _close_thumbnail(
        result: tuple[DownloadedAsset | None, tuple[io.BytesIO, str] | None],
    ) -> None:
        cover, thumbnail = result
        if cover is not None:
            cover.file.close()
        if thumbnail is not None:
            thumbnail[0].close()

    @asynccontextmanager
    async def _background_thumbnail(
        self,
        cover: AssetDescriptor | None,
        filename: str,
    ) -> AsyncIterator[asyncio.Task[tuple[DownloadedAsset | None, tuple[io.BytesIO, str] | None]]]:
        task = asyncio.create_task(self._bounded_thumbnail(cover, filename))
        try:
            yield task
        finally:
            if not task.done():
                task.cancel()
            result = (await asyncio.gather(task, return_exceptions=True))[0]
            if isinstance(result, tuple):
                self._close_thumbnail(result)

    async def _download_with_prepared_thumbnail(
        self,
        media: AssetDescriptor,
        cover: AssetDescriptor | None,
        thumbnail_filename: str,
    ) -> tuple[DownloadedAsset, DownloadedAsset | None, tuple[io.BytesIO, str] | None]:
        """Overlap preparation, bound optional work, and stop it if the media fails."""
        media_task = asyncio.create_task(self._download(media))
        cover_task = asyncio.create_task(self._bounded_thumbnail(cover, thumbnail_filename))
        try:
            media_result = await media_task
            cover_result, thumbnail = await cover_task
            return media_result, cover_result, thumbnail
        except BaseException:
            media_task.cancel()
            cover_task.cancel()
            results = await asyncio.gather(media_task, cover_task, return_exceptions=True)
            if isinstance(results[0], DownloadedAsset):
                results[0].file.close()
            if isinstance(results[1], tuple):
                self._close_thumbnail(results[1])
            raise

    async def _thumbnail(
        self, cover: DownloadedAsset | None, filename: str
    ) -> tuple[io.BytesIO, str] | None:
        if cover is None:
            return None
        try:
            data = await self._images.read_file(cover.file)
            converted = await self._images.prepare_thumbnail(data, filename)
        except Exception as exc:
            log_event(
                logger,
                "telegram.thumbnail_preparation.failed",
                level=logging.WARNING,
                message="Media cover conversion failed; Telegram will generate a preview",
                error_type=type(exc).__name__,
                success=False,
            )
            return None
        return io.BytesIO(converted.data), converted.filename

    def _fields(self, parameters: TelegramParameters, allowed: set[str]) -> dict[str, Any]:
        fields = parameters.model_dump(exclude_none=True)
        known_invalid = (set(fields) & _KNOWN_TELEGRAM_FIELDS) - allowed
        if known_invalid:
            names = ", ".join(sorted(known_invalid))
            raise TelegramParameterError(f"Parameters are invalid for this delivery: {names}")
        return fields

    @staticmethod
    def _default(
        fields: dict[str, Any], parameters: TelegramParameters, name: str, value: Any
    ) -> None:
        if name not in parameters.model_fields_set and value is not None:
            fields[name] = value

    async def _try_relay_video(
        self,
        extraction: TikTokExtractionResponse,
        fields: dict[str, Any],
    ) -> TelegramDeliveryOutcome | None:
        # Cover preparation overlaps the CDN connection/TLS/response headers.
        async with (
            self._background_thumbnail(
                extraction.cover, f"{extraction.source_id}_thumbnail.jpg"
            ) as thumbnail_task,
            self._stream(extraction.media[0]) as streamed,
        ):
            if streamed is None:
                return None
            _cover, thumbnail = await thumbnail_task
            filename = filename_for_type(extraction.media[0].filename, streamed.content_type)
            fields["video"] = "attach://video_file"
            uploads = [
                TelegramUpload(
                    "video_file",
                    streamed.chunks,
                    filename,
                    streamed.content_type,
                    size=streamed.size,
                )
            ]
            if thumbnail is not None:
                thumbnail_file, thumbnail_name = thumbnail
                fields["thumbnail"] = "attach://thumbnail_file"
                fields["cover"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload(
                        "thumbnail_file",
                        thumbnail_file,
                        thumbnail_name,
                        "image/jpeg",
                    )
                )
            try:
                response = await self._call("sendVideo", fields, uploads)
            except Exception:
                if streamed.failure is None:
                    raise
                log_event(
                    logger,
                    "telegram.relay.fallback",
                    level=logging.WARNING,
                    message="Interrupted media relay is retrying through verified spool",
                    delivery="media",
                    error_type=type(streamed.failure).__name__,
                    success=False,
                )
                return None
            return TelegramDeliveryOutcome([response])

    async def _deliver_video(
        self, request: TikTokTelegramDeliveryRequest, extraction: TikTokExtractionResponse
    ) -> TelegramDeliveryOutcome:
        if request.delivery == "document":
            fields = self._fields(request.telegram, _DOCUMENT_FIELDS)
            self._default(
                fields,
                request.telegram,
                "disable_content_type_detection",
                True,
            )
            async with self._stream(extraction.media[0]) as streamed:
                if streamed is not None:
                    filename = filename_for_type(
                        extraction.media[0].filename,
                        streamed.content_type,
                    )
                    fields["document"] = "attach://document_file"
                    try:
                        response = await self._call(
                            "sendDocument",
                            fields,
                            [
                                TelegramUpload(
                                    "document_file",
                                    streamed.chunks,
                                    filename,
                                    streamed.content_type,
                                    size=streamed.size,
                                )
                            ],
                        )
                    except Exception:
                        if streamed.failure is None:
                            raise
                        log_event(
                            logger,
                            "telegram.relay.fallback",
                            level=logging.WARNING,
                            message="Interrupted media relay is retrying through verified spool",
                            delivery="document",
                            error_type=type(streamed.failure).__name__,
                            success=False,
                        )
                    else:
                        return TelegramDeliveryOutcome([response])
            video = await self._download(extraction.media[0])
            try:
                filename = filename_for_type(extraction.media[0].filename, video.content_type)
                fields["document"] = "attach://document_file"
                response = await self._call(
                    "sendDocument",
                    fields,
                    [TelegramUpload("document_file", video.file, filename, video.content_type)],
                )
                return TelegramDeliveryOutcome([response])
            finally:
                video.file.close()

        fields = self._fields(request.telegram, _VIDEO_FIELDS)
        self._default(fields, request.telegram, "duration", extraction.duration_seconds)
        self._default(fields, request.telegram, "width", extraction.width)
        self._default(fields, request.telegram, "height", extraction.height)
        self._default(fields, request.telegram, "supports_streaming", True)
        relayed = await self._try_relay_video(extraction, fields)
        if relayed is not None:
            return relayed

        fields.pop("thumbnail", None)
        fields.pop("cover", None)

        video, cover, thumbnail = await self._download_with_prepared_thumbnail(
            extraction.media[0],
            extraction.cover,
            f"{extraction.source_id}_thumbnail.jpg",
        )
        extra_files: list[BinaryIO] = []
        try:
            filename = filename_for_type(
                extraction.media[0].filename,
                video.content_type,
            )
            fields["video"] = "attach://video_file"
            uploads = [
                TelegramUpload(
                    "video_file",
                    video.file,
                    filename,
                    video.content_type,
                )
            ]
            if thumbnail is not None:
                thumbnail_file, thumbnail_name = thumbnail
                extra_files.append(thumbnail_file)
                fields["thumbnail"] = "attach://thumbnail_file"
                fields["cover"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload("thumbnail_file", thumbnail_file, thumbnail_name, "image/jpeg")
                )
            response = await self._call("sendVideo", fields, uploads)
            return TelegramDeliveryOutcome([response])
        finally:
            video.file.close()
            if cover is not None:
                cover.file.close()
            _close_files(extra_files)

    async def _music_for_request(
        self, request: TikTokTelegramDeliveryRequest
    ) -> TikTokMusicMetadata | TikTokMusicResponse:
        source = request.source
        extraction: TikTokExtractionResponse | None = None
        if source.extraction_id is not None:
            extraction = await self._tiktok.get_extraction(source.extraction_id)
        elif source.url is not None:
            extraction = await self._tiktok.extract_url(str(source.url), refresh=request.refresh)
        elif source.video_id is not None and not request.refresh:
            extraction = await self._tiktok.get_cached_video(source.video_id)
        if extraction is not None and extraction.music is not None:
            if extraction.music.audio is not None:
                return extraction.music
        video_id = source.video_id
        if video_id is None and extraction is not None:
            video_id = int(extraction.source_id)
        if video_id is None:
            raise ExtractionError("TikTok post has no reusable music information")
        return await self._tiktok.extract_music(video_id, refresh=request.refresh)

    async def _deliver_audio(
        self, request: TikTokTelegramDeliveryRequest
    ) -> TelegramDeliveryOutcome:
        music = await self._music_for_request(request)
        audio_descriptor = music.audio
        if audio_descriptor is None:
            raise ExtractionError("TikTok response has no music asset")
        fields = self._fields(request.telegram, _AUDIO_FIELDS)
        self._default(fields, request.telegram, "duration", music.duration_seconds)
        self._default(fields, request.telegram, "title", music.title)
        self._default(fields, request.telegram, "performer", music.author)
        audio, cover, thumbnail = await self._download_with_prepared_thumbnail(
            audio_descriptor,
            music.cover,
            f"{audio_descriptor.asset_id}_thumbnail.jpg",
        )
        extra_files: list[BinaryIO] = []
        try:
            fields["audio"] = "attach://audio_file"
            filename = filename_for_type(audio_descriptor.filename, audio.content_type)
            uploads = [TelegramUpload("audio_file", audio.file, filename, audio.content_type)]
            if thumbnail is not None:
                thumbnail_file, thumbnail_name = thumbnail
                extra_files.append(thumbnail_file)
                fields["thumbnail"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload("thumbnail_file", thumbnail_file, thumbnail_name, "image/jpeg")
                )
            response = await self._call("sendAudio", fields, uploads)
            return TelegramDeliveryOutcome([response])
        finally:
            audio.file.close()
            if cover is not None:
                cover.file.close()
            _close_files(extra_files)

    async def _prepare_instagram_item(
        self,
        item: InstagramMediaItem,
        downloaded: DownloadedAsset,
        thumbnail: DownloadedAsset | None,
        *,
        document: bool,
        converted_thumbnail: tuple[io.BytesIO, str] | None = None,
        thumbnail_prepared: bool = False,
    ) -> tuple[_PreparedInstagramItem, list[BinaryIO]]:
        filename = filename_for_type(item.asset.filename, downloaded.content_type)
        if document:
            return (
                _PreparedInstagramItem(
                    media_type=item.media_type,
                    media=_PreparedUpload(downloaded.file, filename, downloaded.content_type),
                ),
                [],
            )
        if item.media_type == "image":
            upload, converted_file = await self._prepare_slideshow_item(
                item.asset,
                downloaded,
                document=False,
            )
            return (
                _PreparedInstagramItem(media_type="image", media=upload),
                [converted_file] if converted_file is not None else [],
            )

        extra_files: list[BinaryIO] = []
        prepared_thumbnail = None
        converted = converted_thumbnail
        if not thumbnail_prepared:
            converted = await self._thumbnail(
                thumbnail,
                f"{item.asset.asset_id}_thumbnail.jpg",
            )
        if converted is not None:
            thumbnail_file, thumbnail_name = converted
            extra_files.append(thumbnail_file)
            prepared_thumbnail = _PreparedUpload(
                thumbnail_file,
                thumbnail_name,
                "image/jpeg",
            )
        return (
            _PreparedInstagramItem(
                media_type="video",
                media=_PreparedUpload(downloaded.file, filename, downloaded.content_type),
                thumbnail=prepared_thumbnail,
            ),
            extra_files,
        )

    async def _deliver_instagram_single(
        self,
        request: InstagramTelegramDeliveryRequest,
        item: InstagramMediaItem,
    ) -> TelegramDeliveryOutcome:
        if request.delivery == "document":
            fields = self._fields(request.telegram, _DOCUMENT_FIELDS)
            self._default(
                fields,
                request.telegram,
                "disable_content_type_detection",
                True,
            )
        elif item.media_type == "image":
            fields = self._fields(request.telegram, _PHOTO_FIELDS)
        else:
            fields = self._fields(request.telegram, _VIDEO_FIELDS)
            self._default(fields, request.telegram, "supports_streaming", True)
        use_thumbnail = request.delivery == "media" and item.media_type == "video"
        if use_thumbnail:
            (
                downloaded,
                thumbnail,
                converted_thumbnail,
            ) = await self._download_with_prepared_thumbnail(
                item.asset,
                item.thumbnail,
                f"{item.asset.asset_id}_thumbnail.jpg",
            )
        else:
            downloaded, thumbnail = await self._download_with_cover(item.asset, None)
            converted_thumbnail = None
        extra_files: list[BinaryIO] = []
        try:
            prepared, owned_files = await self._prepare_instagram_item(
                item,
                downloaded,
                thumbnail,
                document=request.delivery == "document",
                converted_thumbnail=converted_thumbnail,
                thumbnail_prepared=use_thumbnail,
            )
            extra_files.extend(owned_files)
            if request.delivery == "document":
                fields["document"] = "attach://document_file"
                response = await self._call(
                    "sendDocument",
                    fields,
                    [
                        TelegramUpload(
                            "document_file",
                            prepared.media.file,
                            prepared.media.filename,
                            prepared.media.content_type,
                        )
                    ],
                )
                return TelegramDeliveryOutcome([response])

            if item.media_type == "image":
                fields["photo"] = "attach://photo_file"
                response = await self._call(
                    "sendPhoto",
                    fields,
                    [
                        TelegramUpload(
                            "photo_file",
                            prepared.media.file,
                            prepared.media.filename,
                            prepared.media.content_type,
                        )
                    ],
                )
                return TelegramDeliveryOutcome([response])

            fields["video"] = "attach://video_file"
            uploads = [
                TelegramUpload(
                    "video_file",
                    prepared.media.file,
                    prepared.media.filename,
                    prepared.media.content_type,
                )
            ]
            if prepared.thumbnail is not None:
                fields["thumbnail"] = "attach://thumbnail_file"
                fields["cover"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload(
                        "thumbnail_file",
                        prepared.thumbnail.file,
                        prepared.thumbnail.filename,
                        prepared.thumbnail.content_type,
                    )
                )
            response = await self._call("sendVideo", fields, uploads)
            return TelegramDeliveryOutcome([response])
        finally:
            downloaded.file.close()
            if thumbnail is not None:
                thumbnail.file.close()
            _close_files(extra_files)
            if converted_thumbnail is not None:
                converted_thumbnail[0].close()

    async def _deliver_instagram_carousel(
        self,
        request: InstagramTelegramDeliveryRequest,
        extraction: InstagramExtractionResponse,
    ) -> TelegramDeliveryOutcome:
        document = request.delivery == "document"

        item_fields = set(_ALBUM_CAPTION_FIELDS)
        if document:
            item_fields.add("disable_content_type_detection")
        else:
            item_fields |= (
                _ALBUM_MEDIA_FIRST_FIELDS | _ALBUM_MEDIA_ITEM_FIELDS | _ALBUM_VIDEO_FIELDS
            )
        fields = self._fields(request.telegram, _MEDIA_GROUP_FIELDS | item_fields)
        first_item_fields = {
            name: fields.pop(name)
            for name in _ALBUM_CAPTION_FIELDS | _ALBUM_MEDIA_FIRST_FIELDS
            if name in fields
        }
        all_media_fields = {
            name: fields.pop(name) for name in _ALBUM_MEDIA_ITEM_FIELDS if name in fields
        }
        video_fields = {name: fields.pop(name) for name in _ALBUM_VIDEO_FIELDS if name in fields}
        disable_content_type_detection = fields.pop(
            "disable_content_type_detection",
            True,
        )

        async def download_item(
            item: InstagramMediaItem,
        ) -> tuple[
            DownloadedAsset,
            DownloadedAsset | None,
            tuple[io.BytesIO, str] | None,
        ]:
            cover = item.thumbnail if not document and item.media_type == "video" else None
            if cover is not None:
                return await self._download_with_prepared_thumbnail(
                    item.asset,
                    cover,
                    f"{item.asset.asset_id}_thumbnail.jpg",
                )
            downloaded, downloaded_cover = await self._download_with_cover(item.asset, None)
            return downloaded, downloaded_cover, None

        download_started_at = perf_counter()
        download_outcomes = await asyncio.gather(
            *[download_item(item) for item in extraction.media],
            return_exceptions=True,
        )
        downloaded_items: list[
            tuple[
                DownloadedAsset,
                DownloadedAsset | None,
                tuple[io.BytesIO, str] | None,
            ]
        ] = []
        download_failure: BaseException | None = None
        for download_outcome in download_outcomes:
            if isinstance(download_outcome, BaseException):
                download_failure = download_failure or download_outcome
            else:
                downloaded_items.append(download_outcome)
        if download_failure is not None:
            for media, thumbnail, converted_thumbnail in downloaded_items:
                media.file.close()
                if thumbnail is not None:
                    thumbnail.file.close()
                if converted_thumbnail is not None:
                    converted_thumbnail[0].close()
            log_event(
                logger,
                "telegram.album_downloads.failed",
                level=logging.WARNING,
                message="Instagram carousel downloads failed",
                platform="instagram",
                delivery=request.delivery,
                item_count=len(extraction.media),
                elapsed_ms=elapsed_ms(download_started_at),
                error_type=type(download_failure).__name__,
                success=False,
            )
            raise download_failure
        log_event(
            logger,
            "telegram.album_downloads.completed",
            message="Instagram carousel downloads completed",
            platform="instagram",
            delivery=request.delivery,
            item_count=len(downloaded_items),
            output_bytes=sum(
                media.size + (thumbnail.size if thumbnail is not None else 0)
                for media, thumbnail, _converted_thumbnail in downloaded_items
            ),
            elapsed_ms=elapsed_ms(download_started_at),
            success=True,
        )

        extra_files: list[BinaryIO] = []
        try:
            preparation_started_at = perf_counter()
            preparation_outcomes = await asyncio.gather(
                *[
                    self._prepare_instagram_item(
                        item,
                        downloaded[0],
                        downloaded[1],
                        document=document,
                        converted_thumbnail=downloaded[2],
                        thumbnail_prepared=True,
                    )
                    for item, downloaded in zip(
                        extraction.media,
                        downloaded_items,
                        strict=True,
                    )
                ],
                return_exceptions=True,
            )
            prepared: list[_PreparedInstagramItem] = []
            preparation_failure: BaseException | None = None
            for preparation_outcome in preparation_outcomes:
                if isinstance(preparation_outcome, BaseException):
                    preparation_failure = preparation_failure or preparation_outcome
                else:
                    prepared.append(preparation_outcome[0])
                    extra_files.extend(preparation_outcome[1])
            if preparation_failure is not None:
                log_event(
                    logger,
                    "telegram.album_preparation.failed",
                    level=logging.WARNING,
                    message="Instagram carousel preparation failed",
                    platform="instagram",
                    delivery=request.delivery,
                    item_count=len(extraction.media),
                    elapsed_ms=elapsed_ms(preparation_started_at),
                    error_type=type(preparation_failure).__name__,
                    success=False,
                )
                raise preparation_failure
            log_event(
                logger,
                "telegram.album_preparation.completed",
                message="Instagram carousel preparation completed",
                platform="instagram",
                delivery=request.delivery,
                item_count=len(prepared),
                conversion_count=len(extra_files),
                elapsed_ms=elapsed_ms(preparation_started_at),
                success=True,
            )

            calls: list[TelegramCallResponse] = []
            batches = _album_batches(prepared)
            for batch_index, batch in enumerate(batches):
                batch_fields = dict(fields)
                if batch_index > 0:
                    batch_fields.pop("reply_parameters", None)
                media_payload: list[dict[str, Any]] = []
                uploads: list[TelegramUpload] = []
                for item_index, prepared_item in enumerate(batch):
                    attach_name = f"media_{batch_index}_{item_index}"
                    media_type = (
                        "document"
                        if document
                        else "photo"
                        if prepared_item.media_type == "image"
                        else "video"
                    )
                    media_item: dict[str, Any] = {
                        "type": media_type,
                        "media": f"attach://{attach_name}",
                    }
                    if batch_index == 0 and item_index == 0:
                        media_item.update(first_item_fields)
                    if document:
                        media_item["disable_content_type_detection"] = (
                            disable_content_type_detection
                        )
                    else:
                        media_item.update(all_media_fields)
                        if prepared_item.media_type == "video":
                            media_item.update(video_fields)
                            media_item.setdefault("supports_streaming", True)
                    uploads.append(
                        TelegramUpload(
                            attach_name,
                            prepared_item.media.file,
                            prepared_item.media.filename,
                            prepared_item.media.content_type,
                        )
                    )
                    if not document and prepared_item.thumbnail is not None:
                        thumbnail_name = f"thumbnail_{batch_index}_{item_index}"
                        media_item["thumbnail"] = f"attach://{thumbnail_name}"
                        media_item["cover"] = f"attach://{thumbnail_name}"
                        uploads.append(
                            TelegramUpload(
                                thumbnail_name,
                                prepared_item.thumbnail.file,
                                prepared_item.thumbnail.filename,
                                prepared_item.thumbnail.content_type,
                            )
                        )
                    media_payload.append(media_item)
                batch_fields["media"] = media_payload
                batch_started_at = perf_counter()
                response = await self._call("sendMediaGroup", batch_fields, uploads)
                calls.append(response)
                log_event(
                    logger,
                    "telegram.album_batch.completed",
                    level=logging.INFO if response.ok else logging.WARNING,
                    message="Instagram Telegram album batch completed",
                    platform="instagram",
                    delivery=request.delivery,
                    batch_index=batch_index + 1,
                    batch_count=len(batches),
                    item_count=len(batch),
                    status_code=response.status_code,
                    elapsed_ms=elapsed_ms(batch_started_at),
                    success=response.ok,
                )
                if not response.ok:
                    break
            return TelegramDeliveryOutcome(calls)
        finally:
            for media, thumbnail, converted_thumbnail in downloaded_items:
                media.file.close()
                if thumbnail is not None:
                    thumbnail.file.close()
                if converted_thumbnail is not None:
                    converted_thumbnail[0].close()
            _close_files(extra_files)

    async def _prepare_slideshow_item(
        self, descriptor: AssetDescriptor, downloaded: DownloadedAsset, *, document: bool
    ) -> tuple[_PreparedUpload, BinaryIO | None]:
        filename = filename_for_type(descriptor.filename, downloaded.content_type)
        if document:
            return _PreparedUpload(downloaded.file, filename, downloaded.content_type), None
        downloaded.file.seek(0)
        prefix = downloaded.file.read(32)
        downloaded.file.seek(0)
        detected = detect_image_format(prefix)
        if detected in {"jpeg", "png", "webp"}:
            compliant = is_native_telegram_photo(prefix) and (
                await self._images.native_photo_is_compliant(
                    downloaded.file,
                    downloaded.size,
                    downloaded.content_type,
                    downloaded.declared_content_type,
                )
            )
            if compliant:
                return _PreparedUpload(downloaded.file, filename, downloaded.content_type), None
            data = await self._images.read_file(downloaded.file)
            normalized = await self._images.normalize_photo(data, filename)
            normalized_file = io.BytesIO(normalized.data)
            return (
                _PreparedUpload(
                    normalized_file,
                    normalized.filename,
                    normalized.content_type,
                ),
                normalized_file,
            )
        if detected not in {"heic", "heif", "avif", "tiff", "bmp", "gif"}:
            raise ImageConversionError(f"Unsupported or corrupt Telegram photo format: {detected}")
        data = await self._images.read_file(downloaded.file)
        converted = (
            await self._images.convert_photo(data, filename)
            if detected in {"heic", "heif"}
            else await self._images.normalize_photo(data, filename)
        )
        converted_file = io.BytesIO(converted.data)
        return (
            _PreparedUpload(converted_file, converted.filename, converted.content_type),
            converted_file,
        )

    async def _deliver_slideshow(
        self, request: TikTokTelegramDeliveryRequest, extraction: TikTokExtractionResponse
    ) -> TelegramDeliveryOutcome:
        if len(extraction.media) == 1:
            allowed_fields = _DOCUMENT_FIELDS if request.delivery == "document" else _PHOTO_FIELDS
            fields = self._fields(request.telegram, allowed_fields)
        else:
            fields = self._fields(request.telegram, _MEDIA_GROUP_FIELDS)
        download_started_at = perf_counter()
        tasks = [asyncio.create_task(self._download(item)) for item in extraction.media]
        results = await asyncio.gather(*tasks, return_exceptions=True)
        downloaded = [item for item in results if isinstance(item, DownloadedAsset)]
        failures = [item for item in results if isinstance(item, BaseException)]
        if failures:
            _close_files([item.file for item in downloaded])
            log_event(
                logger,
                "telegram.album_downloads.failed",
                level=logging.WARNING,
                message="TikTok slideshow downloads failed",
                platform="tiktok",
                delivery=request.delivery,
                item_count=len(extraction.media),
                elapsed_ms=elapsed_ms(download_started_at),
                error_type=type(failures[0]).__name__,
                success=False,
            )
            raise failures[0]
        log_event(
            logger,
            "telegram.album_downloads.completed",
            message="TikTok slideshow downloads completed",
            platform="tiktok",
            delivery=request.delivery,
            item_count=len(downloaded),
            output_bytes=sum(item.size for item in downloaded),
            elapsed_ms=elapsed_ms(download_started_at),
            success=True,
        )
        converted_files: list[BinaryIO] = []
        try:
            preparation_started_at = perf_counter()
            prepared_outcomes = await asyncio.gather(
                *[
                    self._prepare_slideshow_item(
                        descriptor,
                        item,
                        document=request.delivery == "document",
                    )
                    for descriptor, item in zip(extraction.media, downloaded, strict=True)
                ],
                return_exceptions=True,
            )
            prepared_results: list[tuple[_PreparedUpload, BinaryIO | None]] = []
            preparation_failure: BaseException | None = None
            for outcome in prepared_outcomes:
                if isinstance(outcome, BaseException):
                    preparation_failure = preparation_failure or outcome
                else:
                    prepared_results.append(outcome)
                    if outcome[1] is not None:
                        converted_files.append(outcome[1])
            if preparation_failure is not None:
                log_event(
                    logger,
                    "telegram.album_preparation.failed",
                    level=logging.WARNING,
                    message="TikTok slideshow preparation failed",
                    platform="tiktok",
                    delivery=request.delivery,
                    item_count=len(extraction.media),
                    elapsed_ms=elapsed_ms(preparation_started_at),
                    error_type=type(preparation_failure).__name__,
                    success=False,
                )
                raise preparation_failure
            prepared = [item[0] for item in prepared_results]
            log_event(
                logger,
                "telegram.album_preparation.completed",
                message="TikTok slideshow preparation completed",
                platform="tiktok",
                delivery=request.delivery,
                item_count=len(prepared),
                conversion_count=len(converted_files),
                elapsed_ms=elapsed_ms(preparation_started_at),
                success=True,
            )
            if len(prepared) == 1:
                item = prepared[0]
                if request.delivery == "document":
                    single_call_fields = dict(fields)
                    single_call_fields["disable_content_type_detection"] = True
                    single_call_fields["document"] = "attach://media_0"
                    response = await self._call(
                        "sendDocument",
                        single_call_fields,
                        [TelegramUpload("media_0", item.file, item.filename, item.content_type)],
                    )
                else:
                    single_call_fields = dict(fields)
                    single_call_fields["photo"] = "attach://media_0"
                    response = await self._call(
                        "sendPhoto",
                        single_call_fields,
                        [TelegramUpload("media_0", item.file, item.filename, item.content_type)],
                    )
                return TelegramDeliveryOutcome([response])

            calls: list[TelegramCallResponse] = []
            batches = _album_batches(prepared)
            for batch_index, batch in enumerate(batches):
                batch_fields = dict(fields)
                if batch_index > 0:
                    batch_fields.pop("reply_parameters", None)
                media: list[dict[str, Any]] = []
                uploads: list[TelegramUpload] = []
                for item_index, item in enumerate(batch):
                    attach_name = f"media_{batch_index}_{item_index}"
                    media_item: dict[str, Any] = {
                        "type": "document" if request.delivery == "document" else "photo",
                        "media": f"attach://{attach_name}",
                    }
                    if request.delivery == "document":
                        media_item["disable_content_type_detection"] = True
                    media.append(media_item)
                    uploads.append(
                        TelegramUpload(attach_name, item.file, item.filename, item.content_type)
                    )
                batch_fields["media"] = media
                batch_started_at = perf_counter()
                response = await self._call("sendMediaGroup", batch_fields, uploads)
                calls.append(response)
                log_event(
                    logger,
                    "telegram.album_batch.completed",
                    level=logging.INFO if response.ok else logging.WARNING,
                    message="TikTok Telegram album batch completed",
                    platform="tiktok",
                    delivery=request.delivery,
                    batch_index=batch_index + 1,
                    batch_count=len(batches),
                    item_count=len(batch),
                    status_code=response.status_code,
                    elapsed_ms=elapsed_ms(batch_started_at),
                    success=response.ok,
                )
                if not response.ok:
                    break
            return TelegramDeliveryOutcome(calls)
        finally:
            _close_files([item.file for item in downloaded])
            _close_files(converted_files)
