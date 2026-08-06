"""Small streaming Telegram Bot API multipart client."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, BinaryIO

import aiohttp

from ..config import Settings
from ..errors import ConfigurationError, TelegramNetworkError, TelegramTimeoutError


@dataclass(frozen=True, slots=True)
class TelegramUpload:
    field_name: str
    file: BinaryIO
    filename: str
    content_type: str


@dataclass(frozen=True, slots=True)
class TelegramCallResponse:
    method: str
    status_code: int
    body: bytes
    content_type: str

    @property
    def value(self) -> dict[str, Any] | list[Any] | str | None:
        try:
            parsed = json.loads(self.body)
        except (UnicodeDecodeError, json.JSONDecodeError):
            return self.body.decode("utf-8", "replace")
        if isinstance(parsed, (dict, list, str)) or parsed is None:
            return parsed
        return str(parsed)

    @property
    def ok(self) -> bool:
        value = self.value
        return 200 <= self.status_code < 300 and isinstance(value, dict) and value.get("ok") is True


def _form_value(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (dict, list)):
        return json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    return str(value)


class TelegramClient:
    def __init__(self, settings: Settings) -> None:
        self._token = settings.telegram_bot_token.get_secret_value()
        self._base_url = settings.telegram_api_base_url
        timeout = aiohttp.ClientTimeout(
            total=settings.telegram_upload_timeout_seconds,
            connect=min(30.0, settings.telegram_upload_timeout_seconds),
            sock_read=settings.telegram_upload_timeout_seconds,
        )
        connector = aiohttp.TCPConnector(
            limit=settings.telegram_upload_concurrency,
            limit_per_host=settings.telegram_upload_concurrency,
            ttl_dns_cache=300,
        )
        self._session = aiohttp.ClientSession(
            connector=connector,
            timeout=timeout,
            trust_env=False,
        )

    @property
    def configured(self) -> bool:
        return bool(self._token)

    async def call(
        self,
        method: str,
        fields: dict[str, Any],
        uploads: list[TelegramUpload],
    ) -> TelegramCallResponse:
        if not self._token:
            raise ConfigurationError("Telegram delivery is not configured")
        form = aiohttp.FormData(quote_fields=False)
        for name, value in fields.items():
            if value is not None:
                form.add_field(name, _form_value(value))
        for upload in uploads:
            upload.file.seek(0)
            form.add_field(
                upload.field_name,
                upload.file,
                filename=upload.filename,
                content_type=upload.content_type,
            )
        # Never expose this URL in logs or exception messages: it contains the bot token.
        url = f"{self._base_url}/bot{self._token}/{method}"
        try:
            async with self._session.post(url, data=form) as response:
                body = await response.read()
                return TelegramCallResponse(
                    method=method,
                    status_code=response.status,
                    body=body,
                    content_type=response.headers.get("Content-Type", "application/json"),
                )
        except TimeoutError as exc:
            raise TelegramTimeoutError("Telegram upload timed out") from exc
        except aiohttp.ClientError as exc:
            raise TelegramNetworkError("Telegram upload failed before a response") from exc

    async def close(self) -> None:
        await self._session.close()
