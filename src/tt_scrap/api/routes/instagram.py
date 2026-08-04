from __future__ import annotations

from typing import cast

from fastapi import APIRouter, Depends, Request

from ...models import InstagramExtractionRequest, InstagramExtractionResponse
from ..dependencies import require_api_key

router = APIRouter(
    prefix="/v1/instagram",
    tags=["instagram"],
    dependencies=[Depends(require_api_key)],
)


@router.post("/extractions", response_model=InstagramExtractionResponse)
async def extract_instagram(
    payload: InstagramExtractionRequest, request: Request
) -> InstagramExtractionResponse:
    result = await request.app.state.instagram.extract_url(
        str(payload.url), refresh=payload.refresh
    )
    return cast(InstagramExtractionResponse, result)
