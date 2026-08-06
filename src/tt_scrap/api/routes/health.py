from __future__ import annotations

from fastapi import APIRouter, Request

from ...errors import ConfigurationError
from ...models import ErrorResponse, LiveResponse, ReadyResponse

router = APIRouter(tags=["health"])


@router.get("/health/live", response_model=LiveResponse, operation_id="getHealthLive")
async def live() -> LiveResponse:
    """Report whether the process can serve HTTP requests."""
    return LiveResponse()


@router.get(
    "/health/ready",
    response_model=ReadyResponse,
    operation_id="getHealthReady",
    responses={
        503: {
            "model": ErrorResponse,
            "description": "A required service credential is not configured",
        }
    },
)
async def ready(request: Request) -> ReadyResponse:
    """Report whether required configuration is present."""
    if not request.app.state.settings.rapidapi_key.get_secret_value():
        raise ConfigurationError("RAPIDAPI_KEY is not configured")
    return ReadyResponse()
