from __future__ import annotations

from fastapi import APIRouter, Request

from ...errors import ConfigurationError
from ...models import LiveResponse, ReadyResponse

router = APIRouter(tags=["health"])


@router.get("/health/live", response_model=LiveResponse)
async def live() -> LiveResponse:
    return LiveResponse()


@router.get("/health/ready", response_model=ReadyResponse)
async def ready(request: Request) -> ReadyResponse:
    if not request.app.state.settings.rapidapi_key.get_secret_value():
        raise ConfigurationError("RAPIDAPI_KEY is not configured")
    return ReadyResponse()
