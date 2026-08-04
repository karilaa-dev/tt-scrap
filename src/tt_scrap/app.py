"""FastAPI application factory and lifecycle."""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from uuid import uuid4

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from redis.asyncio import Redis
from starlette.responses import Response

from .api.routes import assets, health, instagram, tiktok
from .cache import CacheStore
from .config import Settings, get_settings
from .errors import ScraperError
from .logging import configure_logging, request_id_var
from .media import AssetDownloader
from .models import ErrorDetail, ErrorResponse
from .platforms.instagram import InstagramService
from .platforms.tiktok import TikTokService
from .proxy import ProxyManager

logger = logging.getLogger(__name__)


def _error_response(status: int, code: str, message: str, request_id: str) -> JSONResponse:
    payload = ErrorResponse(error=ErrorDetail(code=code, message=message, request_id=request_id))
    return JSONResponse(status_code=status, content=payload.model_dump(mode="json"))


def create_app(
    settings: Settings | None = None,
    *,
    redis_client: Redis | None = None,
) -> FastAPI:
    configured_settings = settings or get_settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        configure_logging(configured_settings.log_level)
        redis = redis_client or Redis.from_url(
            configured_settings.redis_url.get_secret_value(), decode_responses=False
        )
        cache = CacheStore(
            redis,
            configured_settings.cache_encryption_key.get_secret_value(),
            configured_settings.cache_ttl_seconds,
        )
        proxy_manager = ProxyManager(
            configured_settings.proxy_file,
            include_host=configured_settings.proxy_include_host,
        )
        app.state.settings = configured_settings
        app.state.cache = cache
        app.state.proxy_manager = proxy_manager
        app.state.asset_downloader = AssetDownloader(configured_settings, proxy_manager)
        app.state.tiktok = TikTokService(configured_settings, cache, proxy_manager)
        app.state.instagram = InstagramService(configured_settings, cache)
        await cache.ping()
        logger.info("tt-scrap started")
        try:
            yield
        finally:
            await app.state.instagram.close()
            await app.state.tiktok.close()
            await app.state.asset_downloader.close()
            await cache.close()
            logger.info("tt-scrap stopped")

    app = FastAPI(
        title="tt-scrap",
        description="Authenticated TikTok and Instagram extraction API",
        version=configured_settings.app_version,
        lifespan=lifespan,
    )

    @app.middleware("http")
    async def request_context(
        request: Request, call_next: Callable[[Request], Awaitable[Response]]
    ) -> Response:
        request_id = request.headers.get("X-Request-ID") or str(uuid4())
        request.state.request_id = request_id
        token = request_id_var.set(request_id)
        try:
            response = await call_next(request)
            response.headers["X-Request-ID"] = request_id
            return response
        finally:
            request_id_var.reset(token)

    @app.exception_handler(ScraperError)
    async def scraper_error_handler(request: Request, exc: ScraperError) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        logger.warning("Request failed with %s", exc.code)
        return _error_response(exc.status_code, exc.code, str(exc), request_id)

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        return _error_response(422, "validation_error", "Request validation failed", request_id)

    @app.exception_handler(Exception)
    async def unexpected_error_handler(request: Request, exc: Exception) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        logger.exception("Unhandled request error")
        return _error_response(500, "internal_error", "Internal server error", request_id)

    app.include_router(health.router)
    app.include_router(tiktok.router)
    app.include_router(instagram.router)
    app.include_router(assets.router)
    return app
