# Migrate to the Go service

The Go service replaces the Python server and its app worker with one binary. TikTok uses only the Android app method; Instagram remains available. Existing routes, authentication, and error envelopes retain their contracts. The embedded OpenAPI document adds GET/POST identity preparation at `/v1/tiktok/identities/preparations`.

## Prepare the deployment

Build the [Docker image](../Dockerfile), or install the native dependencies and run `make build`. Replace the Python entrypoint with `./bin/tt-scrap`. Python, uv, Node, npm, and Bun are no longer part of the build or runtime. libheif remains required for image handling. MP4 validation runs in Go; FFmpeg is no longer required.

Review `.env` against [`.env.example`](../.env.example). Existing environment variables take precedence over the file. Keep your API key, Instagram key, Telegram settings, and download limits. Replace `YTDLP_COOKIES` with `TIKTOK_SESSION_ID`, containing only the value of your TikTok `sessionid` cookie. Cookie files and stored account credentials are no longer loaded. TikTok requests start anonymously and use the environment session only after a content restriction. The session is not saved in PostgreSQL. See the [account fallback configuration](../README.md#identity-lifecycle).

Remove `TIKTOK_METHOD` from your environment; the method selector and TikTok web extractor have been removed. Supply `DB_URL`; the service creates `scrap_proxies`, `scrap_identities`, and `scrap_ownership` in `DB_SCHEMA` (default `public`). The schema must already exist. The database role needs permission to create, alter, and update those tables. Existing compatible app identities are loaded from the database.

For a new app database, import proxy addresses through `PROXY_FILE`, or set `PROXY_INCLUDE_HOST=true` to use the host directly. Saved database proxies do not require a proxy file. With Docker Compose, use `compose.proxies.yml` alongside `docker-compose.yml` to mount `proxies.txt`. The base Compose file requires only `.env` and a reachable database.

App startup registers all ten identities per proxy before opening the HTTP listener. It does not fetch signing seeds; requests fetch them on demand. Allow roughly 90 seconds plus upstream time for a fresh pool with healthy proxies; proxy workers run in parallel. A restart with a fully registered pool makes no TikTok requests before opening the listener. Retries can extend this. Startup exits if any slot remains unprepared after three attempts, retaining saved progress for the next start. Configure deployment startup deadlines accordingly; the Docker image allows a five-minute initial health-check grace period.

## Replace the running process

Stop the previous app service before starting the Go service against the same schema. One app process owns a schema through a PostgreSQL advisory lock. A persisted ownership token prevents a disconnected process from resuming with stale RAM after another process has started and stopped. Overlapping app replicas cannot share a schema, including during a rolling deployment. Use separate schemas and proxy assignments for independent app instances.

Start the new service and check `/health/live`, `/health/ready`, and an authenticated request. Readiness requires the Instagram API key as well as a healthy app pool. Startup reuses saved identities and registers missing slots until all ten identities per proxy are registered; the HTTP listener opens only afterward. Signing material is fetched before the first signed request and refreshed on use once it is an hour old.

RAM caches and asset tokens restart with the process. Request fresh metadata after deployment instead of reusing old asset tokens. Allow 45 seconds for container shutdown so the service can finish its 30-second request drain, let cancelled identity operations return, and spend at most 10 seconds saving identity state, including cancelling a stalled save and closing the database connection.

## Roll back

Keep the previous image and configuration until verification finishes. Stop Go before starting the previous app implementation against its database. The `scrap_*` tables are separate from older application tables; do not drop unrelated tables. The Python release at `v1.3.5` remains in Git history.
