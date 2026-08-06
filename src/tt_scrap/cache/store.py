"""Bounded, process-local TTL storage for metadata and asset contexts."""

from __future__ import annotations

import asyncio
import hashlib
import heapq
import secrets
import time
from collections import OrderedDict
from dataclasses import dataclass
from itertools import count
from typing import TypeVar

from pydantic import BaseModel

from ..errors import AssetExpiredError
from ..models import AssetFetchContext

ModelT = TypeVar("ModelT", bound=BaseModel)


@dataclass(frozen=True, slots=True)
class _Entry:
    payload: bytes
    expires_at: float
    generation: int


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
        self._expirations: list[tuple[float, int, str]] = []
        self._generations = count()
        self._lock = asyncio.Lock()

    @staticmethod
    def metadata_key(platform: str, identity: str) -> str:
        digest = hashlib.sha256(identity.encode()).hexdigest()
        return f"tt-scrap:v1:metadata:{platform}:{digest}"

    @staticmethod
    def _asset_key(token: str) -> str:
        return f"tt-scrap:v1:asset:{token}"

    def _purge_expired(self, now: float) -> None:
        while self._expirations and self._expirations[0][0] <= now:
            _, generation, key = heapq.heappop(self._expirations)
            entry = self._entries.get(key)
            if entry is not None and entry.generation == generation:
                del self._entries[key]

    def _compact_expirations(self) -> None:
        if len(self._expirations) <= max(self.max_entries * 2, len(self._entries) * 4):
            return
        self._expirations = [
            (entry.expires_at, entry.generation, key) for key, entry in self._entries.items()
        ]
        heapq.heapify(self._expirations)

    async def _get_with_ttl(self, key: str) -> tuple[bytes | None, float | None]:
        async with self._lock:
            now = time.monotonic()
            self._purge_expired(now)
            entry = self._entries.get(key)
            if entry is None:
                return None, None
            if entry.expires_at <= now:
                del self._entries[key]
                return None, None
            return entry.payload, entry.expires_at - now

    async def _get(self, key: str) -> bytes | None:
        payload, _remaining_ttl = await self._get_with_ttl(key)
        return payload

    async def _set(self, key: str, payload: bytes, ttl_seconds: float | None = None) -> None:
        ttl = self.ttl_seconds if ttl_seconds is None else ttl_seconds
        if ttl <= 0:
            raise ValueError("ttl_seconds must be positive")
        async with self._lock:
            now = time.monotonic()
            self._purge_expired(now)
            self._entries.pop(key, None)
            while len(self._entries) >= self.max_entries:
                self._entries.popitem(last=False)
            generation = next(self._generations)
            expires_at = now + ttl
            self._entries[key] = _Entry(payload, expires_at, generation)
            heapq.heappush(self._expirations, (expires_at, generation, key))
            self._compact_expirations()

    async def get_model(self, key: str, model: type[ModelT]) -> ModelT | None:
        value = await self._get(key)
        if value is None:
            return None
        try:
            return model.model_validate_json(value)
        except ValueError:
            await self.delete(key)
            return None

    async def get_model_with_ttl(
        self, key: str, model: type[ModelT]
    ) -> tuple[ModelT | None, float | None]:
        value, remaining_ttl = await self._get_with_ttl(key)
        if value is None:
            return None, None
        try:
            return model.model_validate_json(value), remaining_ttl
        except ValueError:
            await self.delete(key)
            return None, None

    async def set_model(
        self, key: str, value: BaseModel, *, ttl_seconds: float | None = None
    ) -> None:
        await self._set(key, value.model_dump_json().encode(), ttl_seconds)

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
            self._expirations.clear()
