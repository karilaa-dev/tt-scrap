"""JSON logging and request correlation."""

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


def configure_logging(level: str) -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(JsonFormatter())
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)
    for name in ("httpx", "httpcore", "uvicorn.access"):
        logging.getLogger(name).setLevel(logging.WARNING)
