from __future__ import annotations

import os

import pytest
from redis.asyncio import Redis

from tt_scrap.cache import CacheStore
from tt_scrap.models import AssetFetchContext


@pytest.mark.integration
@pytest.mark.asyncio
async def test_real_redis_encrypted_ttl_round_trip(settings) -> None:
    redis_url = os.getenv("TEST_REDIS_URL")
    if not redis_url:
        pytest.skip("TEST_REDIS_URL is not configured")
    redis = Redis.from_url(redis_url, decode_responses=False)
    await redis.flushdb()
    cache = CacheStore(
        redis,
        settings.cache_encryption_key.get_secret_value(),
        ttl_seconds=30,
    )
    context = AssetFetchContext(
        platform="instagram",
        upstream_url="https://cdn.test/private",
        filename="asset.jpg",
        kind="image",
        cookies={"secret": "value"},
    )
    try:
        assert await cache.ping()
        token = await cache.store_asset(context)
        key = cache._asset_key(token)
        raw = await redis.get(key)
        assert raw is not None and b"cdn.test" not in raw and b"value" not in raw
        assert 0 < await redis.ttl(key) <= 30
        assert await cache.get_asset(token) == context
    finally:
        await redis.flushdb()
        await cache.close()
