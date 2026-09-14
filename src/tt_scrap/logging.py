"""Consistent console/JSON logging and request correlation."""

from __future__ import annotations

import asyncio
import contextvars
import copy
import json
import logging
import threading
from collections import Counter, deque
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
    "failure_stage",
    "upstream_status_code",
    "upstream_error_type",
    "resolver_queue_wait_ms",
    "extraction_queue_wait_ms",
    "download_queue_wait_ms",
    "pipeline_queue_wait_ms",
    "upload_queue_wait_ms",
    "image_queue_wait_ms",
    "event_loop_lag_ms",
    "dropped_records",
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
    "failure_stage",
    "failure_reason",
    "upstream_status_code",
    "upstream_error_type",
    "resolver_queue_wait_ms",
    "extraction_queue_wait_ms",
    "download_queue_wait_ms",
    "pipeline_queue_wait_ms",
    "upload_queue_wait_ms",
    "image_queue_wait_ms",
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


def record_queue_wait(stage: str, milliseconds: float) -> None:
    """Retain the longest wait per stage; concurrent waits must not be summed."""
    key = f"{stage}_queue_wait_ms"
    if key in _SUMMARY_FIELDS and (context := request_log_var.get()):
        context.fields[key] = max(context.fields.get(key, 0.0), milliseconds)


def elapsed_ms(started_at: float) -> float:
    """Return stable monotonic elapsed time rounded for compact JSON logs."""
    return round((perf_counter() - started_at) * 1_000, 3)


_FAILURE_STAGES = {
    "tiktok.metadata",
    "tiktok.url_resolution",
    "tiktok.normalization",
    "instagram.upstream",
    "media.upstream_download",
    "media.upstream_relay",
    "telegram.api_call",
    "media.remux",
    "image.photo_conversion",
    "image.photo_normalization",
    "telegram.album_preparation",
}


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
    stage, _, outcome = event.rpartition(".")
    if stage in _FAILURE_STAGES and (context := request_log_var.get()):
        if outcome == "failed" or (outcome == "completed" and fields.get("success") is False):
            reason = fields.get("failure_reason")
            if reason is None:
                if outcome == "completed":
                    reason = "upstream_rejected"
                else:
                    reason = (
                        "http_status" if fields.get("status_code") is not None else "upstream_error"
                    )
            context.update(
                failure_stage=stage,
                failure_reason=reason,
                upstream_status_code=fields.get("status_code"),
                upstream_error_type=fields.get("error_type"),
            )
        # A sibling album item completing must not erase another item's failure.
        # The request middleware clears these fields when the whole request succeeds.
    if not logger.isEnabledFor(level):
        return
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
        if record.exc_text:
            payload["exception"] = record.exc_text
        elif record.exc_info:
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
        if record.exc_text:
            line = f"{line}\n{record.exc_text}"
        elif record.exc_info:
            line = f"{line}\n{self.formatException(record.exc_info)}"
        return line


class RequestIdFilter(logging.Filter):
    def filter(self, record: logging.LogRecord) -> bool:
        if not hasattr(record, "request_id"):
            record.request_id = request_id_var.get()
        return True


class BackgroundLogHandler(logging.Handler):
    """Bounded, ordered output; disk/stdout backpressure never blocks request tasks."""

    def __init__(self, sink: logging.Handler, capacity: int = 10_000) -> None:
        super().__init__()
        if capacity < 1:
            raise ValueError("Logging capacity must be positive")
        self.sink = sink
        self.capacity = capacity
        self._condition = threading.Condition()
        self._normal: deque[tuple[int, logging.LogRecord]] = deque()
        self._important: deque[tuple[int, logging.LogRecord]] = deque()
        self._sequence = 0
        self._active = False
        self._stopping = False
        self._dropped: Counter[str] = Counter()
        self._thread = threading.Thread(target=self._write, name="tt-scrap-logs", daemon=True)
        self._thread.start()

    def emit(self, record: logging.LogRecord) -> None:
        try:
            snapshot = copy.copy(record)
            snapshot.request_id = getattr(record, "request_id", request_id_var.get())
            snapshot.msg = record.getMessage()
            snapshot.args = None
            # Do not retain traceback frames and their request-local resources in the queue.
            if record.exc_info:
                snapshot.exc_text = logging.Formatter().formatException(record.exc_info)
                snapshot.exc_info = None
        except Exception:
            # Malformed diagnostics must not fail the caller. Count the drop
            # without recursively logging or exposing the original message.
            with self._condition:
                if not self._stopping:
                    self._dropped[record.levelname] += 1
            return
        with self._condition:
            if self._stopping:
                return
            if len(self._normal) + len(self._important) >= self.capacity:
                if snapshot.levelno >= logging.WARNING and self._normal:
                    _, evicted = self._normal.popleft()
                    self._dropped[evicted.levelname] += 1
                else:
                    self._dropped[snapshot.levelname] += 1
                    return
            queue = self._important if snapshot.levelno >= logging.WARNING else self._normal
            queue.append((self._sequence, snapshot))
            self._sequence += 1
            self._condition.notify()

    def _write(self) -> None:
        while True:
            with self._condition:
                self._condition.wait_for(lambda: self._normal or self._important or self._stopping)
                if not self._normal and not self._important:
                    return
                queue = self._normal
                if self._important and (not queue or self._important[0][0] < queue[0][0]):
                    queue = self._important
                _, record = queue.popleft()
                self._active = True
            try:
                self.sink.handle(record)
            except Exception:
                # A broken log sink must not crash the writer or recursively log to itself.
                with self._condition:
                    self._dropped[record.levelname] += 1
            else:
                with self._condition:
                    dropped = dict(self._dropped)
                    self._dropped.clear()
                if dropped:
                    summary = logging.LogRecord(
                        "tt_scrap.logging",
                        logging.WARNING,
                        "",
                        0,
                        "Log records dropped",
                        (),
                        None,
                    )
                    summary.event = "logging.records_dropped"
                    summary.request_id = "-"
                    summary.dropped_records = dropped
                    try:
                        self.sink.handle(summary)
                    except Exception:
                        with self._condition:
                            self._dropped.update(dropped)
            finally:
                with self._condition:
                    self._active = False
                    self._condition.notify_all()

    def flush(self) -> None:
        if threading.current_thread() is not self._thread:
            with self._condition:
                self._condition.wait_for(
                    lambda: not self._normal and not self._important and not self._active, timeout=5
                )

    def close(self) -> None:
        with self._condition:
            self._stopping = True
            self._condition.notify_all()
        if threading.current_thread() is not self._thread:
            self._thread.join(timeout=5)
        super().close()


def flush_logging() -> None:
    for handler in logging.getLogger().handlers:
        if isinstance(handler, BackgroundLogHandler):
            handler.flush()


async def monitor_event_loop(
    *, interval: float = 1.0, threshold: float = 1.0, cooldown: float = 60.0
) -> None:
    last_warning = float("-inf")
    while True:
        started = perf_counter()
        await asyncio.sleep(interval)
        now = perf_counter()
        lag = max(0.0, now - started - interval)
        if lag > threshold and now - last_warning >= cooldown:
            last_warning = now
            log_event(
                logging.getLogger(__name__),
                "runtime.event_loop.stalled",
                level=logging.WARNING,
                event_loop_lag_ms=round(lag * 1000, 3),
                success=False,
            )


def configure_logging(level: str, log_format: str = "console") -> None:
    sink = logging.StreamHandler()
    sink.setFormatter(JsonFormatter() if log_format == "json" else ConsoleFormatter())
    root = logging.getLogger()
    for previous in root.handlers:
        if isinstance(previous, BackgroundLogHandler):
            previous.close()
    root.handlers.clear()
    handler = BackgroundLogHandler(sink)
    handler.setLevel(level)
    handler.addFilter(RequestIdFilter())
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
