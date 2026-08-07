from __future__ import annotations

import hashlib
import io
import json
import logging
from pathlib import Path
from unittest.mock import AsyncMock

import httpx
import pytest
from pydantic import SecretStr

from tt_scrap.app import create_app
from tt_scrap.media import DownloadedAsset
from tt_scrap.models import AssetFetchContext, TikTokResolutionResponse
from tt_scrap.openapi import build_openapi_schema
from tt_scrap.telegram import TelegramCallResponse, TelegramDeliveryOutcome


class FakeDownloader:
    def __init__(self, payload: bytes) -> None:
        self.payload = payload

    async def download(self, context: AssetFetchContext) -> DownloadedAsset:
        return DownloadedAsset(
            file=io.BytesIO(self.payload),
            size=len(self.payload),
            sha256=hashlib.sha256(self.payload).hexdigest(),
            content_type="image/jpeg",
        )

    async def close(self) -> None:
        return None


class FakeTelegramDelivery:
    def __init__(self, calls: list[TelegramCallResponse]) -> None:
        self.calls = calls

    async def deliver(self, payload) -> TelegramDeliveryOutcome:
        return TelegramDeliveryOutcome(self.calls)

    async def deliver_instagram(self, payload) -> TelegramDeliveryOutcome:
        return TelegramDeliveryOutcome(self.calls)


@pytest.mark.asyncio
async def test_health_auth_validation_and_asset_delivery(settings) -> None:
    app = create_app(settings)
    async with app.router.lifespan_context(app):
        original = app.state.asset_downloader
        await original.close()
        payload = b"\xff\xd8\xfftest-image"
        app.state.asset_downloader = FakeDownloader(payload)
        token = await app.state.cache.store_asset(
            AssetFetchContext(
                platform="instagram",
                upstream_url="https://cdn.test/private-url",
                filename="photo.bin",
                kind="image",
            )
        )

        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            live = await client.get("/health/live")
            ready = await client.get("/health/ready")
            openapi = await client.get("/openapi.json")
            unauthorized = await client.get(f"/v1/assets/{token}")
            invalid = await client.post(
                "/v1/tiktok/extractions",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json={"url": "not-a-url"},
            )
            asset = await client.get(
                f"/v1/assets/{token}",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
            )

        assert live.status_code == 200
        assert live.headers["server-timing"].startswith("app;dur=")
        assert ready.status_code == 200
        assert openapi.status_code == 200
        assert openapi.json() == app.openapi()
        assert unauthorized.status_code == 401
        assert unauthorized.json()["error"]["code"] == "authentication_required"
        assert unauthorized.headers["X-Request-ID"]
        assert invalid.status_code == 422
        assert invalid.json()["error"]["code"] == "validation_error"
        assert asset.status_code == 200
        assert asset.content == payload
        assert asset.headers["content-type"] == "image/jpeg"
        assert asset.headers["content-length"] == str(len(payload))
        assert asset.headers["content-disposition"].endswith('filename="photo.jpg"')
        assert asset.headers["x-content-sha256"] == hashlib.sha256(payload).hexdigest()
        assert "cdn.test" not in asset.text


@pytest.mark.asyncio
async def test_expired_asset_has_stable_error(settings) -> None:
    app = create_app(settings)
    async with app.router.lifespan_context(app):
        records: list[logging.LogRecord] = []

        class RecordHandler(logging.Handler):
            def emit(self, record: logging.LogRecord) -> None:
                records.append(record)

        handler = RecordHandler()
        app_logger = logging.getLogger("tt_scrap.app")
        app_logger.addHandler(handler)
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        try:
            async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
                response = await client.get(
                    "/v1/assets/missing-secret-token",
                    headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                )
        finally:
            app_logger.removeHandler(handler)
        assert response.status_code == 404
        assert response.json()["error"]["code"] == "asset_not_found_or_expired"
        completed = next(
            record
            for record in records
            if getattr(record, "event", None) == "http.request.completed"
        )
        assert completed.path == "/v1/assets/{token}"
        assert completed.status_code == 404
        assert completed.elapsed_ms >= 0
        assert "missing-secret-token" not in completed.getMessage()


def test_openapi_has_expected_contract(settings) -> None:
    app = create_app(settings)
    schema = app.openapi()
    assert "/v1/tiktok/resolutions" in schema["paths"]
    assert "/v1/tiktok/extractions" in schema["paths"]
    assert "/v1/tiktok/music" in schema["paths"]
    assert "/v1/tiktok/telegram-deliveries" in schema["paths"]
    assert "/v1/instagram/extractions" in schema["paths"]
    assert "/v1/instagram/telegram-deliveries" in schema["paths"]
    assert "/v1/assets/{token}" in schema["paths"]
    assert "RawVideoResponse" not in schema["components"]["schemas"]
    assert "HTTPValidationError" not in schema["components"]["schemas"]
    assert "## Client integration" in schema["info"]["description"]
    resolution = schema["paths"]["/v1/tiktok/resolutions"]["post"]
    assert resolution["operationId"] == "resolveTikTokUrl"
    assert resolution["responses"]["200"]["content"]["application/json"]["schema"] == {
        "$ref": "#/components/schemas/TikTokResolutionResponse"
    }
    assert (
        schema["components"]["schemas"]["TikTokResolutionResponse"]["properties"]["source_id"][
            "pattern"
        ]
        == r"^\d+$"
    )
    delivery = schema["paths"]["/v1/tiktok/telegram-deliveries"]["post"]
    assert delivery["operationId"] == "deliverTikTokToTelegram"
    assert delivery["responses"]["200"]["content"]["application/json"]["schema"]["anyOf"]
    assert delivery["responses"]["207"]["content"]["application/json"]["schema"] == {
        "$ref": "#/components/schemas/TelegramMultiDeliveryResponse"
    }
    assert delivery["responses"]["422"]["content"]["application/json"]["schema"] == {
        "$ref": "#/components/schemas/ErrorResponse"
    }
    instagram_delivery = schema["paths"]["/v1/instagram/telegram-deliveries"]["post"]
    assert instagram_delivery["operationId"] == "deliverInstagramToTelegram"
    assert instagram_delivery["responses"]["207"]["content"]["application/json"]["schema"] == {
        "$ref": "#/components/schemas/TelegramMultiDeliveryResponse"
    }
    download = schema["paths"]["/v1/assets/{token}"]["get"]
    download_content = download["responses"]["200"]["content"]
    assert "application/json" not in download_content
    assert download_content["video/mp4"]["schema"] == {
        "type": "string",
        "format": "binary",
    }
    assert all(
        operation.get("description")
        for methods in schema["paths"].values()
        for operation in methods.values()
    )


@pytest.mark.asyncio
async def test_tiktok_resolution_endpoint_returns_full_url_without_extraction(settings) -> None:
    app = create_app(settings)
    response_model = TikTokResolutionResponse(
        source_id="1234567890123456789",
        source_url="https://www.tiktok.com/t/EXAMPLE/",
        resolved_url="https://www.tiktok.com/@creator/video/1234567890123456789",
    )
    async with app.router.lifespan_context(app):
        resolver = AsyncMock(return_value=response_model)
        app.state.tiktok.resolve_url = resolver
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                "/v1/tiktok/resolutions",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json={"url": "https://www.tiktok.com/t/EXAMPLE/"},
            )

    assert response.status_code == 200
    assert response.json() == response_model.model_dump(mode="json")
    resolver.assert_awaited_once_with("https://www.tiktok.com/t/EXAMPLE/")


def test_exported_openapi_schema_is_current(settings) -> None:
    schema_path = Path(__file__).parents[2] / "openapi.json"
    exported = json.loads(schema_path.read_text(encoding="utf-8"))

    assert exported == build_openapi_schema(settings)


@pytest.mark.asyncio
async def test_telegram_delivery_requires_configuration(settings) -> None:
    unconfigured = settings.model_copy(update={"telegram_bot_token": SecretStr("")})
    app = create_app(unconfigured)
    async with app.router.lifespan_context(app):
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post(
                "/v1/tiktok/telegram-deliveries",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json={
                    "source": {"url": "https://www.tiktok.com/@creator/video/123"},
                    "delivery": "media",
                    "telegram": {"chat_id": 123},
                },
            )
            instagram_response = await client.post(
                "/v1/instagram/telegram-deliveries",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json={
                    "source": {"url": "https://www.instagram.com/p/ABC123/"},
                    "delivery": "media",
                    "telegram": {"chat_id": 123},
                },
            )

    assert response.status_code == 503
    assert response.json()["error"]["code"] == "service_not_configured"
    assert instagram_response.status_code == 503
    assert instagram_response.json()["error"]["code"] == "service_not_configured"


@pytest.mark.asyncio
async def test_telegram_responses_are_raw_for_one_call_and_wrapped_for_many(settings) -> None:
    app = create_app(settings)
    payload = {
        "source": {"url": "https://www.tiktok.com/@creator/video/123"},
        "delivery": "media",
        "telegram": {"chat_id": 123},
    }
    async with app.router.lifespan_context(app):
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            app.state.telegram_delivery = FakeTelegramDelivery(
                [
                    TelegramCallResponse(
                        "sendVideo",
                        200,
                        b'{"ok":true,"result":{"message_id":1}}',
                        "application/json",
                    )
                ]
            )
            single = await client.post(
                "/v1/tiktok/telegram-deliveries",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json=payload,
            )
            instagram_single = await client.post(
                "/v1/instagram/telegram-deliveries",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json={
                    "source": {"url": "https://www.instagram.com/p/ABC123/"},
                    "delivery": "media",
                    "telegram": {"chat_id": 123},
                },
            )
            app.state.telegram_delivery = FakeTelegramDelivery(
                [
                    TelegramCallResponse(
                        "sendMediaGroup", 200, b'{"ok":true,"result":[]}', "application/json"
                    ),
                    TelegramCallResponse(
                        "sendMediaGroup",
                        429,
                        b'{"ok":false,"error_code":429}',
                        "application/json",
                    ),
                ]
            )
            multiple = await client.post(
                "/v1/tiktok/telegram-deliveries",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
                json=payload,
            )

    assert single.status_code == 200
    assert single.json() == {"ok": True, "result": {"message_id": 1}}
    assert instagram_single.status_code == 200
    assert instagram_single.json() == {"ok": True, "result": {"message_id": 1}}
    assert multiple.status_code == 207
    assert multiple.json()["partial"] is True
    assert [item["status_code"] for item in multiple.json()["deliveries"]] == [200, 429]
