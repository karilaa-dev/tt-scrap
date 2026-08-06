"""Public API and internal asset-context models."""

from __future__ import annotations

from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, HttpUrl, model_validator

AssetKind = Literal["video", "image", "audio", "cover", "thumbnail"]


class AssetDescriptor(BaseModel):
    """Temporary opaque reference to media that tt-scrap can download on demand."""

    asset_id: str = Field(description="Opaque asset token; do not parse or persist it")
    kind: AssetKind = Field(description="Logical purpose of this asset")
    position: int = Field(default=0, description="Zero-based order within the post")
    download_url: str = Field(
        description=("Relative authenticated download path. Resolve against the tt-scrap base URL")
    )
    filename: str = Field(description="Suggested filename before final format detection")
    content_type: str | None = Field(
        default=None,
        description="Declared MIME type when known; trust the download response type",
    )
    expires_at: datetime = Field(description="Expiry time for this asset reference")


class TikTokMusicMetadata(BaseModel):
    """Music information embedded in a TikTok extraction response."""

    title: str
    author: str
    duration_seconds: int
    cover: AssetDescriptor | None = None
    audio: AssetDescriptor | None = None


class TikTokExtractionRequest(BaseModel):
    """Extract metadata and temporary asset references from a TikTok URL."""

    model_config = ConfigDict(
        str_strip_whitespace=True,
        json_schema_extra={
            "examples": [
                {
                    "url": "https://www.tiktok.com/@creator/video/1234567890123456789",
                    "refresh": False,
                }
            ]
        },
    )

    url: HttpUrl = Field(description="Public TikTok video, slideshow, or short URL")
    refresh: bool = Field(
        default=False,
        description="Bypass the short metadata cache and create a new extraction",
    )


class TikTokExtractionResponse(BaseModel):
    """Normalized TikTok post information with no upstream CDN URLs exposed."""

    extraction_id: str = Field(
        description=(
            "Short-lived extraction reference reusable by Telegram delivery; default TTL 60s"
        )
    )
    platform: Literal["tiktok"] = "tiktok"
    source_id: str
    source_url: str
    resolved_url: str
    content_type: Literal["video", "slideshow"] = Field(
        description="Determines whether media contains one video or ordered slideshow images"
    )
    cover: AssetDescriptor | None = None
    width: int | None = None
    height: int | None = None
    duration_seconds: int | None = None
    likes: int | None = None
    views: int | None = None
    media: list[AssetDescriptor] = Field(
        description="One selected best-quality video or ordered slideshow images"
    )
    music: TikTokMusicMetadata | None = None
    expires_at: datetime


class TikTokMusicRequest(BaseModel):
    """Extract TikTok music using a numeric post ID."""

    model_config = ConfigDict(json_schema_extra={"examples": [{"video_id": 1234567890123456789}]})

    video_id: int = Field(gt=0, description="Numeric TikTok post ID")


class TikTokMusicResponse(BaseModel):
    """TikTok music metadata and temporary audio/cover asset references."""

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
    """Exactly one source selector for Telegram delivery.

    `url` performs or reuses extraction, `extraction_id` reuses recent extraction
    context, and `video_id` is accepted only for audio delivery.
    """

    model_config = ConfigDict(str_strip_whitespace=True)

    url: HttpUrl | None = Field(default=None, description="TikTok URL to resolve and extract")
    extraction_id: str | None = Field(
        default=None,
        description="Recent extraction_id; avoids URL resolution and extraction",
    )
    video_id: int | None = Field(
        default=None,
        gt=0,
        description="Numeric TikTok post ID; only valid when delivery is audio",
    )

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
    """Destination and caller-controlled Telegram Bot API parameters.

    Extra Telegram fields are forwarded when compatible with the selected method.
    tt-scrap owns all multipart media fields, so callers must not supply `video`,
    `audio`, `photo`, `document`, `media`, `thumbnail`, or `cover`. Fields supported
    by one Telegram method can be rejected for another delivery/media combination.
    """

    model_config = ConfigDict(extra="allow")

    chat_id: int | str = Field(
        description="Target chat ID or @channelusername accepted by the configured bot"
    )
    business_connection_id: str | None = None
    message_thread_id: int | None = None
    direct_messages_topic_id: int | None = None
    receiver_user_id: int | None = None
    callback_query_id: str | None = None
    duration: int | None = None
    width: int | None = None
    height: int | None = None
    start_timestamp: int | None = None
    caption: str | None = Field(
        default=None,
        description=(
            "Caption for single calls; Instagram carousels use the first item; "
            "TikTok slideshows reject it"
        ),
    )
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
    """Request server-side TikTok preparation and direct Telegram upload.

    The Telegram bot token is server configuration, not part of this request.
    `refresh` cannot be used with `source.extraction_id`.
    """

    model_config = ConfigDict(
        json_schema_extra={
            "examples": [
                {
                    "source": {
                        "url": ("https://www.tiktok.com/@creator/video/1234567890123456789")
                    },
                    "delivery": "media",
                    "refresh": False,
                    "telegram": {"chat_id": 123456789, "caption": "Optional caption"},
                },
                {
                    "source": {"extraction_id": "recent-extraction-id"},
                    "delivery": "document",
                    "telegram": {"chat_id": 123456789},
                },
                {
                    "source": {"video_id": 1234567890123456789},
                    "delivery": "audio",
                    "telegram": {"chat_id": 123456789},
                },
            ]
        }
    )

    source: TikTokTelegramSource = Field(description="Exactly one TikTok source selector")
    delivery: Literal["media", "document", "audio"] = Field(
        default="media",
        description=(
            "media sends playable media/galleries; document sends files; audio sends music"
        ),
    )
    refresh: bool = Field(
        default=False,
        description="Bypass cached URL extraction; invalid with source.extraction_id",
    )
    telegram: TelegramParameters = Field(
        description="Telegram destination and optional method-compatible overrides"
    )

    @model_validator(mode="after")
    def validate_source_for_delivery(self) -> TikTokTelegramDeliveryRequest:
        if self.source.video_id is not None and self.delivery != "audio":
            raise ValueError("video_id is only supported for audio delivery")
        if self.source.extraction_id is not None and self.refresh:
            raise ValueError("refresh cannot be used with extraction_id")
        return self


class TelegramDeliveryRecord(BaseModel):
    """One attempted Telegram album batch in delivery order."""

    method: str
    status_code: int
    response: dict[str, Any] | list[Any] | str | None


class TelegramAPIResponse(BaseModel):
    """Raw response envelope returned by the Telegram Bot API."""

    model_config = ConfigDict(extra="allow")

    ok: bool
    result: Any | None = None
    error_code: int | None = None
    description: str | None = None
    parameters: dict[str, Any] | None = None


class TelegramMultiDeliveryResponse(BaseModel):
    """Result used when a slideshow requires multiple Telegram album calls."""

    ok: bool
    partial: bool
    deliveries: list[TelegramDeliveryRecord]


class InstagramExtractionRequest(BaseModel):
    """Extract metadata and assets from an Instagram post URL."""

    model_config = ConfigDict(
        str_strip_whitespace=True,
        json_schema_extra={
            "examples": [{"url": "https://www.instagram.com/reel/SHORTCODE/", "refresh": False}]
        },
    )

    url: HttpUrl = Field(description="Public Instagram post, reel, TV, or story URL")
    refresh: bool = Field(
        default=False,
        description="Bypass cached extraction and create a new extraction_id",
    )


class InstagramMediaItem(BaseModel):
    """One ordered Instagram media item with an optional thumbnail."""

    position: int = Field(description="Zero-based source carousel order")
    media_type: Literal["video", "image"] = Field(
        description="Telegram media type selected for this item"
    )
    quality: str | None = Field(default=None, description="Upstream quality label when supplied")
    asset: AssetDescriptor = Field(description="Temporary image or video asset")
    thumbnail: AssetDescriptor | None = Field(
        default=None,
        description="Optional video thumbnail prepared for Telegram media mode",
    )


class InstagramExtractionResponse(BaseModel):
    """Normalized Instagram post information and temporary asset references."""

    extraction_id: str = Field(
        description="Cached extraction reference reusable by Instagram Telegram delivery"
    )
    platform: Literal["instagram"] = "instagram"
    source_url: str
    content_type: Literal["video", "image", "carousel"] = Field(
        description="Single media kind or an ordered mixed carousel"
    )
    media: list[InstagramMediaItem] = Field(description="Ordered extracted media items")
    expires_at: datetime = Field(description="Expiry time for the associated asset references")


class InstagramTelegramSource(BaseModel):
    """Exactly one Instagram source selector for Telegram delivery."""

    model_config = ConfigDict(str_strip_whitespace=True)

    url: HttpUrl | None = Field(default=None, description="Instagram post or reel URL")
    extraction_id: str | None = Field(
        default=None,
        description="Recent Instagram extraction_id; avoids another upstream request",
    )

    @model_validator(mode="after")
    def exactly_one_source(self) -> InstagramTelegramSource:
        if (self.url is None) == (self.extraction_id is None):
            raise ValueError("Exactly one Instagram source must be supplied")
        return self


class InstagramTelegramDeliveryRequest(BaseModel):
    """Request direct Telegram delivery of an Instagram post.

    `media` sends photos, videos, or mixed media groups. `document` sends every
    item as a file and preserves original image bytes. The Telegram bot token is
    configured on tt-scrap and is not part of this request.
    """

    model_config = ConfigDict(
        json_schema_extra={
            "examples": [
                {
                    "source": {"url": "https://www.instagram.com/reel/SHORTCODE/"},
                    "delivery": "media",
                    "refresh": False,
                    "telegram": {"chat_id": 123456789, "caption": "Optional caption"},
                },
                {
                    "source": {"extraction_id": "recent-extraction-id"},
                    "delivery": "document",
                    "telegram": {"chat_id": 123456789},
                },
            ]
        }
    )

    source: InstagramTelegramSource
    delivery: Literal["media", "document"] = Field(
        default="media",
        description="media sends displayable media; document sends original/file media",
    )
    refresh: bool = Field(
        default=False,
        description="Bypass cached URL extraction; invalid with source.extraction_id",
    )
    telegram: TelegramParameters

    @model_validator(mode="after")
    def validate_source_for_delivery(self) -> InstagramTelegramDeliveryRequest:
        if self.source.extraction_id is not None and self.refresh:
            raise ValueError("refresh cannot be used with extraction_id")
        return self


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
    """Stable machine-readable error details."""

    code: str = Field(description="Stable error identifier suitable for branching")
    message: str = Field(description="Human-readable diagnostic; wording may change")
    request_id: str = Field(description="Correlation ID to retain for logs and support")


class ErrorResponse(BaseModel):
    """Error envelope returned by tt-scrap exception handlers."""

    error: ErrorDetail


class LiveResponse(BaseModel):
    status: Literal["ok"] = "ok"


class ReadyResponse(BaseModel):
    status: Literal["ready"] = "ready"
