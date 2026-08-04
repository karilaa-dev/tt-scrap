"""Bounded, process-local TTL storage for metadata and asset contexts."""

from __future__ import annotations

import asyncio
import hashlib
import secrets
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import TypeVar

from pydantic import BaseModel

from ..errors import AssetExpiredError
from ..models import AssetFetchContext

ModelT = TypeVar("ModelT", bound=BaseModel)


@dataclass(frozen=True, slots=True)
class _Entry:
    payload: bytes
    expires_at: float


class CacheStore:
    """A small FIFO cache shared by all requests in one Uvicorn process."""

    def __init__(self, ttl_seconds: int, max_entries: int) -> None:
        if ttl_seconds <= 0:
            raise ValueError("ttl_seconds must be positive")
        if max_entries <= 0:
            raise ValueError("max_entries must be positive")
        self.ttl_seconds = ttl_seconds
        self.max_entries = max_entries
        self._entries: OrderedDict[str, _Entry] = OrderedDict()
        self._lock = asyncio.Lock()

    @staticmethod
    def metadata_key(platform: str, identity: str) -> str:
        digest = hashlib.sha256(identity.encode()).hexdigest()
        return f"tt-scrap:v1:metadata:{platform}:{digest}"

    @staticmethod
    def _asset_key(token: str) -> str:
        return f"tt-scrap:v1:asset:{token}"

    def _purge_expired(self, now: float) -> None:
        while self._entries:
            key, entry = next(iter(self._entries.items()))
            if entry.expires_at > now:
                break
            del self._entries[key]

    async def _get(self, key: str) -> bytes | None:
        async with self._lock:
            now = time.monotonic()
            self._purge_expired(now)
            entry = self._entries.get(key)
            if entry is None:
                return None
            if entry.expires_at <= now:
                del self._entries[key]
                return None
            return entry.payload

    async def _set(self, key: str, payload: bytes) -> None:
        async with self._lock:
            now = time.monotonic()
            self._purge_expired(now)
            self._entries.pop(key, None)
            while len(self._entries) >= self.max_entries:
                self._entries.popitem(last=False)
            self._entries[key] = _Entry(payload, now + self.ttl_seconds)

    async def get_model(self, key: str, model: type[ModelT]) -> ModelT | None:
        value = await self._get(key)
        if value is None:
            return None
        try:
            return model.model_validate_json(value)
        except ValueError:
            await self.delete(key)
            return None

    async def set_model(self, key: str, value: BaseModel) -> None:
        await self._set(key, value.model_dump_json().encode())

    async def delete(self, key: str) -> None:
        async with self._lock:
            self._entries.pop(key, None)

    async def store_asset(self, context: AssetFetchContext) -> str:
        token = secrets.token_urlsafe(32)
        await self._set(self._asset_key(token), context.model_dump_json().encode())
        return token

    async def get_asset(self, token: str) -> AssetFetchContext:
        payload = await self._get(self._asset_key(token))
        if payload is None:
            raise AssetExpiredError("Asset token was not found or has expired")
        try:
            return AssetFetchContext.model_validate_json(payload)
        except ValueError as exc:
            raise AssetExpiredError("Asset token is invalid") from exc

    async def close(self) -> None:
        async with self._lock:
            self._entries.clear()
