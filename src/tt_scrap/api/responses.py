"""Reusable OpenAPI response descriptions matching the application handlers."""

from __future__ import annotations

from typing import Any

from ..models import (
    ErrorResponse,
    TelegramAPIResponse,
    TelegramMultiDeliveryResponse,
)

AUTHENTICATED_RESPONSES: dict[int | str, dict[str, Any]] = {
    401: {
        "model": ErrorResponse,
        "description": "Missing or invalid tt-scrap bearer token",
    },
    422: {
        "model": ErrorResponse,
        "description": "Request validation or delivery-parameter error",
    },
    "default": {
        "model": ErrorResponse,
        "description": "Stable tt-scrap error envelope; inspect error.code",
    },
}

TELEGRAM_DELIVERY_RESPONSES: dict[int | str, dict[str, Any]] = {
    207: {
        "model": TelegramMultiDeliveryResponse,
        "description": "Some earlier Telegram album batches succeeded before a failure",
    },
    "default": {
        "model": TelegramAPIResponse | ErrorResponse,
        "description": (
            "A raw Telegram Bot API failure or a stable tt-scrap error envelope. "
            "Do not automatically retry an ambiguous upload response."
        ),
    },
}
