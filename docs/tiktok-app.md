# Native TikTok app method

`internal/tiktokapp` implements device registration, encrypted SDK seed acquisition, request signing, feed/detail requests, cookies and media routing in Go. Its cryptographic tests use independently generated vectors from the original Node implementation, including all eight signing variants. The production service invokes no Python or JavaScript interpreter.

`internal/identity` owns the PostgreSQL-backed, RAM-resident identity pool. Existing `scrap_proxies` and `scrap_identities` rows remain compatible. Each proxy has ten permanent identity slots, created only when selected. Registration and three-hour signing renewal happen on demand. Completed operations persist their updated sessions, assignment counts and health. Startup loads the pool without registering devices. One process owns a schema; database connection loss or a conflicting revision stops further identity work.

The public API is unchanged. Select `TIKTOK_METHOD=app` and supply `DB_URL`; the `web` backend uses the same endpoints. Proxies can be bootstrapped from `PROXY_FILE` or loaded from the database. New identities use the [Pixel/Samsung device catalog](device-catalog.md); healthy saved devices retain their existing profile.

Each used identity retains its protocol client and cookie jar in RAM. A group of rendition probes owns one identity lease, runs up to three probes concurrently, and persists the final state once. Recovery waits for outstanding probes to finish before renewing or recreating a device; ownership and revision failures remain fatal. [App optimization results](go-app-optimization.md) compare this path with the original Go implementation.

Protocol, persistence and API checks run through `go test -race ./...`. Set `TIKTOK_APP_TEST_DB_URL` for isolated database tests. Opt-in live checks use `TT_SCRAP_LIVE_APP=1` or `TT_SCRAP_LIVE_WEB=1` with `go test -run TestLive -v ./internal/platform`. Live app checks use the configured service database and can lazily register an identity.

The [migration notes](migration.md) describe deployment changes from the Python release. The production and test toolchains use Go; the original Node signing vectors remain as JSON fixtures.
