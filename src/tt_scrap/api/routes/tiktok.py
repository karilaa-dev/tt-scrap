from __future__ import annotations

from typing import cast

from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse
from starlette.responses import Response

from ...models import (
    TelegramDeliveryRecord,
    TelegramMultiDeliveryResponse,
    TikTokExtractionRequest,
    TikTokExtractionResponse,
    TikTokMusicRequest,
    TikTokMusicResponse,
    TikTokTelegramDeliveryRequest,
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


@router.post("/telegram-deliveries", response_model=None)
async def deliver_tiktok_to_telegram(
    payload: TikTokTelegramDeliveryRequest, request: Request
) -> Response:
    outcome = await request.app.state.telegram_delivery.deliver(payload)
    if len(outcome.calls) == 1:
        call = outcome.calls[0]
        return Response(
            content=call.body,
            status_code=call.status_code,
            headers={"Content-Type": call.content_type},
        )

    ok = all(call.ok for call in outcome.calls)
    response = TelegramMultiDeliveryResponse(
        ok=ok,
        partial=not ok and any(call.ok for call in outcome.calls),
        deliveries=[
            TelegramDeliveryRecord(
                method=call.method,
                status_code=call.status_code,
                response=call.value,
            )
            for call in outcome.calls
        ],
    )
    return JSONResponse(
        status_code=200 if ok else 207,
        content=response.model_dump(mode="json"),
    )
