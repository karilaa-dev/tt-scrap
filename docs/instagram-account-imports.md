# Importing Instagram accounts

This guide covers the native account importer in tt-scrap. Imports choose and check a proxy route, prefer browser cookies and fall back to the account's password and authenticator secret when the session expires, verify the returned session, and save it in PostgreSQL. Saved accounts are available immediately and load again after restart. A phone, ADB, Python, and instagrapi are not required at runtime.

## 1. Prepare the service

Run a version containing the Instagram account-import API. Configure the existing `DB_URL`, `DB_SCHEMA`, and `TT_SCRAP_API_KEY`, plus:

```dotenv
INSTAGRAM_METHOD=app
INSTAGRAM_ACTIVE_ACCOUNTS=10
INSTAGRAM_ACCOUNT_CONCURRENCY=1
INSTAGRAM_ACCOUNT_DOWNLOAD_CONCURRENCY=1
INSTAGRAM_LEGACY_FALLBACK=false
```

Proxy candidates come from tt-scrap's configured/stored proxy pool. Use the existing `PROXY_FILE` to seed proxies if needed. `PROXY_INCLUDE_HOST=true` explicitly includes a direct-host route; it is not necessary when using proxies already stored in the database. File-based Instagram sessions are optional: API imports do not require `INSTAGRAM_ACCOUNTS_FILE` or `INSTAGRAM_ACCOUNTS_DIR`.

Set the API origin in your shell and make the existing service API key available as `TT_SCRAP_API_KEY`:

```sh
export TT_SCRAP_BASE_URL='http://127.0.0.1:8000'

curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

Use HTTPS when sending credentials to a remote service. Account administration works even when there are no healthy Instagram accounts and `/health/ready` returns 503; use a reachable service origin that allows those administration requests. The API process and its database/proxy initialization must have started successfully.

## 2. Prepare a CSV, JSON or cookie export file

Keep the file private, for example with `chmod 600 /private/instagram-accounts.csv`. The examples below contain placeholders; replace them with the account's actual values.

### CSV

The importer recognizes these exact column names:

| CSV column | Meaning |
| --- | --- |
| `User ID / Email Address` | Instagram login username |
| `Password` | Account password, preserved exactly |
| `First Secret Question` | Authenticator **TOTP secret**, not a security question or a current six-digit code |
| `Device Profile` | Optional device profile ID; blank or `random` selects one automatically |
| `Cookies` | Optional JSON cookie array; use CSV quoting for embedded quotes |

```csv
User ID / Email Address,Password,First Secret Question,Device Profile
'example_account,REPLACE_WITH_PASSWORD,REPLACE_WITH_BASE32_SECRET,galaxy-s23-android-14
```

The first three headers are required. Other columns, including `First Secret Answer`, are ignored. A leading spreadsheet apostrophe (`'`) is removed from the username before duplicate detection and login. Surrounding username whitespace is trimmed; passwords are not trimmed or otherwise normalized. Use normal CSV quoting for passwords containing commas, double quotes, or line breaks. UTF-8 BOMs and CRLF line endings are supported.

For an account without authenticator 2FA, leave `First Secret Question` empty. If Instagram requires authenticator verification, a valid secret is needed. SMS/email verification and interactive challenges are not implemented.

### JSON

Save this structure to a private file, such as `/private/instagram-accounts.json`:

```json
{
  "accounts": [
    {
      "username": "example_account",
      "password": "REPLACE_WITH_PASSWORD",
      "totp_secret": "REPLACE_WITH_BASE32_SECRET",
      "device_profile": "galaxy-s23-android-14"
    }
  ]
}
```

For password imports, `username` and `password` are required. `totp_secret` may be omitted for an account without authenticator 2FA. Both formats accept Base32 secrets with spaces or an `otpauth://totp/` URI using SHA-1, six digits, and a 30-second period. Keep the server clock synchronized: tt-scrap generates the code just before submitting it.

Discover supported profiles with:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/device-profiles" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

Omitting `device_profile`, leaving it blank, or using `random` selects a profile once during login. The generated device identity is persisted with the session and reused after restart. An unsupported profile rejects the entire batch before login begins.

### Browser cookies

Exported browser cookies are accepted directly, including `.txt` files with one JSON cookie array per account on separate lines. A single pretty-printed array is also accepted. Each array needs `sessionid` and `ds_user_id` cookies scoped for `i.instagram.com`; domain, path, host-only and expiration attributes are respected. Keep the file private: session cookies grant account access.

Upload that file without converting it to passwords or bearer tokens:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: text/plain' \
  --data-binary @/private/instagram-cookies.txt
```

Alternatively, use `-F 'file=@/private/instagram-cookies.txt;type=text/plain'`. For `application/json`, submit a single cookie array or `{"accounts":[{"cookies":[...],"device_profile":"random"}]}`. You can include `username`, `password`, and `totp_secret` alongside `cookies` on the same account:

```json
{"accounts":[{"cookies":[{"domain":".instagram.com","path":"/","name":"sessionid","value":"SESSION_VALUE"},{"domain":".instagram.com","path":"/","name":"ds_user_id","value":"123"}],"username":"example_account","password":"REPLACE_WITH_PASSWORD","totp_secret":"REPLACE_WITH_BASE32_SECRET"}]}
```

Cookies are verified first. A working cookie session skips password login. Expired cookies or an explicit `login_required`/`session_expired` response trigger one password login with TOTP if needed. The verified account ID must match the cookie account. Partial login credentials are rejected. Rate limits, challenges, network errors, and other service failures do not trigger password fallback.

Successful cookie imports store the original cookie JSON and the selected device/proxy in PostgreSQL. The Android bearer is derived from `sessionid` and `ds_user_id` only in memory, including after restart; the bearer column remains NULL. Supplied username/password/TOTP credentials are also saved privately in `login_credentials`, including for password-only imports. Treat this database column and backups as sensitive credential storage, just like session tokens. Password imports retain their bearer-session storage. Cookies and bearers are never returned by the account list or import response.

Existing accounts are matched by Instagram user ID for cookie imports and by normalized username for password imports. An unfamiliar login name or email is matched by its verified Instagram user ID after login. Both import directions update the same account row and return the existing `account_id`:

- **Cookies into an existing password account:** upload the cookie export normally. The importer verifies it on the account's saved proxy and device, then stores it as the preferred session while keeping the password/TOTP backup. A verified session update reactivates a dead account. Verification failure preserves the previous session and backup.
- **Password/2FA into an existing cookie account:** submit only its username, password and TOTP secret; the cookies do not need to be sent again. A known username updates the backup without attempting password login or replacing working cookies. Incorrect backups fail when recovery is needed. Login-only updates do not reactivate dead accounts.
- **Both together:** include both fields to update the session and backup together. Omitted login fields preserve the saved backup; omitted cookies preserve the session. Unchanged credentials return `duplicate`.

For example, add a backup to an account previously imported using only cookies:

```json
{"accounts":[{"username":"example_account","password":"REPLACE_WITH_PASSWORD","totp_secret":"REPLACE_WITH_BASE32_SECRET"}]}
```

Use the same `POST /v1/instagram/accounts/imports` endpoint. Account ID, insertion order, proxy and device are preserved. Cookie replacement waits for older metadata requests and their health writes to finish, so an old authentication failure cannot disable the replacement session.

Malformed cookies reject the batch before network requests. Expired cookies require a complete login backup; without it, they also reject the batch. Account-specific failures return per-row results, and rate limits skip remaining rows.

### Automatic recovery

When extraction encounters an expired or rejected session, the account attempts password/TOTP recovery on its existing proxy and device, then retries metadata once. Concurrent requests share the login attempt. The verified user ID must match the saved account, so a bad backup cannot switch it to another account. A successful replacement session is committed to PostgreSQL before use, and the rejected cookies are cleared. Subsequent requests and restarts reuse the replacement; login credentials remain available for future recovery.

Temporary login/network/database failures cool down for at least one minute before retrying. Rejected passwords, unsupported 2FA, account mismatches and challenges disable the account as before. An expired cookie account with a saved backup can recover after restart; cookie-only expired accounts remain disabled. Existing accounts imported before this change have no saved password/TOTP: re-import login credentials to enable recovery. Login-only updates do not reactivate dead accounts; verified replacement sessions do.

## 3. Submit the import

CSV multipart upload:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -F 'file=@/private/instagram-accounts.csv;type=text/csv'
```

Raw CSV upload:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: text/csv' \
  --data-binary @/private/instagram-accounts.csv
```

JSON upload:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @/private/instagram-accounts.json
```

Uploads are limited to **1 MiB and 100 accounts**. Multipart requests must contain exactly one file named `file`, with no additional form fields. Empty batches and duplicate usernames within the same batch are rejected; username matching is case-insensitive after normalization.

Imports are synchronous and sequential, in the submitted order. There is no background job to poll. Only one import runs at a time; a concurrent import gets 409 `import_busy`. Allow enough time in your HTTP client and reverse proxy for the whole batch, or submit smaller batches. Each route probe can take up to 10 seconds, and each account's login sequence can take up to 90 seconds.

The importer checks the least-assigned proxy first, skips failed proxy probes, and submits credentials on one checked route. Multiple accounts can share a route/IP. It saves the proxy URL and observed egress IP with the account. Use static/sticky proxies to preserve that IP: tt-scrap cannot prevent a provider from rotating the address behind a proxy URL.

## 4. Read the per-account results

An HTTP 200 response can contain failures. Inspect every result; `curl --fail-with-body` only detects HTTP-level errors.

```json
{
  "results": [
    {"row": 1, "account_id": "account-id", "status": "imported"},
    {"row": 2, "status": "failed", "error_code": "upstream_rate_limited"},
    {"row": 3, "status": "skipped", "error_code": "batch_rate_limited"}
  ]
}
```

`row` is one-based and excludes the CSV header. For cookie exports it counts account arrays.

| Status | Meaning |
| --- | --- |
| `imported` | Login and session verification succeeded; the account was saved and added to the pool. |
| `updated` | Credentials were added or replaced on the existing account; its ID, device and route were kept. |
| `duplicate` | A matching account is already stored; no replacement session is written. Known usernames skip login entirely. A conflict discovered after login can also produce this status, without an `account_id`. |
| `failed` | This account was not imported; inspect `error_code`. |
| `skipped` | No attempt was made for this row because an earlier account was rate-limited. |

The whole batch is validated before any login starts, but successful accounts are committed individually. An error or disconnect can leave earlier rows saved. Check the account list before retrying an interrupted request. Rate limits stop the remaining batch; the importer does not repeatedly submit passwords or TOTP codes.

## 5. Verify persistence and account selection

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

The response contains `active_limit` and an oldest-first `accounts` list. Each account has `id`, `state`, `device_profile`, and optional `username` and `egress_ip` fields. Passwords, TOTP secrets, session tokens, generated device IDs, and proxy credentials are not returned.

- `active`: one of the oldest healthy accounts selected by `INSTAGRAM_ACTIVE_ACCOUNTS`. It may temporarily be busy or cooling down.
- `reserve`: healthy, but newer than the selected active accounts.
- `dead`: permanently excluded from metadata selection after an authentication failure or challenge.

For example, with a limit of 10 and 15 healthy accounts, the oldest 10 serve metadata. If one becomes dead, the next oldest reserve replaces it. Temporary cooldowns and busy accounts do not promote reserves. Requests balance across available active accounts, with separate per-account limits for metadata and CDN downloads.

No restart is needed after import. After a normal service restart using the same database and schema, the saved accounts, device identities, routes, and dead status are restored. Supplied passwords and TOTP seeds are retained privately with the authenticated session for automatic recovery. They are not returned by account APIs. Changing or removing a seed file does not remove a stored account.

Re-importing only login credentials for a dead account returns `duplicate` or `updated` and keeps it dead. Import a working cookie session for that account to verify and reactivate it. There is no account-delete API.

## Troubleshooting

| Error | Next step |
| --- | --- |
| HTTP 401 | Use the service API key in the bearer header. |
| `invalid_account_import` / HTTP 422 | Check headers/JSON shape, required values, batch size, duplicate usernames, and TOTP secret format. |
| `invalid_device_profile` / HTTP 422 | Select an ID from the device-profile endpoint, or omit it. |
| `import_busy` / HTTP 409 | Wait for the current import to finish. |
| `proxy_check_failed` | Check the configured proxy pool, credentials, and access to the HTTPS IP probe. No account password is submitted on a failed probe. |
| `upstream_rate_limited`, `batch_rate_limited` | Stop immediate retries. Check the account/route state and retry later, after throttling clears. |
| `challenge_required` | Resolve the account's challenge through Instagram; the importer cannot complete interactive challenges. |
| `unsupported_two_factor` | Check that authenticator TOTP is available and that the supplied seed belongs to this account. Other verification methods are unsupported. |
| `login_rejected`, `login_session_not_returned` | Check account status and exact login credentials. The server did not obtain a usable bearer session. |
| `session_check_failed` | Login returned a session, but authenticated verification on the selected route failed; no account was saved. |
| `instagram_database_unavailable` / HTTP 503 | Restore database access, then check the account list for previously committed rows before retrying. |

Legacy fallback applies to **media extraction**, not account login. `scrap_instagram_attempts.legacy_used` records extraction-method attempts; it is not an import-success flag. See [Instagram architecture and legacy measurements](instagram-app.md) for configuration and SQL examples.

## Cookie-import verification

On 2026-10-30, the last six accounts of a supplied browser export were submitted through the real HTTP import endpoint in an isolated temporary database schema. Five passed session verification and were persisted as cookies with NULL bearer columns; one failed session verification. Reopening the pool restored all five, and a photo extraction succeeded. Only those six accounts were attempted, and the temporary schema was removed afterward. This does not guarantee that every exported session remains usable.

The schema migration adds nullable `cookies` and `login_credentials` JSONB columns and makes `android_authorization` nullable. Existing password-session rows remain unchanged. Older releases cannot load cookie-only rows; use this version or later after importing cookies.

## Verification and tt-bot compatibility

Rechecked on 2026-10-30: a fresh development-account login through the real HTTP imports handler returned `imported` in 21.42 seconds, with a verified session, device profile, and egress IP saved in an isolated temporary database schema. Saved-session Reel and photo extraction/download tests also passed. The check did not replace the normal database's existing account records or test every account in the CSV.

tt-bot `main` at `613f6d3` already accepts the new Instagram metadata, displays/caches likes and views, and treats music downloads as optional. This tt-scrap implementation returns `music.audio: null`, so newly extracted posts do not get a sound-download button. Account administration stays in tt-scrap; tt-bot has no account-import UI/API wrapper.

Deploy the updated tt-scrap API before using that tt-bot version for Instagram. Its client requires `method` and `resolved_url`; the older tt-scrap `main` at `66e8cbf` omits them and is rejected as `invalid_response`. Selecting `legacy` in the updated tt-scrap is compatible because the updated legacy response includes those fields. This is a source compatibility check, not confirmation of the deployed bot/service versions.
