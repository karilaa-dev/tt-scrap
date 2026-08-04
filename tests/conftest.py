from __future__ import annotations

import pytest
from cryptography.fernet import Fernet

from tt_scrap.config import Settings


@pytest.fixture
def settings() -> Settings:
    return Settings(
        tt_scrap_api_key="test-api-key-that-is-long-enough",
        cache_encryption_key=Fernet.generate_key().decode(),
        redis_url="redis://localhost:6379/0",
        rapidapi_key="rapid-test-key",
        download_retry_base_delay=0,
        instagram_retry_delay_seconds=0,
        cache_ttl_seconds=60,
        executor_workers=2,
        extraction_concurrency=2,
        download_concurrency=2,
        http_max_connections=4,
    )
