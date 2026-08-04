# tt-scrap

An authenticated FastAPI service that extracts TikTok and Instagram metadata
and serves their media through opaque, short-lived asset URLs.

Supported content:

- TikTok videos, slideshows, covers, and music
- Instagram reels, images, and mixed carousels
- TikTok short links, cookies, sticky proxies, rotation, and staged retries

The extraction code is derived from `karilaa-dev/tt-bot` main commit
`9263c3dbf30240478bff7a1a861655850105d232` under CC BY-NC 4.0.

## Start locally

Requirements: Python 3.13 and [uv](https://docs.astral.sh/uv/).

```bash
cp .env.example .env
# Set TT_SCRAP_API_KEY and RAPIDAPI_KEY in .env.
uv sync --locked
uv run tt-scrap
```

The API listens on `http://127.0.0.1:8000`. Check it with:

```bash
curl http://127.0.0.1:8000/health/ready
```

To prepare `.env`, cookies, and proxies from a nearby `tt-bot` checkout:

```bash
uv run python scripts/bootstrap_local_env.py /path/to/tt-bot
```

## Configuration

The complete list and defaults are in `.env.example`. The important settings
are:

| Variable | Purpose |
| --- | --- |
| `TT_SCRAP_API_KEY` | Bearer token required by every `/v1/` endpoint |
| `RAPIDAPI_KEY` | Instagram RapidAPI credential |
| `YTDLP_COOKIES` | Optional Netscape cookie file for restricted TikToks |
| `PROXY_FILE` | Optional file containing one proxy URL per line |
| `PROXY_DATA_ONLY` | Bypass proxies for media downloads when `true` |
| `CACHE_TTL_SECONDS` | Lifetime of metadata and asset tokens; default 600 |
| `CACHE_MAX_ENTRIES` | Maximum in-memory cache entries; default 10,000 |
| `MAX_VIDEO_DURATION` | Maximum duration in seconds; zero disables it |
| `MAX_ASSET_BYTES` | Maximum downloaded size; zero disables it |

The cache is bounded and exists only in the API process. It stores normalized
metadata and upstream fetch context, never media bytes or request history. All
entries and asset tokens disappear on restart. Run exactly one Uvicorn worker;
multiple workers would not share tokens.

Keep `.env`, `cookies.txt`, and `proxies.txt` private. Signed URLs, credentials,
cookies, and proxy passwords are redacted from logs.

## API

Every `/v1/` request needs:

```http
Authorization: Bearer <TT_SCRAP_API_KEY>
```

Health endpoints and API documentation at `/docs` are public.

### Extract TikTok media

```bash
curl http://127.0.0.1:8000/v1/tiktok/extractions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.tiktok.com/@creator/video/123","refresh":false}'
```

### Extract TikTok music

```bash
curl http://127.0.0.1:8000/v1/tiktok/music \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"video_id":123}'
```

### Extract Instagram media

```bash
curl http://127.0.0.1:8000/v1/instagram/extractions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.instagram.com/reel/SHORTCODE/","refresh":false}'
```

Extraction responses contain normalized metadata and asset paths such as
`/v1/assets/opaque-token`; upstream CDN URLs are never returned.

### Download an asset

```bash
curl -fL http://127.0.0.1:8000/v1/assets/OPAQUE_TOKEN \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  --output media.bin
```

The service downloads into an ephemeral spool, validates the result, and then
responds with content length, MIME type, filename, ETag, and SHA-256 headers.
Media bytes are discarded after the response.

Errors use a stable envelope:

```json
{
  "error": {
    "code": "invalid_link",
    "message": "Only HTTPS TikTok URLs are accepted",
    "request_id": "uuid"
  }
}
```

## Retries and proxies

TikTok URL resolution, metadata extraction, and asset download each have their
own three-attempt retry loop. A proxy stays sticky during a request flow and
rotates after retryable failures. Permanent deleted, private, and region-blocked
results fail immediately.

Instagram RapidAPI requests and CDN downloads are direct by default. Network
errors, rate limits, and server failures are retried; definitive missing/private
responses are not.

## Tests

```bash
uv run ruff format --check src tests scripts
uv run ruff check src tests scripts
uv run mypy src/tt_scrap
uv run pytest -m "not live" --cov=tt_scrap
```

Live tests are opt-in because posts can disappear and RapidAPI calls may be
billed. Set the `LIVE_TIKTOK_*` and `LIVE_INSTAGRAM_*` URLs documented by
`scripts/live_smoke.py`, start the API, then run:

```bash
uv run python scripts/live_smoke.py
```

## Optional Docker image

Docker is not required. If desired, the Compose file runs the same single API
process with no additional services:

```bash
docker compose up --build -d
```

Published images are available at `ghcr.io/karilaa-dev/tt-scrap`.
