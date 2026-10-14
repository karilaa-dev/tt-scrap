# tt-scrap

Native Go service for TikTok app extraction, Instagram extraction, authenticated media downloads, and Telegram delivery. Existing extraction and delivery contracts are preserved; the [OpenAPI document](openapi.json) also includes identity preparation.

Production runs one Go binary. TikTok registration and signing run in Go; image conversion uses Go decoders and native libheif. TikTok app videos include their audio; MP4 validation runs in Go.

## Run

Install Go **1.27.2**, a C compiler, `pkg-config`, and `libheif-dev`. Debian/Ubuntu installations that split HEIF codecs into plugins also need `libheif-plugin-libde265` for HEIC and `libheif-plugin-dav1d` for AVIF. On Debian 13 or recent Ubuntu:

```sh
sudo apt-get install build-essential pkg-config libheif-dev libheif-plugin-libde265 libheif-plugin-dav1d
cp .env.example .env
# Edit .env before starting.
go mod download
make build
./bin/tt-scrap
```

Configure `TT_SCRAP_API_KEY` with at least 16 characters. TikTok uses the Android app method and requires `DB_URL` for its identity pool. Set `RAPIDAPI_KEY` for Instagram and readiness. Existing environment variables take precedence over `.env`.

TikTok reads saved proxies from PostgreSQL. `PROXY_FILE` optionally bootstraps addresses, one HTTP, HTTPS, or SOCKS5 address per line. `PROXY_INCLUDE_HOST=true` explicitly adds a direct-connection slot. `PROXY_DATA_ONLY=true` sends CDN transfers directly while app registration, signing, and play API requests keep the assigned proxy. TikTok accepts an optional account session through `TIKTOK_SESSION_ID`.

## API

`/docs`, `/redoc`, `/openapi.json`, `/health/live`, and `/health/ready` are public. All `/v1/` routes require `Authorization: Bearer <TT_SCRAP_API_KEY>`.

| Method | Route | Purpose |
| --- | --- | --- |
| POST | `/v1/tiktok/resolutions` | Resolve a TikTok or `tt.site` share URL |
| POST | `/v1/tiktok/extractions` | Extract TikTok video or slideshow metadata |
| POST | `/v1/tiktok/music` | Extract music using a numeric video ID |
| POST | `/v1/tiktok/telegram-deliveries` | Prepare and send TikTok media to Telegram |
| POST | `/v1/instagram/extractions` | Extract Instagram media |
| POST | `/v1/instagram/telegram-deliveries` | Prepare and send Instagram media to Telegram |
| GET / POST | `/v1/tiktok/identities/preparations` | Read progress / start or reuse identity preparation |
| GET | `/v1/assets/{token}` | Download an opaque, expiring media reference |

Downloads preserve `Content-Length`, MIME type, SHA-256, ETag, and attachment headers. Errors keep the existing `error.code`, `error.message`, and `error.request_id` envelope. Large TikTok IDs remain exact. Telegram albums are fully prepared before any upload, split into groups of up to ten with a final group of at least two, and return HTTP 207 for partial delivery. Ambiguous Telegram requests are not automatically retried.

## Identity lifecycle

Video information retained by tt-bot stays in its database; tt-scrap caches API responses and temporary asset references in RAM. tt-scrap manages `scrap_proxies` and `scrap_identities` in PostgreSQL. The service loads them once into RAM. Selection, balancing, cooldowns, and identity information reads then happen in memory; operation results are written to PostgreSQL after the operation finishes.

TikTok requests start anonymously. For restricted videos, set `TIKTOK_SESSION_ID` to the value of your TikTok `sessionid` cookie, without the `sessionid=` prefix. The session comes only from the environment (including `.env`) and stays in memory; it is never saved in PostgreSQL. An age, login, private-content, or regional restriction triggers one retry with the account. The account must have access to the post. Rate limits, bot challenges, network failures, and deleted posts do not trigger account use. Account cookies stay separate from device sessions, so later public requests remain anonymous.

Restart after changing `TIKTOK_SESSION_ID`. Leaving it unset or empty disables account fallback. Cookie files and `YTDLP_COOKIES` are no longer used.

Each proxy address has ten identity slots. Before opening the API listener, startup registers missing slots and refreshes expired signing material. One worker per proxy runs in parallel with the other proxies; each address waits at least ten seconds after an attempt completes before starting the next one. Fresh saved identities are reused. A slot retains its proxy permanently and renews signing material on use after three hours. Explicit signing failures trigger renewal; invalid device registrations trigger recreation. Network errors and rate limits apply cooldowns rather than discarding valid identities. Preparation waits for identity and proxy cooldowns before retrying. Each attempt bounds cooldown waiting, lease acquisition, and registration/signing by `TIKTOK_APP_TIMEOUT_SECONDS`; shutdown cancels these waits.

One active service process owns each database schema through a PostgreSQL advisory lock. Losing that connection stops identity work until restart. This prevents two RAM pools from concurrently updating the same identities. Existing healthy saved devices continue working; new identities draw from the Pixel and Samsung catalog.

To prepare identities again after startup, call the authenticated endpoint (no body required):

```sh
curl -X POST http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
curl http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

POST returns `202` for a running job or `200` when the pool is already prepared. GET returns `state`, `proxies`, `target`, `ready`, `failed`, and timestamps. Repeated POSTs share one job; polling reads RAM only. Failed slots receive up to three paced attempts per run; later calls or restarts reuse saved progress. A failed startup does not open the API port. Ten proxy addresses mean ten workers and 100 identities, even when their gateway hostname is shared.

## Throughput and memory

Upstream connections are pooled and reused. Matching metadata requests share work. Share links resolve from redirect headers alone: the first location that names a post ends resolution, and redirects to the TikTok home, profile, or login pages fail without loading them.

Downloaded media stays in RAM when its length is known, it is at most `MEDIA_MEMORY_MAX_MB` (default 250), and the shared `MEDIA_MEMORY_BUDGET_MB` (default 1024) has room. Larger media, media of unknown length beyond that limit, and media arriving while the budget is spent use unlinked files in `TEMP_DIR`, which disappear even after a crash. When more than one part is allowed, the first media request asks for 1 MiB; if the server returns a byte range, the remaining parts download over up to `DOWNLOAD_PARALLEL_PARTS` connections (default four). Each media file holds one download slot, and extra part connections use only free slots, so a limit of one still serializes transfers. A dropped part resumes from its last received byte. Media retries use exponential delay plus up to 10% jitter within the download deadline.

A single video, audio file, or document uploads to Telegram while it downloads. The upload withholds its final 64 KiB until the download is validated and, for app media, the identity checkpoint is saved. A failed, invalid, or recovered transfer aborts its request before Telegram receives the complete body, so Telegram never processes it; recovery starts a new upload. Photos need complete bytes for conversion, and albums are fully prepared first, so both upload after preparation. App deliveries share one identity lease across video, thumbnail, and slideshow downloads, with one database checkpoint per delivery. Identity waits do not occupy download slots. Native image conversions have a separate concurrency bound. Queues are bounded, and cancellation propagates through network calls and preparation.

App video selection uses only API metadata: highest pixel resolution, then frame rate, then bitrate. Extraction makes no CDN quality-probe requests. Download validation still inspects the actual downloaded file. Protocol clients and cookie jars stay in RAM across operations.

Default limits favor running work at once over queueing it: under load, requests share bandwidth and CPU and slow down rather than wait for a slot. `TELEGRAM_PIPELINE_CONCURRENCY`, `TELEGRAM_UPLOAD_CONCURRENCY`, `EXTRACTION_CONCURRENCY`, `TELEGRAM_PREPARATION_CONCURRENCY`, and `IMAGE_CONVERSION_WORKERS` default to 32, `DOWNLOAD_CONCURRENCY` to 64, and `HTTP_MAX_CONNECTIONS` to 128. `INSTAGRAM_CONCURRENCY` defaults to 4 Instagram API calls at once. Lower the others for smaller hosts or fewer proxies. `TELEGRAM_PIPELINE_CONCURRENCY` bounds whole deliveries and `TELEGRAM_UPLOAD_CONCURRENCY` bounds uploads; each delivery sends one Telegram request at a time, and a streamed upload holds its slot while its media downloads, so equal limits keep uploads from waiting. `TELEGRAM_PREPARATION_CONCURRENCY` bounds the media items one delivery downloads at once, with thumbnails alongside. `DOWNLOAD_PARALLEL_PARTS` is the number of connections per file, not a capacity limit; raising it sends more requests to the CDN without admitting more work. Requests wait only when a limit is full, and each limit accepts at most `MAX_QUEUED_REQUESTS` waiters before returning 503 `service_busy`. Buffered media can use up to `MEDIA_MEMORY_BUDGET_MB` of RAM beyond normal heap use; set `GOMEMLIMIT` above that budget. `IMAGE_CONVERSION_WORKERS` controls simultaneous decoded images; each can hold a decoded photo of up to 100 megapixels. `MAX_ASSET_BYTES` limits downloads. Request IDs, timings, status codes, and byte counts are logged as JSON. Telegram delivery logs also include the outcome, failed stage, planned/attempted/successful batch counts, and safe error codes. HTTP 200 rejections and partial deliveries produce warnings; HTTP 5xx failures produce errors. Payloads, captions, chat IDs, and bot tokens are excluded from these delivery fields.

`CACHE_MAX_ENTRIES` is the total retained-entry budget: each of six response caches receives one twelfth, and asset references receive the remainder. The default 10,000 allows 5,002 asset references and 833 entries per response cache. The independent caches keep separate locks; unused capacity in one partition is not borrowed by another. Active metadata requests retain their separate queue limit. Reused music responses expire no later than their original asset URLs.

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

The container runs as an unprivileged user and includes the Go binary and native HEIF codecs. `tt-scrap healthcheck` probes readiness. Shutdown drains requests for up to 30 seconds before cancelling remaining work.

`DB_URL` must be reachable from the container. A host-only `127.0.0.1` address refers to the container itself: join the database's Docker network and use its service hostname, or configure a host gateway. The base Compose file uses saved database proxies without a file mount. To import `proxies.txt`, use `docker compose -f docker-compose.yml -f compose.proxies.yml up --build -d`. For a new app database, provide that file or explicitly set `PROXY_INCLUDE_HOST=true`. Leave `PROXY_FILE` unset when using only saved database proxies. For account fallback, set `TIKTOK_SESSION_ID` in `.env`; Compose passes it to the service.

`railpack.json` defines the CGO server build and native media packages. Railpack detects Go and its version from `go.mod` and supplies the default `./out` start command. The package settings follow the [Railpack Go](https://railpack.com/languages/golang/) and [configuration](https://railpack.com/config/file/) documentation. Docker provides the reproducible deployment path for the native codec dependencies.

## Migrate from the Python release

Read the [migration notes](docs/migration.md) before replacing an existing deployment. They cover the app default, PostgreSQL ownership, retained configuration names, and rollback.
