"""Shared HTTP response mapping for direct Telegram deliveries."""

from __future__ import annotations

from fastapi.responses import JSONResponse
from starlette.responses import Response

from ..models import TelegramDeliveryRecord, TelegramMultiDeliveryResponse
from ..telegram import TelegramDeliveryOutcome


def telegram_delivery_response(outcome: TelegramDeliveryOutcome) -> Response:
    """Preserve one Telegram call verbatim or wrap ordered multi-batch results."""
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
