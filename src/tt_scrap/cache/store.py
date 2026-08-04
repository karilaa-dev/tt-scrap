"""TTL-only Redis storage for extraction responses and asset fetch contexts."""

from __future__ import annotations

import hashlib
import secrets
from typing import TypeVar

from cryptography.fernet import Fernet, InvalidToken
from pydantic import BaseModel
from redis.asyncio import Redis
from redis.exceptions import RedisError

from ..errors import AssetExpiredError, CacheUnavailableError
from ..models import AssetFetchContext

ModelT = TypeVar("ModelT", bound=BaseModel)


class CacheStore:
    def __init__(self, redis: Redis, encryption_key: str, ttl_seconds: int) -> None:
        self._redis = redis
        self._fernet = Fernet(encryption_key.encode())
        self.ttl_seconds = ttl_seconds

    @staticmethod
    def metadata_key(platform: str, identity: str) -> str:
        digest = hashlib.sha256(identity.encode()).hexdigest()
        return f"tt-scrap:v1:metadata:{platform}:{digest}"

    @staticmethod
    def _asset_key(token: str) -> str:
        return f"tt-scrap:v1:asset:{token}"

    async def ping(self) -> bool:
        try:
            return bool(await self._redis.ping())
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc

    async def get_model(self, key: str, model: type[ModelT]) -> ModelT | None:
        try:
            value = await self._redis.get(key)
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc
        if value is None:
            return None
        try:
            return model.model_validate_json(value)
        except ValueError:
            await self.delete(key)
            return None

    async def set_model(self, key: str, value: BaseModel) -> None:
        try:
            await self._redis.set(key, value.model_dump_json(), ex=self.ttl_seconds)
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc

    async def delete(self, key: str) -> None:
        try:
            await self._redis.delete(key)
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc

    async def store_asset(self, context: AssetFetchContext) -> str:
        token = secrets.token_urlsafe(32)
        encrypted = self._fernet.encrypt(context.model_dump_json().encode())
        try:
            await self._redis.set(self._asset_key(token), encrypted, ex=self.ttl_seconds)
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc
        return token

    async def get_asset(self, token: str) -> AssetFetchContext:
        try:
            encrypted = await self._redis.get(self._asset_key(token))
        except RedisError as exc:
            raise CacheUnavailableError("Redis is unavailable") from exc
        if encrypted is None:
            raise AssetExpiredError("Asset token was not found or has expired")
        try:
            payload = self._fernet.decrypt(encrypted)
            return AssetFetchContext.model_validate_json(payload)
        except (InvalidToken, ValueError) as exc:
            raise AssetExpiredError("Asset token is invalid") from exc

    async def close(self) -> None:
        await self._redis.aclose()
