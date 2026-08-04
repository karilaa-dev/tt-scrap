from __future__ import annotations

import hashlib
import io

import httpx
import pytest

from tt_scrap.app import create_app
from tt_scrap.media import DownloadedAsset
from tt_scrap.models import AssetFetchContext


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
        assert ready.status_code == 200
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
        transport = httpx.ASGITransport(app=app, raise_app_exceptions=False)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.get(
                "/v1/assets/missing",
                headers={"Authorization": "Bearer test-api-key-that-is-long-enough"},
            )
        assert response.status_code == 404
        assert response.json()["error"]["code"] == "asset_not_found_or_expired"


def test_openapi_has_expected_contract(settings) -> None:
    app = create_app(settings)
    schema = app.openapi()
    assert "/v1/tiktok/extractions" in schema["paths"]
    assert "/v1/tiktok/music" in schema["paths"]
    assert "/v1/instagram/extractions" in schema["paths"]
    assert "/v1/assets/{token}" in schema["paths"]
    assert "RawVideoResponse" not in schema["components"]["schemas"]
