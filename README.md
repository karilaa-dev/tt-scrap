# tt-scrap

Native Go service for TikTok app extraction, Instagram extraction, authenticated media downloads, and Telegram delivery. Existing extraction and delivery contracts are preserved; the [OpenAPI document](openapi.json) also includes identity preparation.

Production runs one Go binary. TikTok registration and signing run in Go; image conversion uses Go decoders and native libheif. TikTok app videos include their audio; MP4 validation runs in Go.

## Run

Install Go **1.27.3**, a C compiler, `pkg-config`, and `libheif-dev`. Debian/Ubuntu installations that split HEIF codecs into plugins also need `libheif-plugin-libde265` for HEIC and `libheif-plugin-dav1d` for AVIF. On Debian 13 or recent Ubuntu:

```sh
sudo apt-get install build-essential pkg-config libheif-dev libheif-plugin-libde265 libheif-plugin-dav1d
cp .env.example .env
# Edit .env before starting.
go mod download
make build
./bin/tt-scrap
```

Configure `TT_SCRAP_API_KEY` with at least 16 characters. TikTok uses the Android app method and requires `DB_URL` for its identity pool. Instagram defaults to the Android app method: set `INSTAGRAM_ACCOUNTS_FILE` to a private account manifest, or `INSTAGRAM_ACCOUNTS_DIR` to saved session profiles. See [Instagram setup](docs/instagram-app.md). `RAPIDAPI_KEY` is only needed for the legacy method or its optional fallback. Existing environment variables take precedence over `.env`.

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

## Instagram accounts and downloads

Follow the [account-import guide](docs/instagram-account-imports.md) to upload credentials, inspect results, and manage active/reserve accounts.

Instagram supports database-backed account imports at `POST /v1/instagram/accounts/imports` (CSV upload or JSON credentials), plus saved Android session files. CSV `First Secret Question` supplies the authenticator TOTP secret. Imports check a least-assigned proxy, log in, verify the session and save it for immediate use and restart recovery. Multiple accounts may share an IP. New imports get a random Pixel or Samsung profile and generated device IDs, persisted with the account. Select a profile with JSON `device_profile` or CSV `Device Profile`; discover options at `GET /v1/instagram/device-profiles`. `INSTAGRAM_ACTIVE_ACCOUNTS=10` keeps the oldest ten healthy accounts active and balances requests across them; a dead account is persisted and replaced by the next reserve. Account/proxy routes stay fixed for metadata and downloads. Static/sticky proxies are needed to preserve the actual egress IP. `GET /v1/instagram/accounts` lists active, reserve and dead accounts without secrets. Native password + TOTP login and API import were verified with the development account; failed imports save no account. See [account import and database details](docs/instagram-app.md).

`scrap_instagram_attempts.legacy_used` records direct legacy and fallback attempts, including failures. App attempts set it to false. Cached extractions do not add attempt rows.

Call `POST /v1/instagram/extractions` for metadata, then download the returned `media[].asset.download_url` with the API bearer token. The response retains the previous ordered media structure and adds `method`, `resolved_url`, cover, dimensions, duration, likes, views, music, caption, comments, creation time, and download eligibility when available. Missing upstream values are `null`. Carousel children carry their own dimensions and duration. `source_id` remains the shortcode; the account and upstream URLs stay private.

Set `INSTAGRAM_METHOD=legacy` to use the previous RapidAPI method. With the default `INSTAGRAM_METHOD=app`, `INSTAGRAM_LEGACY_FALLBACK=true` retries a failed app extraction through RapidAPI; it requires `RAPIDAPI_KEY`. The response's `method` identifies the method that succeeded. Readiness checks the selected Instagram backend and the TikTok pool; the app method does not require a RapidAPI key. See [account configuration and fallback details](docs/instagram-app.md).

## Identity lifecycle

Video information retained by tt-bot stays in its database; tt-scrap caches API responses and temporary asset references in RAM. For TikTok, tt-scrap manages `scrap_proxies` and `scrap_identities` in PostgreSQL. The service loads them once into RAM, which then holds the authoritative identity state. Selection, balancing, cooldowns, and identity information reads happen in memory. PostgreSQL only preserves that state across restarts and crashes: a background writer saves each finished operation's result, combining results that arrive while a save is running. TikTok requests never wait for the database. Instagram imports, permanent account failures and fresh extraction-attempt history use synchronous durable writes.

TikTok requests start anonymously. For restricted videos, set `TIKTOK_SESSION_ID` to the value of your TikTok `sessionid` cookie, without the `sessionid=` prefix. The session comes only from the environment (including `.env`) and stays in memory; it is never saved in PostgreSQL. An age, login, private-content, or regional restriction triggers one retry with the account. The account must have access to the post. Rate limits, bot challenges, network failures, and deleted posts do not trigger account use. Account cookies stay separate from device sessions, so later public requests remain anonymous.

Restart after changing `TIKTOK_SESSION_ID`. Leaving it unset or empty disables account fallback. Cookie files and `YTDLP_COOKIES` are no longer used.

Each proxy address has ten identity slots. Before opening the API listener, startup registers missing slots; it does not fetch signing material. One worker per proxy runs in parallel with the other proxies; each address waits at least ten seconds after an attempt completes before starting the next one. Saved registered identities are reused, so a restart with a complete pool makes no TikTok requests. A slot retains its proxy permanently. Signing material is fetched on demand, immediately before a signed request such as video metadata or a play URL, when the identity has none or its material is at least one hour old. Explicit signing failures trigger renewal; invalid device registrations trigger recreation. Network errors and rate limits apply cooldowns rather than discarding valid identities. Preparation waits for identity and proxy cooldowns before retrying. Each attempt bounds cooldown waiting, lease acquisition, and registration by `TIKTOK_APP_TIMEOUT_SECONDS`; shutdown cancels these waits.

One active service process owns each database schema through a PostgreSQL advisory lock. This prevents two RAM pools from concurrently updating the same identities. The lock lasts as long as the service's database connection, so a failed or stalled save cancels only its statement: the service keeps the connection, keeps serving from RAM, logs the database error, and retries with backoff (one second, doubling to 30 seconds). The connection is checked every five seconds. Schema and privilege errors (SQLSTATE classes 42 and 3F), such as a missing table or a revoked grant, cannot heal by retrying: identity requests return 503 until a save succeeds, so no state accumulates that cannot be saved, while the service keeps its unsaved state, its ownership, and readiness, and logs every retry as an error. If the connection itself is lost, the server may release the lock to another process, so identity requests return 503 until the service reconnects and retakes ownership; operations already running finish, readiness stays up so a restart does not discard unsaved state, and the latest state of every changed identity is saved after reconnecting. A failed connection can remain open on the server and hold the lock until TCP keepalive notices; the service ends only that exact server process (matched by PID and start time). Each successful startup also records a new ownership token in `scrap_ownership`. After retaking the lock, the service verifies that this token is unchanged before accepting requests or saving checkpoints. If another process started during the outage, the old process stops even if that process has already released the lock. A conflicting lock holder, a changed ownership token, or an identity proxy-slot conflict stops identity work and exits for a supervised restart from saved state. On shutdown, once the request drain ends and running identity operations return, it spends at most ten seconds saving unsaved state. Those ten seconds include a save already in progress, cancelling and rolling back a stalled save, reconnecting, and closing the database connection; state still unsaved at the deadline is logged as an error. With nothing unsaved, it does not reconnect. Existing healthy saved devices continue working; new identities draw from the Pixel and Samsung catalog.

To prepare identities again after startup, call the authenticated endpoint (no body required):

```sh
curl -X POST http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
curl http://localhost:8000/v1/tiktok/identities/preparations \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

POST returns `202` for a running job or `200` when the pool is already prepared. GET returns `state`, `proxies`, `target`, `ready`, `failed`, and timestamps. Repeated POSTs share one job; polling reads RAM only. Failed slots receive up to three paced attempts per run; later calls or restarts reuse saved progress. A failed startup does not open the API port. Ten proxy addresses mean ten workers and 100 identities, even when their gateway hostname is shared.

## Throughput and memory

Optional covers download alongside the main media. Once media is ready to upload, a cover gets up to 50 ms of additional selection time, within `TELEGRAM_THUMBNAIL_WAIT_SECONDS` (default 1.5 seconds total). A slower cover is canceled and omitted; cancellation joins its worker before releasing the identity lease, so an uninterruptible decoder can still delay completion. Set the timeout to zero to disable covers. Converted source buffers close as soon as their replacement is ready, and acknowledged album batches release their buffers before the next upload. Memory and concurrency limits remain unchanged.

Upstream connections are pooled and reused. Matching metadata requests share work. Share links resolve from redirect headers alone: the first location that names a post ends resolution, and redirects to the TikTok home, profile, or login pages fail without loading them.

Telegram preparation skips the SHA-256 calculation used by raw asset responses. File-size, media-format, range identity and final-upload validation still run. Raw downloads retain their checksum headers.

Downloaded media stays in RAM when its length is known, it is at most `MEDIA_MEMORY_MAX_MB` (default 250), and the shared `MEDIA_MEMORY_BUDGET_MB` (default 1024) has room. Larger media, media of unknown length beyond that limit, and media arriving while the budget is spent use unlinked files in `TEMP_DIR`, which disappear even after a crash. When more than one part is allowed, the first media request asks for 1 MiB; if the server returns a byte range, the remaining parts download over up to `DOWNLOAD_PARALLEL_PARTS` connections (default four). Each media file holds one download slot, and extra part connections use only free slots, so a limit of one still serializes transfers. Parts are bound to the first response: each sends its strong `ETag` in `If-Range` and must return that `ETag` from the same post-redirect address. If the file changes, the download discards its buffer and restarts rather than mixing versions. A server that sends no strong `ETag` gets one complete request instead of parts. A dropped part resumes from its last received byte under the same check. Media retries use exponential delay plus up to 10% jitter within the download deadline.

A single video, audio file, or document uploads to Telegram while it downloads. The upload withholds its final 64 KiB until the download is validated and, for app media, the identity lease is released. A failed, invalid, or recovered transfer aborts its request before Telegram receives the complete body, so Telegram never processes it; recovery starts a new upload. Extraction width and height can differ from the served rendition, so a video's default `width` and `height` come from its complete downloaded file; a streamed upload starts before they are known and omits them unless the request supplies them. Photos and WebP documents need complete bytes for conversion to JPEG, and albums are fully prepared first, so both upload after preparation. App deliveries share one identity lease across video, thumbnail, and slideshow downloads, with one identity checkpoint per delivery. Identity waits do not occupy download slots. Native image conversions have a separate concurrency bound. Queues are bounded, and cancellation propagates through network calls and preparation.

App video selection uses only API metadata: highest pixel resolution, then frame rate, then bitrate. Extraction makes no CDN quality-probe requests. Download validation still inspects the actual downloaded file. Protocol clients and cookie jars stay in RAM across operations.

Default limits favor running work at once over queueing it: under load, requests share bandwidth and CPU and slow down rather than wait for a slot. `TELEGRAM_PIPELINE_CONCURRENCY`, `TELEGRAM_UPLOAD_CONCURRENCY`, `EXTRACTION_CONCURRENCY`, `TELEGRAM_PREPARATION_CONCURRENCY`, and `IMAGE_CONVERSION_WORKERS` default to 32, `DOWNLOAD_CONCURRENCY` to 64, and `HTTP_MAX_CONNECTIONS` to 128. Instagram app metadata capacity scales with loaded accounts: `INSTAGRAM_ACCOUNT_CONCURRENCY` (default 1) per account. Requests select the least busy available account, rotating ties. `INSTAGRAM_CONCURRENCY` (default 4) applies only to the legacy provider. Lower the others for smaller hosts or fewer proxies. `TELEGRAM_PIPELINE_CONCURRENCY` bounds whole deliveries and `TELEGRAM_UPLOAD_CONCURRENCY` bounds uploads; each delivery sends one Telegram request at a time, and a streamed upload holds its slot while its media downloads, so equal limits keep uploads from waiting. `TELEGRAM_PREPARATION_CONCURRENCY` bounds the media items one delivery downloads at once, with thumbnails alongside. `DOWNLOAD_PARALLEL_PARTS` is the number of connections per file, not a capacity limit; raising it sends more requests to the CDN without admitting more work. Requests wait only when a limit is full, and each limit accepts at most `MAX_QUEUED_REQUESTS` waiters before returning 503 `service_busy`. Buffered media can use up to `MEDIA_MEMORY_BUDGET_MB` of RAM beyond normal heap use. `IMAGE_CONVERSION_WORKERS` bounds images prepared at once, and `IMAGE_MEMORY_BUDGET_MB` (default 1024) bounds their decoded pixels: before decoding, each image reserves an estimate from its header dimensions, typically 8 to 20 bytes per pixel plus conversion canvases, and holds it until preparation ends. Many typical photos fit at once; a photo near the 100-megapixel limit waits for the whole budget and decodes alone. HEIC, HEIF, and AVIF images reserve from the primary image dimensions in their metadata, plus twice the file size for libheif's copies of the encoded image, before the file is read, so an image whose metadata lacks those dimensions is rejected; a file larger than half the budget also decodes alone. The estimate covers decoder output and image buffers, not allocator overhead, uncollected garbage, or libheif's internal working memory, so it is not a measure of RSS. Set `GOMEMLIMIT` above both budgets. `MAX_ASSET_BYTES` limits downloads. Request IDs, timings, status codes, and byte counts are logged as JSON. Telegram delivery logs also include the outcome, failed stage, planned/attempted/successful batch counts, and safe error codes. HTTP 200 rejections and partial deliveries produce warnings; HTTP 5xx failures produce errors. A caller that cancels is logged as a 499 `request_canceled` warning; requests still running when the shutdown drain ends are logged as 503 `service_shutting_down` errors. If a response body is cut off after its status was sent, the log reports that outcome and the sent status as `wire_status`. Payloads, captions, chat IDs, and bot tokens are excluded from these delivery fields.

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

The container runs as an unprivileged user and includes the Go binary and native HEIF codecs. `tt-scrap healthcheck` probes readiness. Shutdown drains requests for up to 30 seconds before cancelling remaining work. Compose allows 60 seconds for shutdown and rotates container logs at five 50 MB files.

`DB_URL` must be reachable from the container. A host-only `127.0.0.1` address refers to the container itself: join the database's Docker network and use its service hostname, or configure a host gateway. The base Compose file uses saved database proxies without a file mount. To import `proxies.txt`, use `docker compose -f docker-compose.yml -f compose.proxies.yml up --build -d`. For a new app database, provide that file or explicitly set `PROXY_INCLUDE_HOST=true`. Leave `PROXY_FILE` unset when using only saved database proxies. For TikTok account fallback, set `TIKTOK_SESSION_ID` in `.env`; Compose passes it to the service. Alternatively, mount Instagram seed session files read-only into the container and point `INSTAGRAM_ACCOUNTS_FILE` or `INSTAGRAM_ACCOUNTS_DIR` at the mounted path; local host paths are not available inside the container automatically.

`railpack.json` defines the CGO server build and native media packages. Railpack detects Go and its version from `go.mod` and supplies the default `./out` start command. The package settings follow the [Railpack Go](https://railpack.com/languages/golang/) and [configuration](https://railpack.com/config/file/) documentation. Docker provides the reproducible deployment path for the native codec dependencies.

For Dokploy with Railpack 0.15.4, `buildAptPackages` must contain only package names: `...` is passed literally to APT and causes dependency conflicts. The `...` in `deploy.aptPackages` is supported and preserves Railpack's default runtime packages.

The build uses structured `cmd` entries because Railpack 0.15.4 wraps string commands in single quotes without escaping embedded quotes. That breaks the linker flags and can report a successful build without producing `out`. The final build command checks that `out` is executable before creating the runtime image.

Railpack's build inputs explicitly include the Go sources and embedded OpenAPI files. Railpack 0.15.4 splits `.dockerignore` negations from exclusions, so the Docker allowlist's `**` exclusion otherwise removes nested sources such as `cmd/tt-scrap`. CI builds the Railpack image and checks its startup command as well as the Dockerfile image.

One process owns each database schema, so updates and rollbacks must stop the old task before starting the new one. [Dokploy settings](docs/dokploy.md) lists the Swarm update, rollback, stop grace, health check, and log retention settings for Dokploy.

## Migrate from the Python release

Read the [migration notes](docs/migration.md) before replacing an existing deployment. They cover the app default, PostgreSQL ownership, retained configuration names, and rollback.
