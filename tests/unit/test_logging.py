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
    output = capsys.readouterr().err
    assert "filtered-warning" not in output
    assert "visible-error" in output
