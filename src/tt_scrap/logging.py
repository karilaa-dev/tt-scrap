"""Consistent console/JSON logging and request correlation."""

from __future__ import annotations

import contextvars
import json
import logging
from dataclasses import dataclass, field
from datetime import UTC, datetime
from time import perf_counter
from typing import Any, Literal

request_id_var: contextvars.ContextVar[str] = contextvars.ContextVar("request_id", default="-")

# Only explicitly approved fields are copied from LogRecord extras. This keeps
# credentials, signed URLs, captions, and request bodies out of structured logs
# even if a dependency attaches additional record attributes.
_STRUCTURED_FIELDS = {
    "attempt",
    "batch_count",
    "batch_index",
    "cache_hit",
    "cache_scope",
    "call_count",
    "compute_sha256",
    "compliant",
    "content_type",
    "conversion_count",
    "delivery",
    "elapsed_ms",
    "error_code",
    "error_type",
    "event",
    "execution",
    "fallback_count",
    "fast_path",
    "failure_reason",
    "height",
    "http_method",
    "http_max_connections",
    "httpx_version",
    "httpcore_version",
    "inflight_requests",
    "item_count",
    "media_count",
    "media_type",
    "operation",
    "output_bytes",
    "path",
    "partial",
    "platform",
    "pool_capacity",
    "proxy_used",
    "proxy_slot",
    "queue_wait_ms",
    "request_bytes",
    "response_bytes",
    "retrying",
    "retry_count",
    "source_id",
    "source_kind",
    "stage",
    "status_code",
    "success",
    "telegram_description",
    "telegram_error_code",
    "telegram_method",
    "telegram_retry_after",
    "thumbnail_skipped_count",
    "upload_bytes",
    "upload_count",
    "uses_separate_audio",
    "url_resolve_timeout_seconds",
    "url_resolve_pool_timeout_seconds",
    "width",
    "worker_count",
    "wait_seconds",
    "warmed_workers",
}


_SUMMARY_FIELDS = {
    "platform",
    "source_id",
    "delivery",
    "source_kind",
    "media_count",
    "media_type",
    "cache_hit",
    "cache_scope",
    "error_code",
    "error_type",
    "success",
    "partial",
    "call_count",
    "telegram_method",
    "telegram_error_code",
    "telegram_description",
    "telegram_retry_after",
    "batch_index",
    "batch_count",
}
RecoveryCounter = Literal["retry_count", "fallback_count", "thumbnail_skipped_count"]


@dataclass(slots=True)
class RequestLogContext:
    """One mutable context shared by a request's concurrent tasks."""

    fields: dict[str, Any] = field(default_factory=dict)
    retry_count: int = 0
    fallback_count: int = 0
    thumbnail_skipped_count: int = 0

    def update(self, **fields: Any) -> None:
        self.fields.update({key: value for key, value in fields.items() if key in _SUMMARY_FIELDS})

    def summary(self) -> dict[str, Any]:
        fields = self.fields.copy()
        for name in ("retry_count", "fallback_count", "thumbnail_skipped_count"):
            if value := getattr(self, name):
                fields[name] = value
        return fields


request_log_var: contextvars.ContextVar[RequestLogContext | None] = contextvars.ContextVar(
    "request_log", default=None
)


def bind_request_context(**fields: Any) -> None:
    if context := request_log_var.get():
        context.update(**fields)


def record_recovery(counter: RecoveryCounter) -> None:
    """Count actual recovery work even when detailed logging is disabled."""
    if context := request_log_var.get():
        setattr(context, counter, getattr(context, counter) + 1)


def elapsed_ms(started_at: float) -> float:
    """Return stable monotonic elapsed time rounded for compact JSON logs."""
    return round((perf_counter() - started_at) * 1_000, 3)


def log_event(
    logger: logging.Logger,
    event: str,
    *,
    level: int = logging.INFO,
    message: str | None = None,
    request_id: str | None = None,
    exc_info: BaseException | None = None,
    **fields: Any,
) -> None:
    """Emit a safe structured event correlated with the active request."""
    extra = {"event": event, "request_id": request_id or request_id_var.get()}
    extra.update({name: value for name, value in fields.items() if name in _STRUCTURED_FIELDS})
    logger.log(level, message or event, extra=extra, exc_info=exc_info)


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, object] = {
            "timestamp": datetime.fromtimestamp(record.created, UTC).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
            "request_id": getattr(record, "request_id", request_id_var.get()),
        }
        for name in _STRUCTURED_FIELDS:
            value = getattr(record, name, None)
            if value is not None:
                payload[name] = value
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, ensure_ascii=False, default=str)


class ConsoleFormatter(logging.Formatter):
    """Compact, human-readable formatter that retains structured diagnostics."""

    @staticmethod
    def _format_value(value: object) -> str:
        if isinstance(value, str):
            return json.dumps(value, ensure_ascii=False)
        return json.dumps(value, ensure_ascii=False, default=str)

    def format(self, record: logging.LogRecord) -> str:
        timestamp = (
            datetime.fromtimestamp(record.created, UTC)
            .isoformat(timespec="milliseconds")
            .replace("+00:00", "Z")
        )
        request_id = getattr(record, "request_id", request_id_var.get())
        message = record.getMessage()
        line = f"{timestamp} {record.levelname:<8} {record.name} [{request_id}] {message}"
        fields = [
            f"{name}={self._format_value(value)}"
            for name in sorted(_STRUCTURED_FIELDS)
            if (value := getattr(record, name, None)) is not None
        ]
        if fields:
            line = f"{line} {' '.join(fields)}"
        if record.exc_info:
            line = f"{line}\n{self.formatException(record.exc_info)}"
        return line


class RequestIdFilter(logging.Filter):
    def filter(self, record: logging.LogRecord) -> bool:
        if not hasattr(record, "request_id"):
            record.request_id = request_id_var.get()
        return True


def configure_logging(level: str, log_format: str = "console") -> None:
    handler = logging.StreamHandler()
    handler.setLevel(level)
    handler.addFilter(RequestIdFilter())
    handler.setFormatter(JsonFormatter() if log_format == "json" else ConsoleFormatter())
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)

    # Uvicorn installs dedicated handlers with its own formatter. Clear them so
    # startup, shutdown, and application events all use the selected format.
    for name in ("uvicorn", "uvicorn.error", "uvicorn.access"):
        uvicorn_logger = logging.getLogger(name)
        uvicorn_logger.handlers.clear()
        uvicorn_logger.propagate = True

    for name in ("httpx", "httpcore", "uvicorn.access"):
        logging.getLogger(name).setLevel(logging.WARNING)
