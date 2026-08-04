from __future__ import annotations

import asyncio

import fakeredis.aioredis
import pytest

from tt_scrap.cache import CacheStore
from tt_scrap.errors import AssetExpiredError
from tt_scrap.models import AssetFetchContext


@pytest.mark.asyncio
async def test_asset_context_is_encrypted_and_expires(settings) -> None:
    redis = fakeredis.aioredis.FakeRedis()
    cache = CacheStore(
        redis,
        settings.cache_encryption_key.get_secret_value(),
        ttl_seconds=1,
    )
    context = AssetFetchContext(
        platform="tiktok",
        upstream_url="https://secret.cdn.test/video?signature=private",
        filename="video.mp4",
        kind="video",
        cookies={"sessionid": "secret-cookie"},
        proxy_slot=2,
    )
    token = await cache.store_asset(context)
    raw = await redis.get(cache._asset_key(token))

    assert raw is not None
    assert b"secret.cdn.test" not in raw
    assert b"secret-cookie" not in raw
    assert await cache.get_asset(token) == context

    await asyncio.sleep(1.05)
    with pytest.raises(AssetExpiredError):
        await cache.get_asset(token)


@pytest.mark.asyncio
async def test_invalid_encrypted_asset_is_rejected(settings) -> None:
    redis = fakeredis.aioredis.FakeRedis()
    cache = CacheStore(redis, settings.cache_encryption_key.get_secret_value(), ttl_seconds=30)
    await redis.set(cache._asset_key("bad"), b"not-fernet", ex=30)
    with pytest.raises(AssetExpiredError):
        await cache.get_asset("bad")
