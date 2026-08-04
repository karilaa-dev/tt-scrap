"""Public asset descriptor creation."""

from __future__ import annotations

from datetime import datetime

from .cache import CacheStore
from .models import AssetDescriptor, AssetFetchContext


class AssetFactory:
    def __init__(self, cache: CacheStore) -> None:
        self._cache = cache

    async def create(
        self,
        context: AssetFetchContext,
        *,
        position: int,
        expires_at: datetime,
    ) -> AssetDescriptor:
        token = await self._cache.store_asset(context)
        return AssetDescriptor(
            asset_id=token,
            kind=context.kind,
            position=position,
            download_url=f"/v1/assets/{token}",
            filename=context.filename,
            content_type=context.declared_content_type,
            expires_at=expires_at,
        )
