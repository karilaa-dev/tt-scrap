from __future__ import annotations

from typing import cast

from fastapi import APIRouter, Depends, Request
from starlette.responses import Response

from ...models import (
    InstagramExtractionRequest,
    InstagramExtractionResponse,
    InstagramTelegramDeliveryRequest,
    TelegramAPIResponse,
    TelegramMultiDeliveryResponse,
)
from ..dependencies import require_api_key
from ..responses import AUTHENTICATED_RESPONSES, TELEGRAM_DELIVERY_RESPONSES
from ..telegram import telegram_delivery_response

router = APIRouter(
    prefix="/v1/instagram",
    tags=["instagram"],
    dependencies=[Depends(require_api_key)],
    responses=AUTHENTICATED_RESPONSES,
)


@router.post(
    "/extractions",
    response_model=InstagramExtractionResponse,
    operation_id="extractInstagram",
)
async def extract_instagram(
    payload: InstagramExtractionRequest, request: Request
) -> InstagramExtractionResponse:
    """Extract an Instagram post into ordered opaque media and thumbnail references."""
    result = await request.app.state.instagram.extract_url(
        str(payload.url), refresh=payload.refresh
    )
    return cast(InstagramExtractionResponse, result)


@router.post(
    "/telegram-deliveries",
    response_model=TelegramAPIResponse | TelegramMultiDeliveryResponse,
    operation_id="deliverInstagramToTelegram",
    responses=TELEGRAM_DELIVERY_RESPONSES,
)
async def deliver_instagram_to_telegram(
    payload: InstagramTelegramDeliveryRequest, request: Request
) -> Response:
    """Resolve, prepare, and upload Instagram media using the server's Telegram bot.

    A single image uses `sendPhoto`, a single video uses `sendVideo`, and a carousel
    uses mixed `sendMediaGroup` batches. Document mode sends all items as files.
    Unsupported image formats and video thumbnails are converted asynchronously.

    One Telegram call is returned verbatim. Multiple batches return
    `TelegramMultiDeliveryResponse`; do not retry an ambiguous or partial upload
    automatically because that can duplicate messages.
    """
    outcome = await request.app.state.telegram_delivery.deliver_instagram(payload)
    return telegram_delivery_response(outcome)
