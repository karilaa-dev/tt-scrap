# Instagram Android sessions

Instagram defaults to `INSTAGRAM_METHOD=app`. The client implements the working session-to-media flow from `/home/agent/how-to-download/instagram`: convert a canonical post shortcode to its exact numeric media ID, request Android `media/{id}/info/`, select the best progressive video or image, and expose temporary download references. The server is still one Go process; the reference's Node package, a phone, and a signing service are not runtime dependencies.

Accounts can be imported through the authenticated API using browser cookies, a username/password/authenticator TOTP secret, or both. Cookies are preferred; saved login credentials allow recovery after session expiry. Password login uses the native Go Android CAA flow observed on the development Pixel 7: register a device signing identity, obtain account-access context and a nonce, encrypt the password with the server-issued RSA key, complete authenticator verification when requested, and verify the issued bearer session through the account's selected route before saving it. Python, instagrapi, ADB and the phone are not runtime dependencies.

Fresh password and TOTP login succeeded on 2026-10-29 using the development account. The live API import through a configured proxy completed in 17.32 seconds and saved the account in PostgreSQL. The account remained active after a service restart. Its persisted session and proxy then passed the live Reel/photo extraction and download tests (12,782,105 and 66,455 bytes). Go's default TLS handshake and a TLS-1.3-only variant both received an empty HTTP 429 at password submission; matching the Pixel's captured ClientHello allowed the same flow to complete. Login uses a dedicated HTTP/2 transport with TLS 1.3, AES-128-GCM, X25519MLKEM768 key exchange and the captured extension order. Certificate verification remains enabled. Metadata, CDN transfers and the final session check retain the normal transport.

New accounts generate their own device identity; existing-account updates and automatic recovery reuse the saved device and proxy. Password login obtains fresh server-issued context. Captured phone tokens, hardware signatures and account state are not replayed. The client reports unavailable Android keystore attestation explicitly; the tested flow succeeded without a phone-backed signer. Existing Android `Bearer IGT:2:…` sessions remain supported. Cookie imports persist the browser cookie JSON and derive Android authorization in memory from `sessionid` and `ds_user_id`. Interactive challenges are not implemented. Live password validation covers the development account and the first supplied CSV account; it does not establish that every account will succeed. Cookie-import verification is recorded in the [importing guide](instagram-account-imports.md#cookie-import-verification).

Protocol references: the user's Pixel 7 running Instagram 452.1.0.53.64 and [instagrapi's CAA implementation](https://github.com/subzeroid/instagrapi/blob/1265dff41ec43e08b5ccf3aea31c1b5185302fae/instagrapi/mixins/bloks.py). [uTLS](https://github.com/refraction-networking/utls) supplies ClientHello customization in Go. See [third-party notices](../THIRD_PARTY_NOTICES.md).


## Device profiles and generated identities

TLS is created locally in Go for every connection. Its random values, ephemeral key shares and session keys are new; the recorded handshake is never replayed. The shared TLS profile (`instagram-android-mns-452`) describes the Instagram Android networking stack. Selecting a different handset changes the reported model, Android version, display, hardware and user agent while keeping this working app protocol.

New imports select a random supported device profile once, like TikTok's identity creation. The initial catalog contains Pixel 7 / Android 15 and 17, Pixel 8 / Android 15, and Galaxy S23 / Android 14 and 15. Every account gets fresh device and family UUIDs and an Android ID, even when several accounts select the same model. The profile and IDs are saved in `scrap_instagram_accounts.device` (JSONB). Login, session verification and metadata use this identity; downloads use its user agent without account/device credentials. Neither requests nor restarts rotate profiles.

Use authenticated `GET /v1/instagram/device-profiles` to discover the catalog. JSON imports may select a profile per account:

```json
{"accounts":[{"username":"account-name","password":"password","totp_secret":"BASE32_SECRET","device_profile":"galaxy-s23-android-14"}]}
```

The optional CSV column `Device Profile` selects the same value. Omitted, blank or `random` uses random selection. Unsupported IDs return 422 `invalid_device_profile` before any account in the batch is submitted. `GET /v1/instagram/accounts` reports `device_profile`, but never returns generated device IDs or keys.

Existing sessions without saved device state retain the previous Pixel 7 user agent. Migration leaves their device column null and does not invent login IDs, re-login accounts or change their routes. Duplicate imports keep the original device. A private session JSON can include the saved `device` object alongside `authorization` and `proxy_url`; manifests inherit it from `session_file`.

Live validation on 2026-10-29: `galaxy-s23-android-14` completed fresh password + TOTP login through the configured proxy using generated identity data. The saved session then downloaded the Reel and photo fixtures successfully (12,782,105 and 66,455 bytes). The original Pixel 7 profile was verified earlier. The first supplied CSV account also imported successfully with `galaxy-s23-android-15` in 13.40 seconds after stripping the spreadsheet username prefix. Testing stopped at that first success; the remaining 14 rows were not attempted in that run. Other catalog combinations have fixture coverage but have not each been tested with a live login.

## Account import API

For CSV/JSON examples, provisioning steps, result handling, and troubleshooting, use the [account-import guide](instagram-account-imports.md).

`POST /v1/instagram/accounts/imports` requires the existing `Authorization: Bearer <TT_SCRAP_API_KEY>` header and accepts:

- `text/csv`: the CSV file as the entire request body.
- `text/plain`: browser cookie exports, with one JSON cookie array per account.
- `multipart/form-data`: one CSV or cookie-export file field named `file`.
- `application/json`: an `accounts` envelope containing login fields, `cookies`, or both; a single cookie array is also accepted.

The supplied CSV format maps `User ID / Email Address` to username, `Password` to password, and **`First Secret Question` to the TOTP secret**. `First Secret Answer` and unrelated columns are ignored. A leading spreadsheet apostrophe (`'`) is removed from usernames before duplicate detection and login, for both CSV and JSON imports. Passwords are preserved exactly, including punctuation and whitespace. Secrets may contain spaces or use an `otpauth://totp/` URI with SHA-1, six digits and a 30-second period. UTF-8 BOMs, CRLF and CSV quoting are supported. The whole batch is validated before login starts. Maximum upload size is 1 MiB; maximum batch size is 100 accounts. Supplied login credentials are saved privately in PostgreSQL for session recovery and are never returned by this API. Imports can combine cookie arrays with password/TOTP credentials; cookies are preferred. Expired sessions can recover on the saved device and proxy. See [cookie-first imports and recovery](instagram-account-imports.md).

```sh
curl --fail-with-body http://127.0.0.1:8000/v1/instagram/accounts/imports \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -F 'file=@/private/accounts.csv;type=text/csv'
```

Imports run synchronously, sequentially in submitted order. Concurrent imports return 409 `import_busy`. Successful rows are committed individually, so they survive a later row failure or client disconnect. Existing accounts match by cookie user ID or normalized username; unfamiliar login names are matched by verified user ID after login. Changed credentials return `updated` with the existing account ID; unchanged credentials return `duplicate`. A request-level failure can leave earlier rows committed; inspect the account list before retrying.

Cookie updates verify the replacement session on the saved proxy and device, retain omitted login credentials, and can reactivate dead accounts. Known-username password/TOTP updates save the complete backup without a password login or replacing the current session, and do not reactivate dead accounts. Include the TOTP secret again when replacing a password if the account uses authenticator 2FA. See [existing-account update examples](instagram-account-imports.md#4-update-an-existing-account).

For accounts not yet matched to a saved account, proxy candidates come from the existing configured/stored proxy pool. The least-assigned route is checked first with `https://api.ipify.org?format=json`; unusable routes are skipped before any credentials are submitted. Login, 2FA and authenticated `current_user` verification use the same route. New accounts save that proxy URL and the observed egress IP in PostgreSQL; known-account updates keep their saved route. Multiple accounts may share an IP. Use static/sticky proxies; an observed address does not prevent a proxy provider from rotating its egress later. Direct-host routing is available only when included in the proxy pool.

Each account gets at most one password submission and one TOTP submission. The login transport does not replay requests after refused streams, connection failures or redirects. A rate-limited login stops the rest of that batch. HTTP 200 contains per-row results; it does not mean every account succeeded:

```json
{"results":[{"row":1,"account_id":"uuid","status":"imported"},{"row":2,"status":"failed","error_code":"upstream_rate_limited"},{"row":3,"status":"skipped","error_code":"batch_rate_limited"}]}
```

`row` is one-based, excluding the CSV header; for cookie exports it counts arrays. Status is `imported`, `updated`, `duplicate`, `failed`, or `skipped`. A challenge, failed proxy check, rejected login or failed session verification does not create an account. Failed credential updates preserve the saved credentials. During extraction, expired or explicitly rejected sessions can recover using the saved password/TOTP on the same device and proxy. Successful recovery stores the replacement bearer session and clears rejected cookies. Network failures, rate limits and challenges do not trigger password fallback. See [automatic recovery](instagram-account-imports.md#automatic-recovery).

`GET /v1/instagram/accounts` returns `active_limit` and accounts in oldest-first order, with `id`, optional `username`, optional `egress_ip`, and `state` (`active`, `reserve`, or `dead`) and `device_profile`. Active accounts can temporarily be cooling down or busy. Neither endpoint exposes authorization tokens, passwords, TOTP seeds or proxy credentials. Account management remains available with no ready Instagram account, allowing an empty service to be provisioned.

## Database and legacy measurements

Startup creates `scrap_instagram_accounts` and `scrap_instagram_attempts` in `DB_SCHEMA`. The existing schema ownership lock still permits only one service instance. Account records contain `ordinal`, `username`, `user_id`, nullable `android_authorization`, `proxy_url`, `egress_ip`, `dead`, `dead_reason`, `dead_at`, `created_at`, and nullable JSONB columns `device`, `cookies`, and `login_credentials`. Cookie-authenticated accounts keep `android_authorization` NULL. Password/TOTP backups, session data, and proxy fields are sensitive; restrict access to the database and its backups. Older releases cannot load cookie-only rows.

**`scrap_instagram_attempts.legacy_used` is true for every legacy method invocation**, whether selected directly or reached through fallback, including failed legacy invocations. An app attempt has `legacy_used=false`. Fallback produces two rows: the failed app attempt and the legacy attempt. This is upstream method-attempt history, not per-caller or per-download history: cache hits and callers sharing in-flight work add no rows. Each row records `source_id`, `created_at`, `finished_at`, nullable `succeeded`, and safe `error_code`. A null outcome means an interrupted or unrecorded completion, not success. No upstream URLs, credentials, or response bodies are stored.

```sql
SELECT legacy_used, count(*) AS attempts,
       count(*) FILTER (WHERE succeeded) AS succeeded,
       count(*) FILTER (WHERE succeeded = false) AS failed,
       count(*) FILTER (WHERE succeeded IS NULL) AS unfinished,
       avg(extract(epoch FROM finished_at-created_at)*1000) AS average_ms
FROM scrap_instagram_attempts
GROUP BY legacy_used;
```

History currently has no automatic retention; archive or delete old attempt rows according to your retention policy. Fresh extraction attempts require a database write before contacting Instagram. A database failure returns 503 instead of silently losing telemetry or triggering legacy fallback. Cache hits and media transfers still use in-memory state.

## Load accounts and bind their routes

Keep account manifests and session files outside source control, readable only by the service user. Point `INSTAGRAM_ACCOUNTS_FILE` at a JSON manifest:

```json
{
  "accounts": [
    {
      "id": "account-a",
      "session_file": "sessions/account-a.json",
      "proxy_url": "http://proxy-a.example:8080"
    },
    {
      "id": "account-b",
      "session_file": "sessions/account-b.json",
      "proxy_url": "socks5h://proxy-b.example:1080"
    }
  ]
}
```

`session_file` resolves relative to the manifest. Each referenced file uses the reference package's format:

```json
{"authorization": "Bearer IGT:2:REPLACE_WITH_SAVED_ANDROID_SESSION"}
```

An account may supply `authorization` directly in the private manifest instead of `session_file`. Omit `proxy_url` to explicitly use the service host's outbound route. Accounts without a proxy share that host IP. HTTP, HTTPS, SOCKS5, and SOCKS5h proxy URLs are supported. Proxy authentication belongs in the private proxy URL and is never returned in extraction responses.

Alternatively, set `INSTAGRAM_ACCOUNTS_DIR` to a directory containing saved account JSON profiles, such as the reference package's `.state/accounts` directory. Profiles may include `id` and `proxy_url`; the filename supplies the default ID and an omitted proxy uses the host route. Both account sources may be combined; duplicate IDs or duplicate normalized authorization values are rejected. Use the manifest when existing session files need explicit proxy mappings without modifying those files. A proxy in the manifest overrides one in its referenced profile.

```dotenv
INSTAGRAM_METHOD=app
INSTAGRAM_ACCOUNTS_FILE=/run/secrets/instagram/accounts.json
INSTAGRAM_ACTIVE_ACCOUNTS=10
INSTAGRAM_ACCOUNT_CONCURRENCY=1
INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY=1
INSTAGRAM_APP_TIMEOUT_SECONDS=15
INSTAGRAM_LEGACY_FALLBACK=false
```

The account's configured route stays fixed for metadata, retries on that account, and all assets issued by that extraction. Different retries may select another available account together with its own route. There is no round-robin proxy rotation within an account. A static or sticky proxy endpoint is required to preserve the actual public IP: the server cannot stop a proxy provider from rotating egress behind the same URL. File accounts seed PostgreSQL at startup. Existing rows, proxy assignments and dead status are retained: editing or removing a seed file does not modify or delete a stored account. API imports become available immediately and survive restart without a file mount. Mount seed files read-only when running in a container.

## Scaling and failures

Metadata capacity is the number of active healthy accounts, capped by `INSTAGRAM_ACTIVE_ACCOUNTS` (default 10), multiplied by `INSTAGRAM_ACCOUNT_CONCURRENCY` (default one). The oldest non-dead accounts by database insertion ordinal fill the active set; newer accounts remain in reserve. A permanent authentication failure marks an account dead in PostgreSQL and immediately promotes the next oldest reserve. Temporary cooldowns and busy accounts do not promote reserves. Each account separately allows `INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY` concurrent CDN batches (also one by default). A slow download therefore does not reserve an authenticated metadata slot. The scheduler chooses the account with the fewest active metadata requests, then the fewest downloads, and rotates remaining ties, so successive requests spread across accounts. `MAX_QUEUED_REQUESTS` bounds waiting work; cancellation also cancels account waits. Matching cached extractions reuse work and do not consume an additional metadata request. For the app method, `/p/`, `/reel/`, `/reels/`, and `/tv/` URLs with the same shortcode share both their cache entry and an in-progress lookup. `refresh=true` refreshes the shared entry; response source URLs still reflect the caller’s URL.

`INSTAGRAM_MAX_ATTEMPTS` bounds metadata attempts. Account/session failures and rate limits affect the relevant account instead of changing its proxy. An invalid or challenged session is marked dead permanently. Duplicate imports do not revive dead accounts. Death writes are completed before replacement admission; if persistence fails, metadata admission pauses while retaining the pending update for the next request to retry. Shutdown also attempts to flush pending death writes. Temporary upstream failures and rate limits allow bounded retries on available accounts. HTTP 429 respects `Retry-After`; absent a usable value, the account cools down for one minute. Cooldowns are capped at 24 hours. Downloads use the account route recorded by extraction, with no Android bearer token or app headers sent to the CDN. A download batch shares one CDN slot on its pinned account across its media and parallel range requests. Metadata and CDN work share the bounded waiting queue but have independent active limits; neither rotates the account’s route. The existing global download, memory, and parallel-range limits still apply.

Malformed configured files fail startup without printing their contents. A server with no loaded app accounts cannot perform app extraction; configure accounts or the explicit legacy fallback. `/health/ready` checks the selected Instagram backend as well as TikTok readiness. A RapidAPI key is not required when the app backend is available.

## Extract information, then download

The API routes and existing media item structure are retained:

```sh
curl -X POST http://localhost:8000/v1/instagram/extractions \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://www.instagram.com/reel/SHORTCODE/"}'
```

The response includes these additional fields when supplied by Instagram:

| Field | Meaning |
| --- | --- |
| `method` | `app` or `legacy`, identifying the successful method |
| `resolved_url` | Normalized canonical Instagram URL |
| `cover` | Temporary cover asset descriptor |
| `width`, `height` | Selected rendition dimensions |
| `duration_seconds` | Video duration as an integer number of seconds |
| `likes`, `views`, `comments` | Available engagement counts |
| `music` | Title, author, duration, and any supplied cover asset; `audio` is always `null` |
| `caption` | Post caption |
| `created_at` | Post creation timestamp in RFC 3339 format |
| `third_party_download_eligible` | Instagram's reported download eligibility flag |

Unavailable fields are `null`; the legacy provider may lack most of these fields. Each `media[]` item also has its own `width`, `height`, and `duration_seconds`. `source_id` remains the post shortcode for compatibility. No account authorization, account ID, proxy address, or upstream CDN URL is exposed.

Fetch `media[].asset.download_url` against the same API origin with the API bearer token, before `expires_at`. Metadata extraction makes no CDN quality-probe requests. Videos select the highest pixel count, then bandwidth/type; photos select the largest image. Carousels retain source order and include both image and video children. Incomplete carousels fail extraction. Signed media links expire, so use `refresh=true` for fresh metadata when needed. Cached `extraction_id` values can still be passed to `/v1/instagram/telegram-deliveries`.

The app method accepts canonical `/p/`, `/reel/`, `/reels/`, and `/tv/` post URLs. It does not resolve opaque `/share/` links or fetch stories. Story URLs remain available through the legacy method when that provider supports them. The reported download eligibility flag is metadata; selected assets are the playback resources returned by Instagram.

## Legacy and fallback

To use the previous provider directly:

```dotenv
INSTAGRAM_METHOD=legacy
RAPIDAPI_KEY=REPLACE_WITH_PROVIDER_KEY
```

To try the new method first and use the previous provider after an app extraction failure:

```dotenv
INSTAGRAM_METHOD=app
INSTAGRAM_LEGACY_FALLBACK=true
RAPIDAPI_KEY=REPLACE_WITH_PROVIDER_KEY
```

Fallback is disabled by default and never runs after caller cancellation or a configured video-duration limit rejection. It applies to extraction, including unusable app metadata; it does not silently replace an asset during a later download. The `method` response field records the selected provider. `INSTAGRAM_CONCURRENCY` and `INSTAGRAM_REQUEST_TIMEOUT_SECONDS` retain their legacy meanings, while `INSTAGRAM_ACCOUNT_CONCURRENCY` and `INSTAGRAM_APP_TIMEOUT_SECONDS` govern the app method. `RAPIDAPI_HOST` and `RAPIDAPI_BASE_URL` continue to configure the legacy provider.

## Verification

`go test ./...` covers extraction, metadata compatibility, explicit legacy fallback, mixed-carousel ordering and transfers, account scheduling, queue limits, failure isolation, and credential-free account-pinned CDN requests. Race checks cover the changed packages.

For opt-in live metadata and download checks using private saved sessions:

```sh
TT_SCRAP_LIVE_INSTAGRAM=1 \
INSTAGRAM_ACCOUNTS_DIR=/path/to/private/accounts \
go test ./internal/platform -run '^TestLiveInstagramExtractionDownload$/(video|image)$' -count=1 -v
```

On 2026-10-28, both reference profiles loaded and the Reel and photo tests passed, downloading 12,782,105 and 66,455 bytes respectively. The historical mixed-carousel sample `DezfpMuEUZO` returned HTTP 400/unavailable through both sessions, so it could not be validated live. The fixture-based mixed-carousel extraction and download test passed. To validate an available live carousel, set `TT_SCRAP_INSTAGRAM_CAROUSEL_CODE` and run the `carousel` subtest. Live tests never print session tokens or signed CDN URLs.

## Performance checks (2026-10-28)

The optimization keeps metadata concurrency at one request per account by default. CDN batches now use a separate per-account limit, so metadata can proceed during an existing transfer. Pinned account selection uses an ID index, idle-account selection ends once the minimum load is found, and releases avoid allocating notification channels when no callers are waiting. Rendition selection uses one pass instead of sorting every candidate and parses only URLs that can improve the selected rendition.

Measurements on the development machine (Go 1.27.2, Intel i5-1240P):

| Check | Before | After |
| --- | ---: | ---: |
| Live alternate-URL lookup, median of three follow-ups | 258.8 ms | 0.9 ms |
| Upstream extractions for four URL aliases of one Reel | 4 | 1 |
| Metadata admission during a simulated 10 ms CDN batch | 10.208 ms | 0.716 µs |
| Pinned account admission with 1,000 loaded accounts | 3.176 µs | 0.113 µs |
| Photo normalization CPU | 9.93 µs | 6.18 µs |
| Video normalization CPU | 14.25 µs | 6.74 µs |
| Five-item mixed-carousel normalization CPU | 44.29 µs | 20.34 µs |
| Mixed-carousel normalization allocations | 23.7 KB | 9.0 KB |

Live checks used the same Reel and two saved accounts, with the first URL refreshed and subsequent aliases requested normally. Fresh upstream metadata medians were 252.3 ms before and 256.7 ms after; a single 12,782,105-byte download took 266.7 ms before and 280.1 ms after. These small live samples show no improvement in upstream network speed. The downloaded bytes had the same SHA-256 hash, and likes/views remained available. The gains are from avoiding duplicate upstream requests, removing transfer-induced queue waits, and reducing local CPU/allocation work.

Scheduler figures are medians of three controlled runs and exclude upstream requests. Normalization figures are medians of four synthetic benchmark runs using 12 image candidates, six video candidates, and a five-child mixed carousel. Reproduce the local benchmarks with:

```sh
go test ./internal/instagramapp -run '^$' -bench 'BenchmarkInstagramMetadataDuringDownload' -benchtime=100x -count=3
go test ./internal/instagramapp -run '^$' -bench 'BenchmarkInstagram(PinnedAdmission|MetadataAdmission)$' -benchmem -benchtime=300ms -count=3
go test ./internal/platform -run '^$' -bench 'BenchmarkInstagram(Prepare|Normalize)$' -benchmem -benchtime=300ms -count=4
```

Regression checks cover concurrent URL aliases and refresh, separate metadata/CDN limits, shared queue bounds, cancellation, fixed proxy routes, and unchanged quality selection. The service package race suite, vet, build, and live extraction/download checks passed. Raw local measurement outputs are in `tmp/instagram-perf/` (ignored by Git).
