from __future__ import annotations

from typing import cast

from fastapi import APIRouter, Depends, Request

from ...models import (
    TikTokExtractionRequest,
    TikTokExtractionResponse,
    TikTokMusicRequest,
    TikTokMusicResponse,
)
from ..dependencies import require_api_key

router = APIRouter(
    prefix="/v1/tiktok",
    tags=["tiktok"],
    dependencies=[Depends(require_api_key)],
)


@router.post("/extractions", response_model=TikTokExtractionResponse)
async def extract_tiktok(
    payload: TikTokExtractionRequest, request: Request
) -> TikTokExtractionResponse:
    result = await request.app.state.tiktok.extract_url(str(payload.url), refresh=payload.refresh)
    return cast(TikTokExtractionResponse, result)


@router.post("/music", response_model=TikTokMusicResponse)
async def extract_tiktok_music(
    payload: TikTokMusicRequest, request: Request
) -> TikTokMusicResponse:
    result = await request.app.state.tiktok.extract_music(payload.video_id)
    return cast(TikTokMusicResponse, result)
