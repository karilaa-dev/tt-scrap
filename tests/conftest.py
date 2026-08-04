from __future__ import annotations

import pytest

from tt_scrap.config import Settings


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
    )
