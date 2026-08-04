# tt-scrap

`tt-scrap` is a standalone authenticated REST API for extracting TikTok and
Instagram metadata and media without exposing upstream CDN URLs. It supports
TikTok videos, photo slideshows, cover art, and music, plus Instagram videos,
images, and mixed carousels.

The extraction implementation is derived from `karilaa-dev/tt-bot` main commit
`9263c3dbf30240478bff7a1a861655850105d232`. The incomplete
`extract-tiktok-media-scraper` prototype was used as reference only.

## Architecture

1. A platform extraction endpoint validates the source URL and extracts
   normalized metadata.
2. TikTok uses a sticky proxy across URL resolution and yt-dlp extraction,
   rotating only when a retryable step fails. Chrome 120 TLS impersonation and
   a matching user agent are preserved from `tt-bot`.
3. Upstream URLs, cookies, referer data, and the proxy slot are encrypted into a
   dedicated Redis instance with a short TTL. Public responses contain only
   opaque `tt-scrap` asset URLs.
4. An authenticated asset request downloads the upstream file into an ephemeral
   spool. The service verifies length, computes SHA-256, retries safely, and only
   then streams the completed file to the caller.
5. Media bytes and request history are never persisted. Redis persistence is
   disabled and no Redis volume is used.

The default profile allows 32 concurrent metadata extractions and 64 asset
downloads. Run one Uvicorn process per service instance and scale instances
against the shared ephemeral Redis service when needed.

## Local setup with uv

Requirements: Python 3.13 and `uv`. Install `redis-server` as well if the local
launcher should manage its own ephemeral Redis process:

```bash
# Debian/Ubuntu
sudo apt-get install redis-server

# macOS
brew install redis
```

Set up and start the complete service without Docker:

```bash
cp .env.example .env
# Fill in the API key, Fernet key, Redis password, and RapidAPI key.
# cookies.txt and proxies.txt are already the default ignored host paths.
uv sync --locked
uv run tt-scrap-local
```

`tt-scrap-local` binds the API to `127.0.0.1:8000`. If `REDIS_URL` is already
reachable, it uses that server. Otherwise, it starts a process-owned Redis on
the loopback port from `REDIS_URL` (6380 by default), with RDB and AOF disabled,
stores its password in a mode-0600 temporary configuration file, and removes
all Redis state when the API exits. Stop both cleanly with `Ctrl+C`.

To expose the API on another interface or use an independently managed Redis:

```bash
# Local Redis must already be running; the launcher will not create it.
uv run tt-scrap-local --no-start-redis --host 0.0.0.0 --port 8000

# Equivalent direct production entry point for a process supervisor/systemd.
uv run tt-scrap
```

For a production host, run a dedicated Redis/Valkey-compatible service with
persistence disabled, set its authenticated URL in `REDIS_URL`, and supervise
`uv run tt-scrap`. Place a TLS reverse proxy in front if the API is reachable
outside a private network.

When developing beside an existing `tt-bot` checkout, provision a new ignored
API/cache configuration and copy only its RapidAPI, cookie, and proxy inputs:

```bash
uv run python scripts/bootstrap_local_env.py /path/to/tt-bot
```

Generate local secrets with:

```bash
openssl rand -hex 32
uv run python -c "from cryptography.fernet import Fernet; print(Fernet.generate_key().decode())"
```

`YTDLP_COOKIES` points to a Netscape-format cookie file. Cookies are optional
for ordinary public posts but needed for some age-restricted TikTok posts.
`PROXY_FILE` contains one `http://`, `https://`, or `socks5://` proxy per line.
Credentials are URL-encoded internally and always redacted from logs.

## Optional Docker Compose

```bash
cp .env.example .env
# Place ignored cookies.txt and proxies.txt files beside docker-compose.yml.
docker compose up --build -d
curl http://127.0.0.1:8000/health/ready
```

Redis is private to the Compose network, password protected, and starts with
both RDB snapshots and AOF disabled. The API image runs as an unprivileged user.

## Authentication

Every `/v1/` route requires:

```http
Authorization: Bearer <TT_SCRAP_API_KEY>
```

`/health/live`, `/health/ready`, `/docs`, and `/openapi.json` are public. The
OpenAPI schema describes normalized responses only; raw TikTok and RapidAPI
payloads are intentionally unavailable.

## API

### TikTok video or slideshow

```bash
curl -sS http://127.0.0.1:8000/v1/tiktok/extractions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.tiktok.com/@creator/video/123","refresh":false}'
```

Example shape:

```json
{
  "extraction_id": "uuid",
  "platform": "tiktok",
  "source_id": "123",
  "source_url": "https://www.tiktok.com/@creator/video/123",
  "resolved_url": "https://www.tiktok.com/@creator/video/123",
  "content_type": "video",
  "cover": {
    "asset_id": "uuid",
    "kind": "cover",
    "position": 0,
    "download_url": "/v1/assets/opaque-cover-token",
    "filename": "123_cover.jpg",
    "content_type": null,
    "expires_at": "2026-08-04T12:10:00Z"
  },
  "width": 1080,
  "height": 1920,
  "duration_seconds": 15,
  "likes": 100,
  "views": 1000,
  "media": [{
    "asset_id": "uuid",
    "kind": "video",
    "position": 0,
    "download_url": "/v1/assets/opaque-video-token",
    "filename": "123.mp4",
    "content_type": "video/mp4",
    "expires_at": "2026-08-04T12:10:00Z"
  }],
  "music": {
    "title": "Sound",
    "author": "Creator",
    "duration_seconds": 15,
    "cover": null
  },
  "expires_at": "2026-08-04T12:10:00Z"
}
```

Set `refresh` to `true` to bypass a successful metadata cache entry. The old
asset tokens remain valid only until their original expiry.

### TikTok music

```bash
curl -sS http://127.0.0.1:8000/v1/tiktok/music \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"video_id":123}'
```

This performs a fresh metadata extraction when it is not already cached and
returns separate authenticated audio and cover assets.

```json
{
  "extraction_id": "uuid",
  "platform": "tiktok",
  "source_id": "123",
  "title": "Sound",
  "author": "Creator",
  "duration_seconds": 15,
  "cover": null,
  "audio": {
    "asset_id": "uuid",
    "kind": "audio",
    "position": 0,
    "download_url": "/v1/assets/opaque-audio-token",
    "filename": "123.mp3",
    "content_type": "audio/mpeg",
    "expires_at": "2026-08-04T12:10:00Z"
  },
  "expires_at": "2026-08-04T12:10:00Z"
}
```

### Instagram media

```bash
curl -sS http://127.0.0.1:8000/v1/instagram/extractions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.instagram.com/reel/SHORTCODE/","refresh":false}'
```

The response `content_type` is `video`, `image`, or `carousel`. `media` preserves
the RapidAPI order and each entry identifies whether it is an image or video,
its optional quality, download asset, and optional thumbnail asset.

```json
{
  "extraction_id": "uuid",
  "platform": "instagram",
  "source_url": "https://www.instagram.com/p/SHORTCODE/",
  "content_type": "image",
  "media": [{
    "position": 0,
    "media_type": "image",
    "quality": "1080p",
    "asset": {
      "asset_id": "uuid",
      "kind": "image",
      "position": 0,
      "download_url": "/v1/assets/opaque-image-token",
      "filename": "instagram_1.jpg",
      "content_type": null,
      "expires_at": "2026-08-04T12:10:00Z"
    },
    "thumbnail": null
  }],
  "expires_at": "2026-08-04T12:10:00Z"
}
```

### Download an asset

```bash
curl -fL http://127.0.0.1:8000/v1/assets/OPAQUE_TOKEN \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  --output media.bin
```

Successful asset responses include `Content-Length`, `Content-Type`,
`Content-Disposition`, `ETag`, and `X-Content-SHA256`. Tokens are reusable for
the configured TTL, but every request performs a new upstream download; media
bytes are not retained after the response completes.

## Errors

All service errors use one shape:

```json
{
  "error": {
    "code": "invalid_link",
    "message": "Only HTTPS TikTok URLs are accepted",
    "request_id": "uuid"
  }
}
```

The response and every log entry include the same request ID. Stable error
codes distinguish authentication, validation, deleted/private content, rate
limits, region blocks, configured limits, cache failures, expired assets,
network failures, and timeouts.

## Retry and proxy behavior

TikTok has three independent configurable stages, each defaulting to three
attempts: short-link resolution, yt-dlp metadata extraction, and asset download.
The initial proxy remains sticky across stages and rotates on retry. Deleted,
private, and region-blocked results fail immediately. `PROXY_DATA_ONLY=true`
uses proxies for TikTok data extraction but downloads media directly.

Instagram keeps the current `tt-bot` behavior: RapidAPI and CDN calls are direct.
Not-found responses are permanent; network failures, 429s, and 5xx responses are
retried. All timeout, attempt, and delay values are configurable in `.env`.

## Development and tests

```bash
uv run ruff format --check src tests scripts
uv run ruff check src tests scripts
uv run mypy src/tt_scrap
uv run pytest -m "not live" --cov=tt_scrap
docker build -t tt-scrap:test .
```

The deterministic suite mocks third-party HTTP and yt-dlp data and exercises a
real-compatible async Redis API through an ephemeral fake. Live tests are kept
outside CI because posts disappear and RapidAPI calls may be billed.

With a server running, provide any currently valid fixture URLs and run:

```bash
export TT_SCRAP_API_KEY=...
export LIVE_TIKTOK_VIDEO_URL=...
export LIVE_TIKTOK_SHORT_URL=...
export LIVE_TIKTOK_SLIDESHOW_URL=...
export LIVE_TIKTOK_AGE_URL=...            # optional/best effort
export LIVE_INSTAGRAM_VIDEO_URL=...
export LIVE_INSTAGRAM_IMAGE_URL=...
export LIVE_INSTAGRAM_CAROUSEL_URL=...
uv run python scripts/live_smoke.py
```

The initial `v0.1.0` validation on 2026-08-04 used these public fixtures:

- TikTok video: `https://www.tiktok.com/@patroxofficial/video/6742501081818877190`
- TikTok short URL: `https://vm.tiktok.com/ZGd9EGcY2/`
- TikTok six-image slideshow: `https://www.tiktok.com/@discoverflagstaff/photo/7629036826480741663`
- Instagram reel: `https://www.instagram.com/reel/DbAqmKPIaY5/`
- Instagram image: `https://www.instagram.com/p/DZu8trmR89Y/`
- Instagram carousel: `https://www.instagram.com/p/DZ0eE3Yk8pG/`

Every returned video, image, cover, thumbnail, and audio asset passed MIME,
non-empty length, and SHA-256 verification through `/v1/assets`. These fixtures
are illustrative and can disappear. No stable, currently accessible
age-restricted TikTok post could be discovered for this release; cookie-file
loading, yt-dlp injection, per-asset cookie extraction, encryption, and log
redaction are covered deterministically.

Run a cached metadata concurrency check with:

```bash
uv run python scripts/benchmark.py 'https://www.tiktok.com/@creator/video/123'
```

## Later `tt-bot` integration

The bot should retain source URL detection and Telegram presentation logic. Its
TikTok and Instagram clients can become thin HTTP adapters that call the
platform-specific extraction endpoints, map stable error codes to localized
messages, and download each returned `tt-scrap` asset with the bearer token.
The bot should no longer import yt-dlp, curl-cffi, cookies, proxies, or RapidAPI
configuration after that migration.
