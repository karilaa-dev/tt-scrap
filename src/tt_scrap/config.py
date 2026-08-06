"""Application configuration."""

from __future__ import annotations

from functools import lru_cache

from pydantic import Field, SecretStr, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
        case_sensitive=False,
    )

    app_name: str = "tt-scrap"
    app_version: str = "0.1.0"
    environment: str = "production"
    log_level: str = "INFO"

    tt_scrap_api_key: SecretStr = Field(min_length=16)
    rapidapi_key: SecretStr = SecretStr("")
    ytdlp_cookies: str = ""

    proxy_file: str = ""
    proxy_data_only: bool = False
    proxy_include_host: bool = False

    url_resolve_max_retries: int = Field(default=3, ge=1, le=10)
    video_info_max_retries: int = Field(default=3, ge=1, le=10)
    download_max_retries: int = Field(default=3, ge=1, le=10)
    download_retry_base_delay: float = Field(default=1.0, ge=0, le=30)

    instagram_max_attempts: int = Field(default=3, ge=1, le=10)
    instagram_request_timeout_seconds: float = Field(default=2.0, gt=0, le=60)
    instagram_retry_delay_seconds: float = Field(default=0.5, ge=0, le=30)

    cache_ttl_seconds: int = Field(default=600, ge=30, le=86_400)
    cache_max_entries: int = Field(default=10_000, ge=100, le=1_000_000)
    tiktok_info_cache_ttl_seconds: int = Field(default=60, ge=1, le=3_600)
    extraction_concurrency: int = Field(default=32, ge=1, le=512)
    download_concurrency: int = Field(default=64, ge=1, le=1024)
    slideshow_concurrency: int = Field(default=8, ge=1, le=64)
    executor_workers: int = Field(default=32, ge=1, le=256)
    http_max_connections: int = Field(default=128, ge=1, le=2048)
    spool_threshold_bytes: int = Field(default=16 * 1024 * 1024, ge=1024)
    download_chunk_bytes: int = Field(default=64 * 1024, ge=1024)
    max_asset_bytes: int = Field(default=0, ge=0)
    max_video_duration: int = Field(default=0, ge=0)
    upstream_download_timeout_seconds: float = Field(default=60.0, gt=0, le=600)

    telegram_bot_token: SecretStr = SecretStr("")
    telegram_api_base_url: str = "https://api.telegram.org"
    telegram_upload_concurrency: int = Field(default=20, ge=1, le=256)
    telegram_upload_timeout_seconds: float = Field(default=600.0, gt=0, le=3_600)
    image_conversion_workers: int = Field(default=0, ge=0, le=256)

    @field_validator("log_level")
    @classmethod
    def normalize_log_level(cls, value: str) -> str:
        normalized = value.strip().upper()
        if normalized not in {"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}:
            raise ValueError("LOG_LEVEL is invalid")
        return normalized

    @field_validator("telegram_api_base_url")
    @classmethod
    def normalize_telegram_api_base_url(cls, value: str) -> str:
        normalized = value.strip().rstrip("/")
        if not normalized.startswith(("https://", "http://")):
            raise ValueError("TELEGRAM_API_BASE_URL must use http or https")
        return normalized


@lru_cache
def get_settings() -> Settings:
    return Settings()  # type: ignore[call-arg]
