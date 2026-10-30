# Importing Instagram accounts

Use `POST /v1/instagram/accounts/imports` to add accounts or update credentials on existing accounts. An account can keep both browser cookies and a username/password/authenticator secret. Cookies are preferred; saved login credentials enable recovery when a session expires. Saved accounts are available immediately and load again after restart. A phone, ADB, Python, and instagrapi are not required at runtime.

| What you want to do | What to import |
| --- | --- |
| Add accounts from a browser export | The original cookie arrays as a text or multipart upload. |
| Add accounts using password/2FA | CSV or JSON with username, password, and the authenticator TOTP secret if enabled. |
| Save cookies and a password backup together | JSON accounts with both `cookies` and login fields, or CSV with the optional `Cookies` column. |
| Add or replace cookies on an existing account | The new cookie export; saved password/2FA is retained. |
| Add or replace password/2FA on a cookie account | Username, password, and TOTP secret; existing cookies need not be resent. |

For existing accounts, see [Update an existing account](#4-update-an-existing-account). Import results distinguish newly `imported` accounts, `updated` credentials, and unchanged `duplicate` submissions.

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
| `User ID / Email Address` | Instagram login username or email; use the listed username when updating a known account |
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

For new accounts, omitting `device_profile`, leaving it blank, or using `random` selects a profile once during login. The generated device identity is persisted with the session and reused after restart. Updates to known accounts keep their saved device; a profile field does not rotate it. An unsupported profile rejects the entire batch before login begins.

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
{
  "accounts": [
    {
      "cookies": [
        {"domain": ".instagram.com", "path": "/", "name": "sessionid", "value": "REPLACE_WITH_SESSION_VALUE"},
        {"domain": ".instagram.com", "path": "/", "name": "ds_user_id", "value": "123"}
      ],
      "username": "example_account",
      "password": "REPLACE_WITH_PASSWORD",
      "totp_secret": "REPLACE_WITH_BASE32_SECRET"
    }
  ]
}
```

Replace `123` with the exported `ds_user_id` value and retain the export's cookie attributes. Cookies are verified first. A working cookie session skips password login. Expired cookies or an explicit `login_required`/`session_expired` response trigger one password login with TOTP if needed, using the login fields supplied in that import. The verified account ID must match the cookie account. Partial login credentials are rejected. Rate limits, challenges, network errors, and other service failures do not trigger password fallback.

Successful cookie imports store the original cookie JSON and the selected device/proxy in PostgreSQL. The Android bearer is derived from `sessionid` and `ds_user_id` only in memory, including after restart; the bearer column remains NULL. Supplied username/password/TOTP credentials are also saved privately in `login_credentials`, including for password-only imports. Treat this database column and backups as sensitive credential storage, just like session tokens. Password imports retain their bearer-session storage. Cookies and bearers are never returned by the account list or import response.

Malformed cookies reject the batch before network requests. Cookies whose export expiration has passed require complete login credentials in the same request; a backup already stored in the database is not used to validate or authenticate an import. Account-specific failures return per-row results, and rate limits skip remaining rows.

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

Uploads are limited to **1 MiB and 100 accounts**. Multipart requests must contain exactly one file named `file`, with no additional form fields; use it for CSV or cookie-export files. Send the JSON `accounts` envelope with `Content-Type: application/json`. Empty batches, duplicate usernames, and repeated cookie user IDs within the same batch are rejected; username matching is case-insensitive after normalization.

Imports are synchronous and sequential, in the submitted order. There is no background job to poll. Only one import runs at a time; a concurrent import gets 409 `import_busy`. Allow enough time in your HTTP client and reverse proxy for the whole batch, or submit smaller batches. Each route probe can take up to 10 seconds, and each account's login sequence can take up to 90 seconds.

For accounts not yet matched to a saved account, the importer checks the least-assigned proxy first, skips failed proxy probes, and submits credentials on one checked route. Multiple accounts can share a route/IP. It saves the proxy URL and observed egress IP with new accounts. Known-account updates reuse the saved route and device. Use static/sticky proxies to preserve the IP: tt-scrap cannot prevent a provider from rotating the address behind a proxy URL.

## 4. Update an existing account

Use the same import endpoint and formats as for a new account. No delete, restart, or separate update endpoint is needed. Updates preserve the existing account ID, insertion order, proxy, and device.

### Add or replace cookies

Upload the new cookie export for the account:

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: text/plain' \
  --data-binary @/private/replacement-cookies.txt
```

The importer uses `ds_user_id` to find the existing account, verifies the replacement cookies on its saved proxy and device, and stores them as the preferred session. Saved password/2FA credentials are retained. Failed verification leaves both the previous session and backup unchanged. A verified session replacement also reactivates a `dead` account.

For cookie-only uploads, omit all login fields. To use password fallback during this import, send the [combined JSON format](#browser-cookies) with complete login credentials in the same request.

### Add or replace password and 2FA

Get the account's `username` from `GET /v1/instagram/accounts`. Save the following as `/private/instagram-login-backup.json`, replacing the placeholders:

```json
{
  "accounts": [
    {
      "username": "example_account",
      "password": "REPLACE_WITH_PASSWORD",
      "totp_secret": "REPLACE_WITH_BASE32_SECRET"
    }
  ]
}
```

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts/imports" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @/private/instagram-login-backup.json
```

For a known username, this saves the backup without a password login or replacing the existing cookies/session. An `updated` result confirms storage, not that Instagram accepted the password or TOTP secret; those credentials are used when recovery is needed. Login-only updates keep a `dead` account dead. CSV with the same login fields works too.

An unfamiliar username or email requires a login to discover its Instagram user ID. If that verified ID matches a stored account, the backup is attached to that account and its session is retained. Prefer the username returned by the account list to avoid this extra login. The import body does not accept `account_id` as a selector.

### Update both or keep one unchanged

| Supplied fields | Effect on an existing account |
| --- | --- |
| `cookies` only | Verify and replace changed cookies; keep the saved login backup. |
| `username`, `password`, optional `totp_secret` | Replace the complete login backup; keep the current session. |
| Cookies and login fields together | Verify a changed cookie session and save the supplied backup together. |
| Unchanged credentials | Return `duplicate` without a new login or session verification for a healthy account. |

**Resend the TOTP secret when changing a password if the account uses authenticator 2FA.** Supplying a username and password without `totp_secret` replaces the backup with no TOTP secret. To change just the secret, include the username and password too. Omitting all login fields preserves the existing backup; omitting cookies preserves the session. Partial login fields, such as cookies plus a username alone, are rejected.

A successful change returns the same ID shown by the account list:

```json
{"results":[{"row":1,"account_id":"existing-account-id","status":"updated"}]}
```

## 5. Read the per-account results

An HTTP 200 response can contain failures. Inspect every result; `curl --fail-with-body` only detects HTTP-level errors.

```json
{
  "results": [
    {"row": 1, "account_id": "account-id", "status": "imported"},
    {"row": 2, "account_id": "existing-account-id", "status": "updated"},
    {"row": 3, "status": "failed", "error_code": "upstream_rate_limited"},
    {"row": 4, "status": "skipped", "error_code": "batch_rate_limited"}
  ]
}
```

`row` is one-based and excludes the CSV header. For cookie exports it counts account arrays.

| Status | Meaning |
| --- | --- |
| `imported` | Login and session verification succeeded; the account was saved and added to the pool. |
| `updated` | Credentials were added or replaced on the existing account; its ID, device and route were kept. |
| `duplicate` | The supplied credentials already match the saved account; nothing changed. A database insertion conflict discovered after login can also produce this status, without an `account_id`. |
| `failed` | This row was not imported or updated; inspect `error_code`. A failed update preserves the saved credentials. |
| `skipped` | No attempt was made for this row because an earlier account was rate-limited. |

The whole batch is validated before any login starts, but successful accounts are committed individually. An error or disconnect can leave earlier rows saved. Check the account list before retrying an interrupted request. Rate limits stop the remaining batch; the importer does not repeatedly submit passwords or TOTP codes.

## 6. Verify persistence and account selection

```sh
curl --fail-with-body "$TT_SCRAP_BASE_URL/v1/instagram/accounts" \
  -H "Authorization: Bearer $TT_SCRAP_API_KEY"
```

The response contains `active_limit` and an oldest-first `accounts` list. Each account has `id`, `state`, `device_profile`, and optional `username` and `egress_ip` fields. Passwords, TOTP secrets, session tokens, generated device IDs, and proxy credentials are not returned.

- `active`: one of the oldest healthy accounts selected by `INSTAGRAM_ACTIVE_ACCOUNTS`. It may temporarily be busy or cooling down.
- `reserve`: healthy, but newer than the selected active accounts.
- `dead`: excluded from metadata selection after an explicit account restriction/challenge or unrecoverable session/credential failure; a verified replacement session can reactivate it.

For example, with a limit of 10 and 15 healthy accounts, the oldest 10 serve metadata. If one becomes dead, the next oldest reserve replaces it. Temporary cooldowns and busy accounts do not promote reserves. Requests balance across available active accounts, with separate per-account limits for metadata and CDN downloads.

No restart is needed after import. After a normal service restart using the same database and schema, the saved accounts, device identities, routes, and dead status are restored. Supplied passwords and TOTP seeds are retained privately with the authenticated session for automatic recovery. They are not returned by account APIs. Changing or removing a seed file does not remove a stored account.

Re-importing only login credentials for a dead account returns `duplicate` or `updated` and keeps it dead. Import a working cookie session for that account to verify and reactivate it. There is no account-delete API.

## Automatic recovery

When extraction encounters locally expired cookies or an explicit `login_required`/`session_expired` response, the account attempts password/TOTP recovery on its existing proxy and device, then retries metadata once. Concurrent requests share the login attempt. The verified user ID must match the saved account, so a bad backup cannot switch it to another account. A successful replacement session is committed to PostgreSQL before use, and the rejected cookies are cleared. Subsequent requests and restarts reuse the replacement; login credentials remain available for future recovery. Bare HTTP 401/403 responses and unknown metadata errors retry a different eligible account without disabling the account or triggering password fallback. Network errors, rate limits, and challenges do not trigger password fallback.

Temporary login/network/database failures and ambiguous login/bootstrap rejections during recovery cool down for at least one minute before retrying. Rejected passwords, unsupported 2FA, account mismatches and challenges disable the account. An expired cookie account with a saved backup can recover after restart unless it was already marked dead; cookie-only expired accounts remain disabled. Accounts imported before password-backup support have no saved password/TOTP: [re-import login credentials](#add-or-replace-password-and-2fa) to enable recovery. Login-only updates do not reactivate dead accounts; verified replacement sessions do.

## Troubleshooting

| Error | Next step |
| --- | --- |
| HTTP 401 | Use the service API key in the bearer header. |
| `invalid_account_import` / HTTP 422 | Check headers/JSON shape, required values, batch size, duplicate usernames, and TOTP secret format. |
| `invalid_device_profile` / HTTP 422 | Select an ID from the device-profile endpoint, or omit it. |
| `import_busy` / HTTP 409 | Wait for the current import to finish. |
| `proxy_check_failed` | Check the configured proxy pool, credentials, and access to the HTTPS IP probe. No account password is submitted on a failed probe. |
| `upstream_rate_limited`, `batch_rate_limited` | Stop immediate retries. Check the account/route state and retry later, after throttling clears. |
| `upstream_server_error` | Instagram's login service returned HTTP 5xx. Retry later; automatic recovery cools down and keeps the account and saved credentials. |
| `challenge_required` | Resolve the account's challenge through Instagram; the importer cannot complete interactive challenges. |
| `unsupported_two_factor` | Check that authenticator TOTP is available and that the supplied seed belongs to this account. Other verification methods are unsupported. |
| `login_rejected`, `login_session_not_returned` | Check account status and exact login credentials. The server did not obtain a usable bearer session. |
| `session_expired` | Import a fresh cookie export, or include complete login credentials with cookies for password fallback during import. |
| `session_check_failed` / `account_mismatch` | The session could not be verified for the expected account. Check that cookies, username, password and TOTP belong to the same account. No new session or backup was saved for that row. |
| `instagram_database_unavailable` / HTTP 503 | Restore database access, then check the account list for previously committed rows before retrying. |

Legacy fallback applies to **media extraction**, not account login. `scrap_instagram_attempts.legacy_used` records extraction-method attempts; it is not an import-success flag. See [Instagram architecture and legacy measurements](instagram-app.md) for configuration and SQL examples.

## Cookie-import verification

On 2026-10-30, the last six accounts of a supplied browser export were submitted through the real HTTP import endpoint in an isolated temporary database schema. Five passed session verification and were persisted as cookies with NULL bearer columns; one failed session verification. Reopening the pool restored all five, and a photo extraction succeeded. Only those six accounts were attempted, and the temporary schema was removed afterward. This does not guarantee that every exported session remains usable.

The schema migration adds nullable `cookies` and `login_credentials` JSONB columns and makes `android_authorization` nullable. Existing password-session rows remain unchanged. Older releases cannot load cookie-only rows; use this version or later after importing cookies.

## Verification and tt-bot compatibility

Rechecked on 2026-10-30: a fresh development-account login through the real HTTP imports handler returned `imported` in 21.42 seconds, with a verified session, device profile, and egress IP saved in an isolated temporary database schema. Saved-session Reel and photo extraction/download tests also passed. The check did not replace the normal database's existing account records or test every account in the CSV.

tt-bot `main` at `613f6d3` already accepts the new Instagram metadata, displays/caches likes and views, and treats music downloads as optional. This tt-scrap implementation returns `music.audio: null`, so newly extracted posts do not get a sound-download button. Account administration stays in tt-scrap; tt-bot has no account-import UI/API wrapper.

Deploy the updated tt-scrap API before using that tt-bot version for Instagram. Its client requires `method` and `resolved_url`; the older tt-scrap `main` at `66e8cbf` omits them and is rejected as `invalid_response`. Selecting `legacy` in the updated tt-scrap is compatible because the updated legacy response includes those fields. This is a source compatibility check, not confirmation of the deployed bot/service versions.
