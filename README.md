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
| `LOG_LEVEL` | Log verbosity; `INFO` keeps request summaries and major milestones, `DEBUG` includes individual stages |
| `LOG_FORMAT` | `console` by default; use `json` for structured log collectors |
| `RAPIDAPI_KEY` | Instagram RapidAPI credential |
| `INSTAGRAM_CONCURRENCY` | Maximum concurrent Instagram provider calls; default 4 |
| `YTDLP_COOKIES` | Optional Netscape cookie file for restricted TikToks |
| `PROXY_FILE` | Optional file containing one proxy URL per line |
| `PROXY_DATA_ONLY` | Bypass proxies for media downloads when `true` |
| `CACHE_TTL_SECONDS` | Lifetime of asset contexts and tokens; default 600 seconds |
| `TIKTOK_INFO_CACHE_TTL_SECONDS` | Absolute lifetime of extraction metadata; default 60 seconds |
| `TIKTOK_RESOLUTION_CACHE_TTL_SECONDS` | Lifetime of successful URL-to-post-ID mappings; default 600 seconds |
| `URL_RESOLVE_TIMEOUT_SECONDS` | Total resolution budget, including duplicate/capacity waits, redirects, and retries; default 12 seconds |
| `URL_RESOLVE_POOL_TIMEOUT_SECONDS` | Wait for an HTTP connection before retiring a failed resolver pool; default 1 second |
| `CACHE_MAX_ENTRIES` | Maximum in-memory cache entries; default 10,000 |
| `IMAGE_CONVERSION_WORKERS` | Image process cap; default 0 uses available CPU cores minus one |
| `TELEGRAM_BOT_TOKEN` | Bot credential; an empty value disables direct delivery |
| `TELEGRAM_API_BASE_URL` | Telegram Bot API base URL, including custom/local servers |
| `TELEGRAM_PIPELINE_CONCURRENCY` | Maximum concurrent extraction/preparation pipelines; default 32 |
| `TELEGRAM_UPLOAD_CONCURRENCY` | Maximum concurrent Telegram API uploads; default 20 |
| `TELEGRAM_UPLOAD_TIMEOUT_SECONDS` | Per Telegram upload timeout; default 600 seconds |
| `TELEGRAM_UPLOAD_MAX_MB` | Per-file Telegram upload limit in MiB; default 50, zero disables it for a local Bot API server |
| `TELEGRAM_THUMBNAIL_WAIT_SECONDS` | Optional cover budget, overlapping media preparation; default 1.5 seconds, zero skips covers |
| `MAX_VIDEO_DURATION` | Maximum duration in seconds; zero disables it |
| `MAX_ASSET_BYTES` | Maximum downloaded size; zero disables it |

Set `TELEGRAM_UPLOAD_MAX_MB=100` to allow files up to 100 MiB when your custom
`TELEGRAM_API_BASE_URL` supports that size. Each MB here is 1,048,576 bytes,
preserving the existing default of 52,428,800 bytes. Restart the service after
changing it.

`IMAGE_CONVERSION_WORKERS=0` is automatic: the service uses the logical CPUs
available to its process minus one, with a minimum of one worker. A positive value
sets a lower operator cap but cannot exceed that CPU-derived limit.

The cache is bounded and exists only in the API process. It stores normalized
metadata and upstream fetch context, never media bytes or request history. TikTok
information entries use a non-sliding 60-second lifetime; resolved URL mappings use
600 seconds and asset contexts retain the asset TTL. Concurrent requests for the
same TikTok share successful or failed work. Failed results are discarded when the
waiting group drains, so later requests can retry. All entries
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

Resolve a short TikTok share link without extracting metadata or downloading media:

```bash
curl http://127.0.0.1:8000/v1/tiktok/resolutions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.tiktok.com/t/EXAMPLE/"}'
```

The response includes the numeric `source_id` and full `resolved_url`. A client can
use `source_id` to check its database before requesting extraction or delivery.

TikTok extraction responses also include nullable `creator_username`, sourced from
the extracted `author.uniqueId`. Instagram extraction responses include the post
`source_id` and nullable provider-supplied `creator_username`; neither service
derives a creator name from a placeholder URL.

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

Length-delimited, unmodified TikTok videos are relayed into Telegram while the
upstream response is still arriving. This overlaps the CDN download with the
Telegram upload and avoids an intermediate spool. Separate audio/video tracks,
unknown-length assets, and transformed media keep the verified download/remux
fallback. Cover preparation starts alongside CDN connection setup and media
downloads. Optional covers have a short deadline; Telegram generates the preview
when a cover is slower than that budget. A failed media download cancels its cover
work immediately.

Video/audio preparation enforces the destination's upload byte limit before reading
known oversized responses. Separate tracks share a combined budget, including when
their lengths are unknown, and the final remuxed output is checked again. Failed
attempts release their byte reservations before retrying. Direct asset downloads
retain their separate `MAX_ASSET_BYTES` policy. Photos can shrink during conversion,
so their final Telegram upload-size check remains authoritative.

Preparation pipelines and Telegram API uploads have independent concurrency limits,
so a slow upload does not stop later requests from extracting and downloading. All
upstream track transfers share `DOWNLOAD_CONCURRENCY`, including adaptive video and
audio pairs. Spools that roll to disk are written outside the event loop, and asset
responses use `DOWNLOAD_CHUNK_BYTES` to reduce per-chunk scheduling overhead.

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
bytes. HEIC/HEIF photos and usable video thumbnails use the same asynchronous
conversion pipeline as TikTok. Carousel captions are placed on the first item of
the first album batch.

Slideshow photo mode passes Telegram-compliant static JPEG, PNG, and WebP through
byte-for-byte. Oversized, extreme-aspect, or animated native photos are normalized;
HEIC/HEIF and other decodable source formats such as AVIF, GIF, TIFF, and BMP are
converted concurrently to baseline JPEG in persistent image worker processes.
Corrupt or unknown formats fail before the first Telegram call. A gallery is prepared
completely before its first album is sent; albums are then sent sequentially in groups
of 2–10 while preserving order. Document mode preserves original media bytes and
skips photo/thumbnail conversion.

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
`/v1/assets/opaque-token`; upstream CDN URLs are never returned. A single media
filename uses the Instagram post shortcode (for example, `DaJJCIVEn2n.mp4`), while
carousel items add a stable one-based suffix such as `DaJJCIVEn2n_1.jpg`.

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

## Diagnostics and timing

Every HTTP response includes `X-Request-ID` and a standard `Server-Timing` header,
for example `Server-Timing: app;dur=1842.317`. The duration measures server work up
to response streaming; it does not measure how long the caller takes to receive a
streamed asset.

At `LOG_LEVEL=INFO`, the service writes extraction, standalone URL resolution,
music extraction, and overall Telegram delivery milestones, followed by one
`http.request.completed` summary. Successful health checks appear only at `DEBUG`.
The summary includes the route, status, duration, request ID, and available platform,
source ID, delivery mode, and error details. Client errors and partial or rejected
Telegram deliveries use `WARNING`; server errors use `ERROR`. Telegram success is
checked against both its HTTP status and JSON `ok` value.

Recovery is summarized with `retry_count`, `fallback_count`, and
`thumbnail_skipped_count`; zero counts are omitted. Retries count extra attempts
that actually start. Fallbacks count interrupted relays retried through a verified
spool. Thumbnail skips count failures or timeouts, not absent or disabled covers.
Successful requests do not emit warnings for this recovery work.

Set `LOG_LEVEL=DEBUG` to inspect cache lookups, individual downloads, image
validation/conversion, remuxing, album preparation, Telegram uploads, and failed
attempts. Durations use `elapsed_ms`; bounded pools also report `queue_wait_ms`.
Transfer events report byte counts without logging media URLs, credentials,
cookies, captions, chat IDs, or request bodies.

`LOG_FORMAT=console` is the default and produces compact human-readable output for
terminal and Dokploy logs. Set `LOG_FORMAT=json` when logs are consumed by `jq` or a
structured log collector. Uvicorn startup/shutdown records use the same formatter.

With `LOG_FORMAT=json` and `LOG_LEVEL=DEBUG`, use the response's request ID to
inspect one complete operation:

```bash
docker compose logs --no-log-prefix tt-scrap \
  | jq 'select(.request_id == "PASTE-X-REQUEST-ID")
        | {event, elapsed_ms, queue_wait_ms, status_code, output_bytes}'
```

The largest `elapsed_ms` stage normally identifies the bottleneck. `queue_wait_ms`
measures application semaphore waiting. HTTPX pool waiting is separate and appears
as `PoolTimeout`. Resolver events include a credential-free `proxy_slot`; startup
records include HTTPX/httpcore versions and effective resolution limits. Retired
resolver pools report the number of users still draining. Metadata with no usable
media emits `tiktok.normalization.failed` with a bounded `failure_reason`.
Telegram failures include its safe error code, description, and `retry_after` value
when provided, but never log the bot-token URL or Telegram success payload.

## Retries and proxies

TikTok URL resolution, metadata extraction, and asset download each have their
own three-attempt retry loop. A proxy stays sticky during a request flow and
rotates after retryable failures. Permanent deleted, private, and region-blocked
results fail immediately.

Resolution uses a 12-second total budget and a 1-second HTTP pool wait by default.
Transport failures retire the affected client; existing users finish before it
closes and new requests use a fresh pool. Healthy clients continue to reuse their
connections. Pool exhaustion returns `service_busy` with HTTP 503, timeouts return
`upstream_timeout` with HTTP 504, and other transport failures return
`upstream_network_error` with HTTP 502. Invalid URLs/redirects and upstream 400/404/410
responses fail without repeated resolution attempts. Read-only callers should bound
their own retries across these responses; Telegram delivery retains its existing
rules against retrying ambiguous uploads.

An offline regression probe exercises the real HTTPX CONNECT/TLS path:

```bash
uv run python scripts/diagnostics/repro_tiktok_proxy_pool.py
```

It needs `openssl` for an ephemeral test certificate and only contacts a local
proxy. `--legacy` deliberately disables application recovery and reproduces the old
pool exhaustion failure. It exits nonzero as a diagnostic control.

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
