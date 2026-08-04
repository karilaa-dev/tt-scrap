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
    cache_encryption_key: SecretStr
    redis_url: SecretStr = SecretStr("redis://:redis@127.0.0.1:6380/0")

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

    @field_validator("cache_encryption_key")
    @classmethod
    def validate_fernet_key(cls, value: SecretStr) -> SecretStr:
        from cryptography.fernet import Fernet

        try:
            Fernet(value.get_secret_value().encode())
        except (TypeError, ValueError) as exc:
            raise ValueError("CACHE_ENCRYPTION_KEY must be a valid Fernet key") from exc
        return value

    @field_validator("log_level")
    @classmethod
    def normalize_log_level(cls, value: str) -> str:
        normalized = value.strip().upper()
        if normalized not in {"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}:
            raise ValueError("LOG_LEVEL is invalid")
        return normalized


@lru_cache
def get_settings() -> Settings:
    return Settings()  # type: ignore[call-arg]
