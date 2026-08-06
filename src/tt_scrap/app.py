"""FastAPI application factory and lifecycle."""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from time import perf_counter
from uuid import uuid4

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from starlette.responses import Response

from .api.routes import assets, health, instagram, tiktok
from .cache import CacheStore
from .config import Settings, get_settings
from .errors import ScraperError
from .logging import configure_logging, elapsed_ms, log_event, request_id_var
from .media import AssetDownloader
from .media.images import ImagePreparationService
from .models import ErrorDetail, ErrorResponse
from .platforms.instagram import InstagramService
from .platforms.tiktok import TikTokService
from .proxy import ProxyManager
from .telegram import TelegramClient, TelegramDeliveryService

logger = logging.getLogger(__name__)

_API_DESCRIPTION = """
Authenticated extraction, download, and direct Telegram-delivery API for TikTok and
Instagram.

## Client integration

Use the origin that served this document as the API base URL. Every `/v1/*` request
requires `Authorization: Bearer <TT_SCRAP_API_KEY>`. `/openapi.json`, `/docs`, and
health endpoints are public. API errors use `ErrorResponse`; keep `error.request_id`
for diagnostics and branch on the stable `error.code` rather than message text.
Every response also includes `X-Request-ID` and `Server-Timing: app;dur=<milliseconds>`;
send a caller-generated `X-Request-ID` to correlate the response with server logs.

### TikTok information and downloads

Call `extractTikTok` with a public TikTok URL. The response contains metadata and
opaque `AssetDescriptor.download_url` paths instead of upstream CDN URLs. Resolve a
relative download path against the tt-scrap base URL, send the bearer token again,
and consume it before `expires_at`. TikTok metadata and `extraction_id` are cached
briefly (60 seconds by default); asset tokens normally live longer. A repeated URL
within the metadata TTL reuses extraction work unless `refresh=true`.

Video selection is server-side: highest pixel resolution first, then TikTok's
precomputed original-reference MVMAF score. When the selected adaptive stream has
separate audio, tt-scrap downloads both concurrently and stream-copies them into MP4
without re-encoding. This happens for both `downloadAsset` and Telegram delivery.

Call `extractInstagram` with a post or reel URL to receive ordered image/video items
and opaque asset paths. Its `extraction_id` can be passed directly to
`deliverInstagramToTelegram` while cached, avoiding another Instagram API request.

### Direct Telegram delivery

Call `deliverTikTokToTelegram` or `deliverInstagramToTelegram`; the Telegram bot token
is configured on tt-scrap and must not be sent by the client. TikTok `source` accepts
exactly one of `url`, a recent `extraction_id`, or `video_id`; `video_id` is valid only
with `delivery="audio"`. Instagram `source` accepts exactly one of `url` or a recent
`extraction_id`. `refresh=true` cannot be combined with `extraction_id`.

Delivery modes:

* `media`: video via `sendVideo`; one slideshow image via `sendPhoto`; multiple
  slideshow images via one or more `sendMediaGroup` calls.
* `document`: video via `sendDocument`; slideshow images as document media groups.
  Original image bytes are preserved and video/photo technical metadata is omitted.
* `audio`: TikTok music via `sendAudio`, with inferred title, performer, duration,
  filename, and a converted thumbnail when available.

Instagram supports `media` and `document`. A mixed carousel is sent in source order
as photo/video media groups; document mode sends every carousel item as a document.

`telegram.chat_id` is required. Other Telegram fields are optional overrides. The
client must not provide managed multipart fields: `video`, `audio`, `photo`,
`document`, `media`, `thumbnail`, or `cover`. Parameter validity depends on the
chosen media and delivery mode; unsupported combinations return
`telegram_parameter_not_supported`. TikTok slideshow delivery does not accept caption
fields. Instagram carousel captions are attached to the first item of the first
album batch.

For one Telegram API call, tt-scrap returns Telegram's JSON and HTTP status directly.
For several album batches, it returns `TelegramMultiDeliveryResponse` in order. HTTP
207 means at least one earlier batch succeeded before a later failure. Do not
automatically retry an ambiguous delivery timeout or an entire partial delivery,
because doing so can duplicate Telegram messages.

### Slideshows and images

TikTok slideshows and Instagram carousels are prepared fully before the first
Telegram call, retain their original order, and are partitioned into valid albums of
2-10 items. Supported photo formats pass through; unsupported formats are converted
asynchronously to baseline JPEG. Document mode preserves original image formats.
Video and audio covers are normalized to Telegram-compatible JPEG thumbnails.
""".strip()

_OPENAPI_TAGS = [
    {"name": "health", "description": "Unauthenticated process health checks."},
    {
        "name": "tiktok",
        "description": "TikTok metadata extraction, music extraction, and Telegram delivery.",
    },
    {
        "name": "instagram",
        "description": (
            "Instagram metadata extraction, opaque assets, and direct Telegram delivery."
        ),
    },
    {
        "name": "assets",
        "description": "Authenticated streaming download of temporary opaque assets.",
    },
]


def _error_response(status: int, code: str, message: str, request_id: str) -> JSONResponse:
    payload = ErrorResponse(error=ErrorDetail(code=code, message=message, request_id=request_id))
    return JSONResponse(status_code=status, content=payload.model_dump(mode="json"))


def create_app(settings: Settings | None = None) -> FastAPI:
    configured_settings = settings or get_settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        configure_logging(configured_settings.log_level)
        cache = CacheStore(
            configured_settings.cache_ttl_seconds,
            configured_settings.cache_max_entries,
        )
        proxy_manager = ProxyManager(
            configured_settings.proxy_file,
            include_host=configured_settings.proxy_include_host,
        )
        app.state.settings = configured_settings
        app.state.cache = cache
        app.state.proxy_manager = proxy_manager
        app.state.asset_downloader = AssetDownloader(configured_settings, proxy_manager)
        app.state.image_preparation = ImagePreparationService(configured_settings)
        app.state.tiktok = TikTokService(configured_settings, cache, proxy_manager)
        app.state.instagram = InstagramService(configured_settings, cache)
        app.state.telegram_client = TelegramClient(configured_settings)
        app.state.telegram_delivery = TelegramDeliveryService(
            configured_settings,
            cache,
            app.state.tiktok,
            app.state.asset_downloader,
            app.state.image_preparation,
            app.state.telegram_client,
            instagram=app.state.instagram,
        )
        logger.info("tt-scrap started")
        try:
            yield
        finally:
            await app.state.telegram_client.close()
            await app.state.image_preparation.close()
            await app.state.instagram.close()
            await app.state.tiktok.close()
            await app.state.asset_downloader.close()
            await cache.close()
            logger.info("tt-scrap stopped")

    app = FastAPI(
        title="tt-scrap",
        description=_API_DESCRIPTION,
        version=configured_settings.app_version,
        lifespan=lifespan,
        openapi_tags=_OPENAPI_TAGS,
    )

    @app.middleware("http")
    async def request_context(
        request: Request, call_next: Callable[[Request], Awaitable[Response]]
    ) -> Response:
        supplied_request_id = request.headers.get("X-Request-ID", "").strip()
        request_id = supplied_request_id[:128] if supplied_request_id else str(uuid4())
        request.state.request_id = request_id
        token = request_id_var.set(request_id)
        started_at = perf_counter()
        status_code = 500
        response_bytes: int | None = None
        try:
            response = await call_next(request)
            status_code = response.status_code
            elapsed = elapsed_ms(started_at)
            response.headers["X-Request-ID"] = request_id
            response.headers["Server-Timing"] = f"app;dur={elapsed:.3f}"
            content_length = response.headers.get("Content-Length")
            if content_length and content_length.isdigit():
                response_bytes = int(content_length)
            return response
        finally:
            route = request.scope.get("route")
            route_path = getattr(route, "path", None)
            operation = getattr(route, "name", None)
            if not isinstance(route_path, str):
                route_path = request.url.path
                if route_path.startswith("/v1/assets/"):
                    route_path = "/v1/assets/{token}"
                route_path = route_path[:256]
            log_event(
                logger,
                "http.request.completed",
                http_method=request.method,
                path=route_path,
                operation=operation if isinstance(operation, str) else None,
                status_code=status_code,
                response_bytes=response_bytes,
                elapsed_ms=elapsed_ms(started_at),
                success=status_code < 400,
            )
            request_id_var.reset(token)

    @app.exception_handler(ScraperError)
    async def scraper_error_handler(request: Request, exc: ScraperError) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        log_event(
            logger,
            "http.request.error",
            level=logging.WARNING,
            message="Request failed with a known application error",
            error_code=exc.code,
            error_type=type(exc).__name__,
            status_code=exc.status_code,
        )
        return _error_response(exc.status_code, exc.code, str(exc), request_id)

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        log_event(
            logger,
            "http.request.validation_error",
            level=logging.WARNING,
            message="Request validation failed",
            error_code="validation_error",
            error_type=type(exc).__name__,
            status_code=422,
        )
        return _error_response(422, "validation_error", "Request validation failed", request_id)

    @app.exception_handler(Exception)
    async def unexpected_error_handler(request: Request, exc: Exception) -> JSONResponse:
        request_id = getattr(request.state, "request_id", "-")
        logger.exception(
            "Unhandled request error",
            extra={
                "event": "http.request.unhandled_error",
                "error_code": "internal_error",
                "error_type": type(exc).__name__,
                "status_code": 500,
            },
        )
        return _error_response(500, "internal_error", "Internal server error", request_id)

    app.include_router(health.router)
    app.include_router(tiktok.router)
    app.include_router(instagram.router)
    app.include_router(assets.router)
    return app
