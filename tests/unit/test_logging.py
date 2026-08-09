from __future__ import annotations

import json
import logging

from tt_scrap.logging import ConsoleFormatter, JsonFormatter, request_id_var


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
