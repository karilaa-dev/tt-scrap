"""Consistent console/JSON logging and request correlation."""

from __future__ import annotations

import contextvars
import json
import logging
from datetime import UTC, datetime
from time import perf_counter
from typing import Any

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
    "fast_path",
    "height",
    "http_method",
    "item_count",
    "media_count",
    "media_type",
    "operation",
    "output_bytes",
    "path",
    "platform",
    "proxy_used",
    "queue_wait_ms",
    "request_bytes",
    "response_bytes",
    "retrying",
    "source_id",
    "source_kind",
    "stage",
    "status_code",
    "success",
    "telegram_description",
    "telegram_error_code",
    "telegram_method",
    "telegram_retry_after",
    "upload_bytes",
    "upload_count",
    "uses_separate_audio",
    "width",
    "worker_count",
}


def elapsed_ms(started_at: float) -> float:
    """Return stable monotonic elapsed time rounded for compact JSON logs."""
    return round((perf_counter() - started_at) * 1_000, 3)


def log_event(
    logger: logging.Logger,
    event: str,
    *,
    level: int = logging.INFO,
    message: str | None = None,
    **fields: Any,
) -> None:
    """Emit a safe structured event correlated with the active request."""
    extra = {"event": event}
    extra.update({name: value for name, value in fields.items() if name in _STRUCTURED_FIELDS})
    logger.log(level, message or event, extra=extra)


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, object] = {
            "timestamp": datetime.now(UTC).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
            "request_id": request_id_var.get(),
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
        timestamp = datetime.now(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")
        request_id = request_id_var.get()
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


def configure_logging(level: str, log_format: str = "console") -> None:
    handler = logging.StreamHandler()
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
