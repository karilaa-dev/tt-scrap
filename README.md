# tt-scrap

Native Go service for TikTok app and web extraction, Instagram extraction, authenticated media downloads, and Telegram delivery. Existing extraction and delivery contracts are preserved; the [OpenAPI document](openapi.json) also includes identity preparation.

Production runs one Go binary. TikTok registration and signing run in Go; image conversion uses Go decoders and native libheif. FFmpeg handles video inspection and separate audio/video remuxing.

## Run

Install Go **1.27.2**, a C compiler, `pkg-config`, `libheif-dev`, and FFmpeg. Debian/Ubuntu installations that split HEIF codecs into plugins also need `libheif-plugin-libde265` for HEIC and `libheif-plugin-dav1d` for AVIF. On Debian 13 or recent Ubuntu:

```sh
sudo apt-get install build-essential pkg-config libheif-dev libheif-plugin-libde265 libheif-plugin-dav1d ffmpeg
cp .env.example .env
# Edit .env before starting.
go mod download
make build
./bin/tt-scrap
```

Configure `TT_SCRAP_API_KEY` with at least 16 characters. Set `TIKTOK_METHOD=app` for the Android app method or `TIKTOK_METHOD=web` for the web method. App mode requires `DB_URL`; web mode does not. Set `RAPIDAPI_KEY` for Instagram and readiness. Existing environment variables take precedence over `.env`.

App mode reads saved proxies from PostgreSQL. `PROXY_FILE` optionally bootstraps addresses, one HTTP, HTTPS, or SOCKS5 address per line. `PROXY_INCLUDE_HOST=true` explicitly adds a direct-connection slot. `PROXY_DATA_ONLY=true` sends CDN transfers directly while app registration, signing, and play API requests keep the assigned proxy. Web media retries try another configured proxy after a failure. Web mode accepts a Netscape-format cookie file through `YTDLP_COOKIES`, preserving the old environment name.

## API

`/docs`, `/redoc`, `/openapi.json`, `/health/live`, and `/health/ready` are public. All `/v1/` routes require `Authorization: Bearer <TT_SCRAP_API_KEY>`.

| Method | Route | Purpose |
| --- | --- | --- |
| POST | `/v1/tiktok/resolutions` | Resolve a TikTok share URL |
| POST | `/v1/tiktok/extractions` | Extract TikTok video or slideshow metadata |
| POST | `/v1/tiktok/music` | Extract music using a numeric video ID |
| POST | `/v1/tiktok/telegram-deliveries` | Prepare and send TikTok media to Telegram |
| POST | `/v1/instagram/extractions` | Extract Instagram media |
| POST | `/v1/instagram/telegram-deliveries` | Prepare and send Instagram media to Telegram |
| GET / POST | `/v1/tiktok/identities/preparations` | Read progress / start or reuse identity preparation |
| GET | `/v1/assets/{token}` | Download an opaque, expiring media reference |

Downloads preserve `Content-Length`, MIME type, SHA-256, ETag, and attachment headers. Errors keep the existing `error.code`, `error.message`, and `error.request_id` envelope. Large TikTok IDs remain exact. Telegram albums are fully prepared before any upload, split into groups of up to ten with a final group of at least two, and return HTTP 207 for partial delivery. Ambiguous Telegram requests are not automatically retried.

## Identity lifecycle

Only `scrap_proxies` and `scrap_identities` are managed. App mode loads them once into RAM. Selection, balancing, cooldowns, and identity information reads then happen in memory; operation results are written to PostgreSQL after the operation finishes.

Each proxy address has ten identity slots. Before opening the API listener, startup registers missing slots and refreshes expired signing material. One worker per proxy runs in parallel with the other proxies; preparation starts are spaced at least ten seconds apart on each address. Fresh saved identities are reused. A slot retains its proxy permanently and renews signing material on use after three hours. Explicit signing failures trigger renewal; invalid device registrations trigger recreation. Network errors and rate limits apply cooldowns rather than discarding valid identities.

One active app-mode process owns each database schema through a PostgreSQL advisory lock. Losing that connection stops identity work until restart. This prevents two RAM pools from concurrently updating the same identities. Existing healthy saved devices continue working; new identities draw from the Pixel and Samsung catalog.

To prepare identities again after startup, call the authenticated endpoint (no body required):

```sh
curl -X POST http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
curl http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

POST returns `202` for a running job or `200` when the pool is already prepared. GET returns `state`, `proxies`, `target`, `ready`, `failed`, and timestamps. Repeated POSTs share one job; polling reads RAM only. Failed slots receive up to three paced attempts per run; later calls or restarts reuse saved progress. A failed startup does not open the API port. Both operations return `503` in web mode. Ten proxy addresses mean ten workers and 100 identities, even when their gateway hostname is shared.


## Throughput and memory

Upstream connections are pooled and reused. Matching metadata requests share work. Downloads and Telegram multipart uploads stream through temporary files without buffering entire videos in RAM. Native image conversions have a separate concurrency bound. Queues are bounded, and cancellation propagates through network calls and preparation.

App video selection uses only API metadata: highest pixel resolution, then frame rate, then bitrate. Extraction makes no CDN quality-probe requests. Download validation still inspects the actual downloaded file. Protocol clients and cookie jars stay in RAM across operations. The [app optimization comparison](docs/go-app-optimization.md) describes the earlier build with probes; those measurements do not measure the current selection path.

Tune `EXTRACTION_CONCURRENCY`, `DOWNLOAD_CONCURRENCY`, `HTTP_MAX_CONNECTIONS`, `MAX_QUEUED_REQUESTS`, and Telegram concurrency limits for the number of proxies and available memory. `IMAGE_CONVERSION_WORKERS` controls simultaneous decoded images. `MAX_ASSET_BYTES` limits downloads; `GOMEMLIMIT` provides an optional Go heap budget. Request IDs, timings, status codes, and byte counts are logged as JSON.

The [18-minute Go/Python comparison](docs/go-python-comparison.md) measured substantially lower Go CPU and memory use, with close, network-dependent end-to-end latency. It covers metadata, downloads, both consecutively, and isolated API requests with signing excluded, at concurrency one and four. See [verification results](docs/go-verification.md) for race tests, real downloads and container checks.


## Verify

```sh
make check
```

See [Contributing](CONTRIBUTING.md) for the package layout, a disposable test database, and benchmark commands. Database tests use `TIKTOK_APP_TEST_DB_URL` or `DB_URL` and create isolated schemas; supply one to include those tests. HTTP tests use local upstream and Telegram fixtures and send no real messages. Tests cover API compatibility, lossless IDs, streamed checksums and lengths, registration/signing, identity concurrency, HEIC conversion, multipart uploads, album batching, cancellation, and cleanup.

## Docker and deployment

```sh
docker build -t tt-scrap:go .
docker compose up --build -d
```

The container runs as an unprivileged user and includes the Go binary, FFmpeg, and native HEIF codecs. `tt-scrap healthcheck` probes readiness. Shutdown drains requests for up to 30 seconds before cancelling remaining work.

`DB_URL` must be reachable from the container. A host-only `127.0.0.1` address refers to the container itself: join the database's Docker network and use its service hostname, or configure a host gateway. The base Compose file uses saved database proxies without a file mount. To import `proxies.txt`, use `docker compose -f docker-compose.yml -f compose.proxies.yml up --build -d`. For a new app database, provide that file or explicitly set `PROXY_INCLUDE_HOST=true`. Leave `PROXY_FILE` unset when using only saved database proxies. For web cookies, mount the file and point `YTDLP_COOKIES` to its container path.

`railpack.json` selects the Go provider and a CGO build with libheif. The package settings follow the [Railpack Go](https://railpack.com/languages/golang/) and [configuration](https://railpack.com/config/file/) documentation. Docker provides the reproducible deployment path for the native codec dependencies.

## Migrate from the Python release

Read the [migration notes](docs/migration.md) before replacing an existing deployment. They cover the app default, PostgreSQL ownership, retained configuration names, and rollback. Historical cross-runtime results remain in `docs`; runtime code and development tooling use Go.
