"""ASGI entry point."""

from __future__ import annotations

import os

import uvicorn

from .app import create_app
from .config import get_settings
from .logging import configure_logging

settings = get_settings()
app = create_app(settings)


def run() -> None:
    configure_logging(settings.log_level, settings.log_format)
    uvicorn.run(
        "tt_scrap.main:app",
        host="0.0.0.0",
        port=int(os.environ.get("PORT", "8000")),
        workers=1,
        access_log=False,
        log_config=None,
    )
