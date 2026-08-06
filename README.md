# tt-scrap

An authenticated FastAPI service that extracts TikTok and Instagram metadata,
serves media through opaque asset URLs, and delivers TikTok media directly through
a Telegram bot.

Supported content:

- TikTok videos, slideshows, covers, and music
- Direct Telegram video, photo-album/carousel, document, and audio delivery
- Instagram reels, images, and mixed carousels
- TikTok short links, cookies, sticky proxies, rotation, and staged retries

The extraction code is derived from `karilaa-dev/tt-bot` main commit
`9263c3dbf30240478bff7a1a861655850105d232` under CC BY-NC 4.0.

## Start locally

Requirements: Python 3.13, [uv](https://docs.astral.sh/uv/), and FFmpeg. FFmpeg is used
only to stream-copy separate TikTok video and audio tracks into one MP4; it does not
re-encode them. The Docker image installs FFmpeg automatically.

```bash
cp .env.example .env
# Set TT_SCRAP_API_KEY and RAPIDAPI_KEY in .env.
# Set TELEGRAM_BOT_TOKEN only when direct Telegram delivery is needed.
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
| `CACHE_TTL_SECONDS` | Lifetime of asset contexts and tokens; default 600 seconds |
| `TIKTOK_INFO_CACHE_TTL_SECONDS` | Absolute lifetime of extraction metadata; default 60 seconds |
| `CACHE_MAX_ENTRIES` | Maximum in-memory cache entries; default 10,000 |
| `IMAGE_CONVERSION_WORKERS` | Persistent image-conversion processes; default 4, capped by CPU count |
| `TELEGRAM_BOT_TOKEN` | Bot credential; an empty value disables direct delivery |
| `TELEGRAM_API_BASE_URL` | Telegram Bot API base URL, including custom/local servers |
| `TELEGRAM_UPLOAD_CONCURRENCY` | Maximum concurrent delivery pipelines; default 16 |
| `TELEGRAM_UPLOAD_TIMEOUT_SECONDS` | Per Telegram upload timeout; default 600 seconds |
| `MAX_VIDEO_DURATION` | Maximum duration in seconds; zero disables it |
| `MAX_ASSET_BYTES` | Maximum downloaded size; zero disables it |

The cache is bounded and exists only in the API process. It stores normalized
metadata and upstream fetch context, never media bytes or request history. TikTok
information entries use a non-sliding 60-second lifetime; asset contexts retain the
longer asset TTL. Concurrent requests for the same TikTok are coalesced. All entries
and asset tokens disappear on restart. Run exactly one Uvicorn worker; multiple
workers would not share tokens or extraction IDs.

Keep `.env`, `cookies.txt`, and `proxies.txt` private. Signed URLs, credentials,
cookies, and proxy passwords are redacted from logs.

## API

Every `/v1/` request needs:

```http
Authorization: Bearer <TT_SCRAP_API_KEY>
```

Health endpoints and API documentation at `/docs` are public.

### OpenAPI schema

The running service generates its OpenAPI 3.1 contract directly from the current
FastAPI routes and Pydantic models:

```bash
curl http://127.0.0.1:8000/openapi.json --output openapi.json
```

The repository also contains the generated `openapi.json` for client generation
without a running server. Regenerate it after API or model changes with:

```bash
uv run tt-scrap-openapi openapi.json
```

Do not edit the generated document manually. The test suite verifies that it is
identical to the schema produced by the current application.

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

### Deliver TikTok media to Telegram

```bash
curl http://127.0.0.1:8000/v1/tiktok/telegram-deliveries \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "source":{"url":"https://www.tiktok.com/@creator/video/123"},
    "delivery":"media",
    "telegram":{"chat_id":1289167515,"caption":"Optional video caption"}
  }'
```

`source` accepts a TikTok `url`, a recent `extraction_id`, or a `video_id` for
audio delivery. Delivery modes map to Telegram as follows:

| Mode | Video | Slideshow | Music |
| --- | --- | --- | --- |
| `media` | `sendVideo` | `sendPhoto` or `sendMediaGroup` | — |
| `document` | `sendDocument` | document `sendMediaGroup` | — |
| `audio` | — | — | `sendAudio` |

Media mode first selects the maximum pixel resolution TikTok exposes. Within that
resolution it uses TikTok's precomputed original-reference `MVMAF` score, avoiding
any media download or local quality analysis during selection. The known quality
tag, TikTok quality tier, and bitrate are fallbacks when `MVMAF` is unavailable. If
the selected video's audio is separate, both files are downloaded concurrently and
FFmpeg stream-copies them into MP4 without re-encoding. This same muxing path is
used by direct Telegram delivery and `/v1/assets` downloads. Video and audio
thumbnails are normalized to Telegram-compliant JPEGs.

### Deliver Instagram media to Telegram

```bash
curl http://127.0.0.1:8000/v1/instagram/telegram-deliveries \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "source":{"url":"https://www.instagram.com/p/SHORTCODE/"},
    "delivery":"media",
    "telegram":{"chat_id":1289167515,"caption":"Optional caption"}
  }'
```

Instagram delivery accepts a URL or a cached `extraction_id`. Media mode uses
`sendPhoto`, `sendVideo`, or mixed photo/video `sendMediaGroup` calls while retaining
carousel order. Document mode sends original/file media and preserves source image
bytes. Unsupported photos and video thumbnails use the same asynchronous conversion
pipeline as TikTok. Carousel captions are placed on the first item of the first
album batch.

Slideshow photo mode passes static JPEG, PNG, and WebP through unchanged. Other
decodable formats are converted concurrently to baseline JPEG in persistent image
worker processes. A gallery is prepared completely before its first album is sent;
albums are then sent sequentially in groups of 2–10 while preserving order.
Document mode preserves original media bytes and skips photo/thumbnail conversion.

To compare JPEG and PNG conversion cost for a representative HEIC/HEIF input
without putting benchmarking work in the request path:

```bash
uv run python scripts/benchmark_image_conversion.py sample.heic
```

For a single Telegram call, the endpoint returns Telegram's raw response and HTTP
status. A multi-album delivery returns an ordered `deliveries` list. If an earlier
album succeeds and a later one fails, delivery stops and the endpoint returns HTTP
207. Ambiguous uploads are not retried, which prevents duplicate Telegram messages.

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
