"""TikTok and Instagram extraction-to-Telegram delivery orchestration."""

from __future__ import annotations

import asyncio
import io
import logging
from dataclasses import dataclass
from time import perf_counter
from typing import Any, BinaryIO, Literal

from ..cache import CacheStore
from ..config import Settings
from ..errors import ConfigurationError, ExtractionError, TelegramParameterError
from ..logging import elapsed_ms, log_event
from ..media import AssetDownloader, DownloadedAsset, ImagePreparationService
from ..media.downloader import filename_for_type
from ..media.images import is_native_telegram_photo
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
        self._pipeline_limit = asyncio.Semaphore(settings.telegram_upload_concurrency)

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
        return await self._downloader.download(context, compute_sha256=False)

    async def _download_with_cover(
        self, media: AssetDescriptor, cover: AssetDescriptor | None
    ) -> tuple[DownloadedAsset, DownloadedAsset | None]:
        media_task = asyncio.create_task(self._download(media))
        cover_task = asyncio.create_task(self._download(cover)) if cover is not None else None
        tasks = [media_task, *([cover_task] if cover_task is not None else [])]
        results = await asyncio.gather(*tasks, return_exceptions=True)
        media_result = results[0]
        if isinstance(media_result, BaseException):
            for result in results[1:]:
                if isinstance(result, DownloadedAsset):
                    result.file.close()
            raise media_result
        cover_result: DownloadedAsset | None = None
        if len(results) > 1:
            candidate = results[1]
            if isinstance(candidate, DownloadedAsset):
                cover_result = candidate
            elif isinstance(candidate, BaseException):
                log_event(
                    logger,
                    "telegram.thumbnail_download.failed",
                    level=logging.WARNING,
                    message="Media cover download failed; Telegram will generate a preview",
                    error_type=type(candidate).__name__,
                    success=False,
                )
        return media_result, cover_result

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

    async def _deliver_video(
        self, request: TikTokTelegramDeliveryRequest, extraction: TikTokExtractionResponse
    ) -> TelegramDeliveryOutcome:
        if request.delivery == "document":
            video = await self._download(extraction.media[0])
            cover = None
        else:
            video, cover = await self._download_with_cover(extraction.media[0], extraction.cover)
        extra_files: list[BinaryIO] = []
        try:
            filename = filename_for_type(extraction.media[0].filename, video.content_type)
            if request.delivery == "document":
                fields = self._fields(request.telegram, _DOCUMENT_FIELDS)
                self._default(
                    fields,
                    request.telegram,
                    "disable_content_type_detection",
                    True,
                )
                fields["document"] = "attach://document_file"
                response = await self._client.call(
                    "sendDocument",
                    fields,
                    [TelegramUpload("document_file", video.file, filename, video.content_type)],
                )
                return TelegramDeliveryOutcome([response])

            fields = self._fields(request.telegram, _VIDEO_FIELDS)
            self._default(fields, request.telegram, "duration", extraction.duration_seconds)
            self._default(fields, request.telegram, "width", extraction.width)
            self._default(fields, request.telegram, "height", extraction.height)
            self._default(fields, request.telegram, "supports_streaming", True)
            fields["video"] = "attach://video_file"
            uploads = [TelegramUpload("video_file", video.file, filename, video.content_type)]
            thumbnail = await self._thumbnail(cover, f"{extraction.source_id}_thumbnail.jpg")
            if thumbnail is not None:
                thumbnail_file, thumbnail_name = thumbnail
                extra_files.append(thumbnail_file)
                fields["thumbnail"] = "attach://thumbnail_file"
                fields["cover"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload("thumbnail_file", thumbnail_file, thumbnail_name, "image/jpeg")
                )
            response = await self._client.call("sendVideo", fields, uploads)
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
        audio, cover = await self._download_with_cover(audio_descriptor, music.cover)
        extra_files: list[BinaryIO] = []
        try:
            fields = self._fields(request.telegram, _AUDIO_FIELDS)
            self._default(fields, request.telegram, "duration", music.duration_seconds)
            self._default(fields, request.telegram, "title", music.title)
            self._default(fields, request.telegram, "performer", music.author)
            fields["audio"] = "attach://audio_file"
            filename = filename_for_type(audio_descriptor.filename, audio.content_type)
            uploads = [TelegramUpload("audio_file", audio.file, filename, audio.content_type)]
            thumbnail = await self._thumbnail(cover, f"{audio_descriptor.asset_id}_thumbnail.jpg")
            if thumbnail is not None:
                thumbnail_file, thumbnail_name = thumbnail
                extra_files.append(thumbnail_file)
                fields["thumbnail"] = "attach://thumbnail_file"
                uploads.append(
                    TelegramUpload("thumbnail_file", thumbnail_file, thumbnail_name, "image/jpeg")
                )
            response = await self._client.call("sendAudio", fields, uploads)
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
        use_thumbnail = request.delivery == "media" and item.media_type == "video"
        downloaded, thumbnail = await self._download_with_cover(
            item.asset,
            item.thumbnail if use_thumbnail else None,
        )
        extra_files: list[BinaryIO] = []
        try:
            prepared, owned_files = await self._prepare_instagram_item(
                item,
                downloaded,
                thumbnail,
                document=request.delivery == "document",
            )
            extra_files.extend(owned_files)
            if request.delivery == "document":
                fields = self._fields(request.telegram, _DOCUMENT_FIELDS)
                self._default(
                    fields,
                    request.telegram,
                    "disable_content_type_detection",
                    True,
                )
                fields["document"] = "attach://document_file"
                response = await self._client.call(
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
                fields = self._fields(request.telegram, _PHOTO_FIELDS)
                fields["photo"] = "attach://photo_file"
                response = await self._client.call(
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

            fields = self._fields(request.telegram, _VIDEO_FIELDS)
            self._default(fields, request.telegram, "supports_streaming", True)
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
            response = await self._client.call("sendVideo", fields, uploads)
            return TelegramDeliveryOutcome([response])
        finally:
            downloaded.file.close()
            if thumbnail is not None:
                thumbnail.file.close()
            _close_files(extra_files)

    async def _deliver_instagram_carousel(
        self,
        request: InstagramTelegramDeliveryRequest,
        extraction: InstagramExtractionResponse,
    ) -> TelegramDeliveryOutcome:
        document = request.delivery == "document"

        async def download_item(
            item: InstagramMediaItem,
        ) -> tuple[DownloadedAsset, DownloadedAsset | None]:
            cover = item.thumbnail if not document and item.media_type == "video" else None
            return await self._download_with_cover(item.asset, cover)

        download_started_at = perf_counter()
        download_outcomes = await asyncio.gather(
            *[download_item(item) for item in extraction.media],
            return_exceptions=True,
        )
        downloaded_items: list[tuple[DownloadedAsset, DownloadedAsset | None]] = []
        download_failure: BaseException | None = None
        for download_outcome in download_outcomes:
            if isinstance(download_outcome, BaseException):
                download_failure = download_failure or download_outcome
            else:
                downloaded_items.append(download_outcome)
        if download_failure is not None:
            for media, thumbnail in downloaded_items:
                media.file.close()
                if thumbnail is not None:
                    thumbnail.file.close()
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
                for media, thumbnail in downloaded_items
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
            video_fields = {
                name: fields.pop(name) for name in _ALBUM_VIDEO_FIELDS if name in fields
            }
            disable_content_type_detection = fields.pop(
                "disable_content_type_detection",
                True,
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
                response = await self._client.call("sendMediaGroup", batch_fields, uploads)
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
            for media, thumbnail in downloaded_items:
                media.file.close()
                if thumbnail is not None:
                    thumbnail.file.close()
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
        if is_native_telegram_photo(prefix) and await self._images.native_photo_is_compliant(
            downloaded.file,
            downloaded.size,
            downloaded.content_type,
            downloaded.declared_content_type,
        ):
            return _PreparedUpload(downloaded.file, filename, downloaded.content_type), None
        data = await self._images.read_file(downloaded.file)
        converted = await self._images.convert_photo(data, filename)
        converted_file = io.BytesIO(converted.data)
        return (
            _PreparedUpload(converted_file, converted.filename, converted.content_type),
            converted_file,
        )

    async def _deliver_slideshow(
        self, request: TikTokTelegramDeliveryRequest, extraction: TikTokExtractionResponse
    ) -> TelegramDeliveryOutcome:
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
                    single_fields = dict(fields)
                    single_fields["disable_content_type_detection"] = True
                    single_fields["document"] = "attach://media_0"
                    response = await self._client.call(
                        "sendDocument",
                        single_fields,
                        [TelegramUpload("media_0", item.file, item.filename, item.content_type)],
                    )
                else:
                    single_fields = dict(fields)
                    single_fields["photo"] = "attach://media_0"
                    response = await self._client.call(
                        "sendPhoto",
                        single_fields,
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
                response = await self._client.call("sendMediaGroup", batch_fields, uploads)
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
