from __future__ import annotations

import asyncio

import pytest

from tt_scrap.cache import CacheStore
from tt_scrap.errors import AssetExpiredError
from tt_scrap.models import AssetFetchContext


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
