from __future__ import annotations

from typing import cast

from fastapi import APIRouter, Depends, Request
from starlette.responses import Response

from ...models import (
    TelegramAPIResponse,
    TelegramMultiDeliveryResponse,
    TikTokExtractionRequest,
    TikTokExtractionResponse,
    TikTokMusicRequest,
    TikTokMusicResponse,
    TikTokResolutionRequest,
    TikTokResolutionResponse,
    TikTokTelegramDeliveryRequest,
)
from ..dependencies import require_api_key
from ..responses import AUTHENTICATED_RESPONSES, TELEGRAM_DELIVERY_RESPONSES
from ..telegram import telegram_delivery_response

router = APIRouter(
    prefix="/v1/tiktok",
    tags=["tiktok"],
    dependencies=[Depends(require_api_key)],
    responses=AUTHENTICATED_RESPONSES,
)


@router.post(
    "/resolutions",
    response_model=TikTokResolutionResponse,
    operation_id="resolveTikTokUrl",
)
async def resolve_tiktok_url(
    payload: TikTokResolutionRequest, request: Request
) -> TikTokResolutionResponse:
    """Follow a TikTok share link and return its full post URL and numeric ID.

    This endpoint performs redirect resolution only. It does not call the TikTok
    metadata extractor, create asset references, or download any media.
    """
    result = await request.app.state.tiktok.resolve_url(str(payload.url))
    return cast(TikTokResolutionResponse, result)


@router.post(
    "/extractions",
    response_model=TikTokExtractionResponse,
    operation_id="extractTikTok",
)
async def extract_tiktok(
    payload: TikTokExtractionRequest, request: Request
) -> TikTokExtractionResponse:
    """Extract a TikTok post and return short-lived opaque asset references."""
    result = await request.app.state.tiktok.extract_url(str(payload.url), refresh=payload.refresh)
    return cast(TikTokExtractionResponse, result)


@router.post(
    "/music",
    response_model=TikTokMusicResponse,
    operation_id="extractTikTokMusic",
)
async def extract_tiktok_music(
    payload: TikTokMusicRequest, request: Request
) -> TikTokMusicResponse:
    """Extract the music attached to a TikTok post by numeric post ID."""
    result = await request.app.state.tiktok.extract_music(payload.video_id)
    return cast(TikTokMusicResponse, result)


@router.post(
    "/telegram-deliveries",
    response_model=TelegramAPIResponse | TelegramMultiDeliveryResponse,
    operation_id="deliverTikTokToTelegram",
    responses=TELEGRAM_DELIVERY_RESPONSES,
)
async def deliver_tiktok_to_telegram(
    payload: TikTokTelegramDeliveryRequest, request: Request
) -> Response:
    """Resolve, prepare, and upload TikTok media using the server's Telegram bot.

    `media` sends videos with metadata or slideshows as photo galleries;
    `document` sends original/file media without Telegram technical metadata; and
    `audio` sends the post's music. The caller supplies destination and optional
    Telegram parameters, but never multipart media fields or a bot token.

    A single Telegram call is returned byte-for-byte with Telegram's HTTP status.
    Multiple gallery batches use `TelegramMultiDeliveryResponse`; HTTP 207 means an
    earlier batch succeeded before a later one failed. Do not blindly retry the
    whole request after timeouts or ambiguous upload failures because that can
    duplicate messages.
    """
    outcome = await request.app.state.telegram_delivery.deliver(payload)
    return telegram_delivery_response(outcome)
