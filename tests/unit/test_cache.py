from __future__ import annotations

import asyncio

import pytest

from tt_scrap.cache import CacheStore
from tt_scrap.errors import AssetExpiredError
from tt_scrap.models import AssetFetchContext, LiveResponse


def context(name: str) -> AssetFetchContext:
    return AssetFetchContext(
        platform="tiktok",
        upstream_url=f"https://cdn.test/{name}",
        filename=f"{name}.mp4",
        kind="video",
    )


@pytest.mark.asyncio
async def test_asset_context_expires() -> None:
    cache = CacheStore(ttl_seconds=1, max_entries=10)
    asset = context("video")
    token = await cache.store_asset(asset)

    assert await cache.get_asset(token) == asset

    await asyncio.sleep(1.05)
    with pytest.raises(AssetExpiredError):
        await cache.get_asset(token)


@pytest.mark.asyncio
async def test_cache_is_bounded_and_evicts_oldest_entry() -> None:
    cache = CacheStore(ttl_seconds=30, max_entries=2)
    first = await cache.store_asset(context("first"))
    second = await cache.store_asset(context("second"))
    third = await cache.store_asset(context("third"))

    with pytest.raises(AssetExpiredError):
        await cache.get_asset(first)
    assert await cache.get_asset(second) == context("second")
    assert await cache.get_asset(third) == context("third")


@pytest.mark.asyncio
async def test_close_discards_all_entries() -> None:
    cache = CacheStore(ttl_seconds=30, max_entries=10)
    token = await cache.store_asset(context("video"))

    await cache.close()

    with pytest.raises(AssetExpiredError):
        await cache.get_asset(token)


@pytest.mark.asyncio
async def test_entries_can_have_independent_absolute_ttls() -> None:
    cache = CacheStore(ttl_seconds=30, max_entries=10)
    short_key = cache.metadata_key("test", "short")
    long_key = cache.metadata_key("test", "long")
    await cache.set_model(short_key, LiveResponse(), ttl_seconds=1)
    await cache.set_model(long_key, LiveResponse())

    await asyncio.sleep(1.05)

    assert await cache.get_model(short_key, LiveResponse) is None
    assert await cache.get_model(long_key, LiveResponse) == LiveResponse()


@pytest.mark.asyncio
async def test_repeated_refreshes_keep_expiration_index_bounded() -> None:
    cache = CacheStore(ttl_seconds=30, max_entries=2)
    key = cache.metadata_key("test", "refreshed")
    for _ in range(20):
        await cache.set_model(key, LiveResponse())

    assert len(cache._expirations) <= 4
