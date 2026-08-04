"""ASGI entry point."""

from __future__ import annotations

import uvicorn

from .app import create_app

app = create_app()


def run() -> None:
    uvicorn.run("tt_scrap.main:app", host="0.0.0.0", port=8000, workers=1)
