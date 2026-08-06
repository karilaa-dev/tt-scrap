"""Public API and internal asset-context models."""

from __future__ import annotations

from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, HttpUrl, model_validator

AssetKind = Literal["video", "image", "audio", "cover", "thumbnail"]


class AssetDescriptor(BaseModel):
    asset_id: str
    kind: AssetKind
    position: int = 0
    download_url: str
    filename: str
    content_type: str | None = None
    expires_at: datetime


class TikTokMusicMetadata(BaseModel):
    title: str
    author: str
    duration_seconds: int
    cover: AssetDescriptor | None = None
    audio: AssetDescriptor | None = None


class TikTokExtractionRequest(BaseModel):
    model_config = ConfigDict(str_strip_whitespace=True)

    url: HttpUrl
    refresh: bool = False


class TikTokExtractionResponse(BaseModel):
    extraction_id: str
    platform: Literal["tiktok"] = "tiktok"
    source_id: str
    source_url: str
    resolved_url: str
    content_type: Literal["video", "slideshow"]
    cover: AssetDescriptor | None = None
    width: int | None = None
    height: int | None = None
    duration_seconds: int | None = None
    likes: int | None = None
    views: int | None = None
    media: list[AssetDescriptor]
    music: TikTokMusicMetadata | None = None
    expires_at: datetime


class TikTokMusicRequest(BaseModel):
    video_id: int = Field(gt=0)


class TikTokMusicResponse(BaseModel):
    extraction_id: str
    platform: Literal["tiktok"] = "tiktok"
    source_id: str
    title: str
    author: str
    duration_seconds: int
    cover: AssetDescriptor | None = None
    audio: AssetDescriptor
    expires_at: datetime


class TikTokTelegramSource(BaseModel):
    model_config = ConfigDict(str_strip_whitespace=True)

    url: HttpUrl | None = None
    extraction_id: str | None = None
    video_id: int | None = Field(default=None, gt=0)

    @model_validator(mode="after")
    def exactly_one_source(self) -> TikTokTelegramSource:
        supplied = sum(value is not None for value in (self.url, self.extraction_id, self.video_id))
        if supplied != 1:
            raise ValueError("Exactly one TikTok source must be supplied")
        return self


_RESERVED_TELEGRAM_MEDIA_FIELDS = {
    "audio",
    "cover",
    "document",
    "media",
    "photo",
    "thumbnail",
    "video",
}


class TelegramParameters(BaseModel):
    model_config = ConfigDict(extra="allow")

    chat_id: int | str
    business_connection_id: str | None = None
    message_thread_id: int | None = None
    direct_messages_topic_id: int | None = None
    receiver_user_id: int | None = None
    callback_query_id: str | None = None
    duration: int | None = None
    width: int | None = None
    height: int | None = None
    start_timestamp: int | None = None
    caption: str | None = None
    parse_mode: str | None = None
    caption_entities: list[dict[str, Any]] | None = None
    show_caption_above_media: bool | None = None
    has_spoiler: bool | None = None
    supports_streaming: bool | None = None
    disable_content_type_detection: bool | None = None
    performer: str | None = None
    title: str | None = None
    disable_notification: bool | None = None
    protect_content: bool | None = None
    allow_paid_broadcast: bool | None = None
    message_effect_id: str | None = None
    suggested_post_parameters: dict[str, Any] | None = None
    reply_parameters: dict[str, Any] | None = None
    reply_markup: dict[str, Any] | None = None

    @model_validator(mode="before")
    @classmethod
    def reject_reserved_media(cls, value: Any) -> Any:
        if isinstance(value, dict):
            reserved = sorted(_RESERVED_TELEGRAM_MEDIA_FIELDS.intersection(value))
            if reserved:
                raise ValueError(
                    "Telegram media fields are managed by tt-scrap: " + ", ".join(reserved)
                )
        return value


class TikTokTelegramDeliveryRequest(BaseModel):
    source: TikTokTelegramSource
    delivery: Literal["media", "document", "audio"] = "media"
    refresh: bool = False
    telegram: TelegramParameters

    @model_validator(mode="after")
    def validate_source_for_delivery(self) -> TikTokTelegramDeliveryRequest:
        if self.source.video_id is not None and self.delivery != "audio":
            raise ValueError("video_id is only supported for audio delivery")
        if self.source.extraction_id is not None and self.refresh:
            raise ValueError("refresh cannot be used with extraction_id")
        return self


class TelegramDeliveryRecord(BaseModel):
    method: str
    status_code: int
    response: dict[str, Any] | list[Any] | str | None


class TelegramMultiDeliveryResponse(BaseModel):
    ok: bool
    partial: bool
    deliveries: list[TelegramDeliveryRecord]


class InstagramExtractionRequest(BaseModel):
    model_config = ConfigDict(str_strip_whitespace=True)

    url: HttpUrl
    refresh: bool = False


class InstagramMediaItem(BaseModel):
    position: int
    media_type: Literal["video", "image"]
    quality: str | None = None
    asset: AssetDescriptor
    thumbnail: AssetDescriptor | None = None


class InstagramExtractionResponse(BaseModel):
    extraction_id: str
    platform: Literal["instagram"] = "instagram"
    source_url: str
    content_type: Literal["video", "image", "carousel"]
    media: list[InstagramMediaItem]
    expires_at: datetime


class AuxiliaryAssetFetchContext(BaseModel):
    upstream_url: str
    alternate_upstream_urls: list[str] = Field(default_factory=list)
    declared_content_type: str | None = None
    cookies: dict[str, str] = Field(default_factory=dict)


class AssetFetchContext(BaseModel):
    platform: Literal["tiktok", "instagram"]
    upstream_url: str
    alternate_upstream_urls: list[str] = Field(default_factory=list)
    filename: str
    kind: AssetKind
    declared_content_type: str | None = None
    referer: str | None = None
    cookies: dict[str, str] = Field(default_factory=dict)
    proxy_slot: int | None = None
    duration_seconds: int | None = None
    extraction_id: str | None = None
    audio: AuxiliaryAssetFetchContext | None = None


class ErrorDetail(BaseModel):
    code: str
    message: str
    request_id: str


class ErrorResponse(BaseModel):
    error: ErrorDetail


class LiveResponse(BaseModel):
    status: Literal["ok"] = "ok"


class ReadyResponse(BaseModel):
    status: Literal["ready"] = "ready"
