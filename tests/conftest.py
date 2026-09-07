from __future__ import annotations

import logging
from collections.abc import Iterator

import pytest

from tt_scrap.config import Settings
from tt_scrap.logging import RequestLogContext, request_id_var, request_log_var


@pytest.fixture
def log_records() -> Iterator[list[logging.LogRecord]]:
    """Capture application logs even when create_app replaces root handlers."""
    records: list[logging.LogRecord] = []

    class Capture(logging.Handler):
        def emit(self, record: logging.LogRecord) -> None:
            records.append(record)

    logger = logging.getLogger("tt_scrap")
    previous_level = logger.level
    handler = Capture()
    logger.setLevel(logging.INFO)
    logger.addHandler(handler)
    try:
        yield records
    finally:
        logger.removeHandler(handler)
        logger.setLevel(previous_level)


@pytest.fixture
def request_log_context() -> Iterator[RequestLogContext]:
    context = RequestLogContext()
    token = request_log_var.set(context)
    id_token = request_id_var.set("unit-request")
    try:
        yield context
    finally:
        request_log_var.reset(token)
        request_id_var.reset(id_token)


@pytest.fixture
def settings() -> Settings:
    return Settings(
        tt_scrap_api_key="test-api-key-that-is-long-enough",
        rapidapi_key="rapid-test-key",
        download_retry_base_delay=0,
        instagram_retry_delay_seconds=0,
        cache_ttl_seconds=60,
        cache_max_entries=1_000,
        executor_workers=2,
        extraction_concurrency=2,
        download_concurrency=2,
        http_max_connections=4,
        image_conversion_workers=1,
    )
