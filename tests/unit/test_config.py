from __future__ import annotations

import pytest
from pydantic import ValidationError

from tt_scrap.config import Settings


@pytest.mark.parametrize(
    ("mb", "expected_mb"),
    [
        (None, 50),
        ("100", 100),
        ("0", 0),
    ],
)
def test_telegram_upload_limit_from_env(monkeypatch, mb, expected_mb):
    monkeypatch.delenv("TELEGRAM_UPLOAD_MAX_MB", raising=False)
    if mb is not None:
        monkeypatch.setenv("TELEGRAM_UPLOAD_MAX_MB", mb)

    settings = Settings(_env_file=None, tt_scrap_api_key="test-api-key-that-is-long-enough")

    assert settings.telegram_upload_max_mb == expected_mb


@pytest.mark.parametrize("mb", ["-1", "invalid"])
def test_telegram_upload_limit_rejects_invalid_mb(monkeypatch, mb):
    monkeypatch.setenv("TELEGRAM_UPLOAD_MAX_MB", mb)

    with pytest.raises(ValidationError, match="telegram_upload_max_mb"):
        Settings(_env_file=None, tt_scrap_api_key="test-api-key-that-is-long-enough")
