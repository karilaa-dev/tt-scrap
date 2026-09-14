from __future__ import annotations

import json
import logging
from unittest.mock import patch

import pytest

from tt_scrap.logging import (
    ConsoleFormatter,
    JsonFormatter,
    RequestIdFilter,
    bind_request_context,
    configure_logging,
    flush_logging,
    log_event,
    record_recovery,
    request_id_var,
)


def test_json_formatter_includes_safe_structured_fields_only() -> None:
    token = request_id_var.set("request-123")
    try:
        record = logging.LogRecord(
            name="tt_scrap.test",
            level=logging.INFO,
            pathname=__file__,
            lineno=10,
            msg="Request completed",
            args=(),
            exc_info=None,
        )
        record.event = "http.request.completed"
        record.elapsed_ms = 12.345
        record.status_code = 200
        record.bot_token = "must-not-be-logged"

        payload = json.loads(JsonFormatter().format(record))
    finally:
        request_id_var.reset(token)

    assert payload["request_id"] == "request-123"
    assert payload["event"] == "http.request.completed"
    assert payload["elapsed_ms"] == 12.345
    assert payload["status_code"] == 200
    assert "bot_token" not in payload


def test_console_formatter_is_readable_and_includes_safe_fields_only() -> None:
    token = request_id_var.set("request-123")
    try:
        record = logging.LogRecord(
            name="tt_scrap.test",
            level=logging.INFO,
            pathname=__file__,
            lineno=10,
            msg="Request completed",
            args=(),
            exc_info=None,
        )
        record.event = "http.request.completed"
        record.elapsed_ms = 12.345
        record.status_code = 200
        record.bot_token = "must-not-be-logged"

        output = ConsoleFormatter().format(record)
    finally:
        request_id_var.reset(token)

    assert "INFO" in output
    assert "tt_scrap.test [request-123] Request completed" in output
    assert 'event="http.request.completed"' in output
    assert "elapsed_ms=12.345" in output
    assert "status_code=200" in output
    assert "must-not-be-logged" not in output


@pytest.mark.parametrize("formatter", [JsonFormatter(), ConsoleFormatter()])
def test_event_retains_request_id_and_approved_diagnostics_after_context_reset(
    formatter, log_records
) -> None:
    token = request_id_var.set("captured-request")
    try:
        log_event(
            logging.getLogger("tt_scrap.test"),
            "diagnostic",
            retry_count=2,
            fallback_count=1,
            thumbnail_skipped_count=1,
            pool_capacity=4,
            warmed_workers=1,
            execution="thread",
            partial=True,
            bot_token="secret-bot-token",
            upstream_url="https://cdn.test/secret-url",
        )
    finally:
        request_id_var.reset(token)
    output = formatter.format(log_records[0])
    assert "captured-request" in output
    for field in (
        "retry_count",
        "fallback_count",
        "thumbnail_skipped_count",
        "pool_capacity",
        "warmed_workers",
        "execution",
        "partial",
    ):
        assert field in output
    assert "secret" not in output


def test_plain_log_record_captures_request_id_at_handler_time() -> None:
    token = request_id_var.set("plain-request")
    try:
        record = logging.makeLogRecord({"msg": "operational warning"})
        assert RequestIdFilter().filter(record)
    finally:
        request_id_var.reset(token)
    assert json.loads(JsonFormatter().format(record))["request_id"] == "plain-request"


def test_recovery_summary_is_independent_of_debug_filtering(request_log_context, log_records):
    bind_request_context(platform="tiktok", source_id="123", bot_token="secret")
    record_recovery("retry_count")
    log_event(logging.getLogger("tt_scrap.test"), "attempt", level=logging.DEBUG)
    assert not log_records
    assert request_log_context.summary() == {
        "platform": "tiktok",
        "source_id": "123",
        "retry_count": 1,
    }


def test_configured_level_filters_dependencies_with_explicit_lower_levels(capsys):
    root = logging.getLogger()
    # Keep this configuration check isolated from pytest's handlers.
    with patch.object(root, "handlers", []), patch.object(root, "level", root.level):
        configure_logging("ERROR")
        dependency = logging.getLogger("logging-test-dependency")
        with patch.object(dependency, "level", logging.INFO):
            dependency.warning("filtered-warning")
            dependency.error("visible-error")
        flush_logging()
        for handler in root.handlers:
            handler.close()
    output = capsys.readouterr().err
    assert "filtered-warning" not in output
    assert "visible-error" in output


def test_background_logging_snapshots_context_and_prioritizes_warnings():
    import threading

    from tt_scrap.logging import BackgroundLogHandler

    entered, release = threading.Event(), threading.Event()
    records = []

    class SlowSink(logging.Handler):
        def emit(self, record):
            if not records:
                entered.set()
                assert release.wait(2)
            records.append(record)

    handler = BackgroundLogHandler(SlowSink(), capacity=2)
    try:
        handler.handle(logging.makeLogRecord({"msg": "blocking", "levelno": logging.INFO}))
        assert entered.wait(1)
        token = request_id_var.set("before-enqueue")
        try:
            for message, level in [
                ("evicted", logging.INFO),
                ("retained", logging.INFO),
                ("dropped", logging.INFO),
                ("warning", logging.WARNING),
            ]:
                handler.handle(logging.LogRecord("test", level, "", 0, message, (), None))
        finally:
            request_id_var.reset(token)
        assert len(handler._normal) + len(handler._important) == 2
        release.set()
        handler.flush()
        assert [record.msg for record in records if not hasattr(record, "event")] == [
            "blocking",
            "retained",
            "warning",
        ]
        assert records[-1].request_id == "before-enqueue"
        dropped = [record for record in records if hasattr(record, "dropped_records")]
        assert len(dropped) == 1
        assert dropped[0].dropped_records == {"INFO": 2}
    finally:
        release.set()
        handler.close()


async def test_loop_lag_warning_is_rate_limited(monkeypatch, log_records):
    import asyncio

    from tt_scrap.logging import monitor_event_loop

    times = iter([0, 3, 3, 6, 62, 65])
    monkeypatch.setattr("tt_scrap.logging.perf_counter", lambda: next(times))
    calls = 0

    async def sleep(interval):
        nonlocal calls
        assert interval == 1
        calls += 1
        if calls > 3:
            raise asyncio.CancelledError

    # Stop before requesting a seventh clock sample.
    def clock():
        try:
            return next(times)
        except StopIteration:
            raise asyncio.CancelledError from None

    monkeypatch.setattr("tt_scrap.logging.perf_counter", clock)
    monkeypatch.setattr("tt_scrap.logging.asyncio.sleep", sleep)
    with pytest.raises(asyncio.CancelledError):
        await monitor_event_loop()
    assert len(log_records) == 2
    assert all(record.event_loop_lag_ms == 2000 for record in log_records)


def test_sibling_success_does_not_erase_upstream_failure(request_log_context):
    logger = logging.getLogger("tt_scrap.test")
    log_event(
        logger,
        "media.upstream_download.failed",
        level=logging.DEBUG,
        status_code=404,
        failure_reason="http_status",
        error_type="NetworkError",
    )
    log_event(logger, "media.upstream_download.completed", level=logging.DEBUG, success=True)
    assert request_log_context.summary()["upstream_status_code"] == 404
    assert request_log_context.summary()["failure_stage"] == "media.upstream_download"
