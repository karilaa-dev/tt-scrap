"""Small streaming Telegram Bot API multipart client."""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncIterable
from dataclasses import dataclass
from time import perf_counter
from typing import Any, BinaryIO

import aiohttp

from ..config import Settings
from ..errors import (
    AssetTooLargeError,
    ConfigurationError,
    TelegramNetworkError,
    TelegramTimeoutError,
)
from ..logging import elapsed_ms, log_event
from ..resources import await_completion, memory_file, run_io

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class TelegramUpload:
    field_name: str
    file: BinaryIO | AsyncIterable[bytes]
    filename: str
    content_type: str
    size: int | None = None


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


class _SizedAsyncIterablePayload(aiohttp.payload.AsyncIterablePayload):
    def __init__(self, value: AsyncIterable[bytes], size: int, **kwargs: Any) -> None:
        super().__init__(value, **kwargs)
        self._size = size


class _BorrowedFilePayload(aiohttp.payload.Payload):
    """Stream a caller-owned file without fileno(), rollover, or implicit close."""

    _autoclose = True

    def __init__(self, file: BinaryIO, size: int | None, chunk_bytes: int, **kwargs: Any) -> None:
        super().__init__(file, **kwargs)
        self.file = file
        self._size = size
        self._chunk_bytes = chunk_bytes
        self._writers: set[asyncio.Task[Any]] = set()

    def decode(self, encoding: str = "utf-8", errors: str = "strict") -> str:
        raise TypeError("Media file payloads cannot be decoded as text")

    async def write(self, writer: Any) -> None:
        await self.write_with_length(writer, None)

    async def write_with_length(self, writer: Any, content_length: int | None) -> None:
        if task := asyncio.current_task():
            self._writers.add(task)
        in_memory = memory_file(self.file)
        remaining = self._size if content_length is None else content_length
        first = True

        def read_chunks() -> list[bytes]:
            if first:
                self.file.seek(0)
            available = remaining
            chunks = []
            # Bound disk read-ahead to 256 KiB while preserving individual 64 KiB
            # reads and writes. One worker job amortizes cancellation tracking.
            for _ in range(1 if in_memory else 4):
                if available == 0:
                    break
                amount = (
                    self._chunk_bytes if available is None else min(self._chunk_bytes, available)
                )
                chunk = self.file.read(amount)
                if not chunk:
                    break
                chunks.append(chunk)
                if available is not None:
                    available -= len(chunk)
            return chunks

        while remaining is None or remaining > 0:
            chunks = read_chunks() if in_memory else await run_io(read_chunks)
            first = False
            if not chunks:
                break
            for chunk in chunks:
                await writer.write(chunk)
                if remaining is not None:
                    remaining -= len(chunk)

    async def finish(self) -> None:
        for task in self._writers:
            if not task.done():
                task.cancel()
        if self._writers:
            await await_completion(asyncio.gather(*self._writers, return_exceptions=True))


def _file_size(upload: TelegramUpload) -> int | None:
    if upload.size is not None:
        return upload.size
    file = upload.file
    if not hasattr(file, "tell") or not hasattr(file, "seek"):
        return None
    try:
        position = file.tell()
        file.seek(0, 2)
        size = file.tell()
        file.seek(position)
        return size
    except (OSError, ValueError):
        return None


def _telegram_error_details(body: bytes) -> dict[str, Any]:
    try:
        value = json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return {}
    if not isinstance(value, dict):
        return {}
    details: dict[str, Any] = {}
    if isinstance(value.get("error_code"), int):
        details["telegram_error_code"] = value["error_code"]
    if isinstance(value.get("description"), str):
        details["telegram_description"] = value["description"][:500]
    parameters = value.get("parameters")
    if isinstance(parameters, dict) and isinstance(parameters.get("retry_after"), int):
        details["telegram_retry_after"] = parameters["retry_after"]
    return details


class TelegramClient:
    def __init__(self, settings: Settings) -> None:
        self._token = settings.telegram_bot_token.get_secret_value()
        self._base_url = settings.telegram_api_base_url
        self._upload_max_bytes = settings.telegram_upload_max_mb * 1024 * 1024
        # Match aiohttp's existing IOBasePayload reads. Relay chunks continue to
        # use DOWNLOAD_CHUNK_BYTES in the downloader.
        self._chunk_bytes = 64 * 1024
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
        started_at = perf_counter()
        upload_sizes = [
            _file_size(upload)
            if isinstance(upload.file, AsyncIterable) or memory_file(upload.file)
            else await run_io(_file_size, upload)
            for upload in uploads
        ]
        upload_bytes = (
            sum(size for size in upload_sizes if size is not None)
            if all(size is not None for size in upload_sizes)
            else None
        )
        if self._upload_max_bytes and any(
            size is not None and size > self._upload_max_bytes for size in upload_sizes
        ):
            log_event(
                logger,
                "telegram.api_call.rejected",
                level=logging.DEBUG,
                message="Telegram upload exceeds the configured size limit",
                telegram_method=method,
                success=False,
                upload_count=len(uploads),
                upload_bytes=upload_bytes,
                error_type="AssetTooLargeError",
                elapsed_ms=elapsed_ms(started_at),
            )
            raise AssetTooLargeError("Telegram upload exceeds TELEGRAM_UPLOAD_MAX_MB")
        form = aiohttp.FormData(quote_fields=False)
        borrowed: list[_BorrowedFilePayload] = []
        for name, value in fields.items():
            if value is not None:
                form.add_field(name, _form_value(value))
        for upload, size in zip(uploads, upload_sizes, strict=True):
            if isinstance(upload.file, AsyncIterable):
                if upload.size is None:
                    raise ValueError("Streaming Telegram uploads require a known size")
                upload_value: aiohttp.payload.Payload = _SizedAsyncIterablePayload(
                    upload.file,
                    upload.size,
                    filename=upload.filename,
                    content_type=upload.content_type,
                )
            else:
                upload_value = _BorrowedFilePayload(
                    upload.file,
                    size,
                    self._chunk_bytes,
                    filename=upload.filename,
                    content_type=upload.content_type,
                )
                borrowed.append(upload_value)
            form.add_field(
                upload.field_name,
                upload_value,
                filename=upload.filename,
                content_type=upload.content_type,
            )
        # Never expose this URL in logs or exception messages: it contains the bot token.
        url = f"{self._base_url}/bot{self._token}/{method}"
        try:
            async with self._session.post(url, data=form) as response:
                body = await response.read()
                result = TelegramCallResponse(
                    method=method,
                    status_code=response.status,
                    body=body,
                    content_type=response.headers.get("Content-Type", "application/json"),
                )
                log_event(
                    logger,
                    "telegram.api_call.completed",
                    level=logging.DEBUG,
                    message="Telegram API upload completed",
                    telegram_method=method,
                    status_code=response.status,
                    success=result.ok,
                    upload_count=len(uploads),
                    upload_bytes=upload_bytes,
                    response_bytes=len(body),
                    elapsed_ms=elapsed_ms(started_at),
                    **(_telegram_error_details(body) if not result.ok else {}),
                )
                return result
        except TimeoutError as exc:
            log_event(
                logger,
                "telegram.api_call.failed",
                level=logging.DEBUG,
                message="Telegram API upload timed out",
                telegram_method=method,
                success=False,
                upload_count=len(uploads),
                upload_bytes=upload_bytes,
                elapsed_ms=elapsed_ms(started_at),
                error_type=type(exc).__name__,
            )
            raise TelegramTimeoutError("Telegram upload timed out") from exc
        except aiohttp.ClientError as exc:
            log_event(
                logger,
                "telegram.api_call.failed",
                level=logging.DEBUG,
                message="Telegram API upload failed before a response",
                telegram_method=method,
                success=False,
                upload_count=len(uploads),
                upload_bytes=upload_bytes,
                elapsed_ms=elapsed_ms(started_at),
                error_type=type(exc).__name__,
            )
            raise TelegramNetworkError("Telegram upload failed before a response") from exc
        finally:
            # aiohttp may return an early rejection or cancel its body writer.
            # Wait until any threaded reads have stopped before returning file ownership.
            if any(not task.done() for payload in borrowed for task in payload._writers):
                await await_completion(asyncio.gather(*(item.finish() for item in borrowed)))

    async def close(self) -> None:
        await self._session.close()
