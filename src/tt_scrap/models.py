"""Public API and internal asset-context models."""

from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, HttpUrl

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
