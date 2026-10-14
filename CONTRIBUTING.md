# Contributing

Use the Go version in `go.mod`. Builds and tests require CGO, a C compiler, `pkg-config`, and libheif with HEIC and AVIF codecs. See the [README](README.md) for package installation.

## Check a change

Run the same checks as CI:

```sh
make fmt
make check
make lint
```

`make check` checks formatting and module files, runs `go vet` and race-enabled tests, and builds `bin/tt-scrap`. `make lint` runs staticcheck and govulncheck at the versions pinned in the `Makefile`; the first run downloads them. `make run` builds and starts the service with your local `.env`. Formatting does not change the embedded `openapi.json`; API changes require an explicit contract update.

Database tests create and remove isolated schemas. Without a database URL, these tests skip. To run the full suite against a disposable PostgreSQL instance:

```sh
docker run --detach --name tt-scrap-test-db \
	-p 127.0.0.1:15432:5432 \
	-e POSTGRES_USER=scrap_test \
	-e POSTGRES_PASSWORD=scrap_test \
	-e POSTGRES_DB=scrap_test postgres:18
docker exec tt-scrap-test-db pg_isready -U scrap_test -d scrap_test
export TIKTOK_APP_TEST_DB_URL=postgresql://scrap_test:scrap_test@127.0.0.1:15432/scrap_test
make check
docker rm --force tt-scrap-test-db
```

Wait for `pg_isready` to report that PostgreSQL accepts connections before running the tests. The test role needs permission to create schemas. `TIKTOK_APP_TEST_DB_URL` takes precedence over `DB_URL` and the root `.env`.

HTTP tests use local fixtures. Live tests require explicit `TT_SCRAP_LIVE_APP=1`; see [app method verification](docs/tiktok-app.md). Instagram and Telegram tests use fixtures and send no real messages.

## Find the implementation

| Path | Responsibility |
| --- | --- |
| `cmd/tt-scrap` | Server startup, readiness command, and shutdown |
| `internal/httpapi` | Public routes, authentication, validation, and Telegram delivery |
| `internal/core` | Configuration, models, caches, and work limits |
| `internal/platform` | TikTok and Instagram extraction, quality selection, and downloads |
| `internal/identity` | PostgreSQL storage, RAM identity pool, proxy binding, and recovery |
| `internal/tiktokapp` | Native registration, signing, app requests, and device catalog |
| `internal/nettransport` | Shared HTTP transports and explicit proxies |
| `internal/media` | Images, animation, and native HEIF decoding |
| `openapi.json` | Embedded public API contract |

## Measure performance

```sh
make bench
```

Signing, request-routing, and image-orientation benchmarks run locally. Extraction benchmarks require the test database URL and use local HTTP fixtures with isolated database schemas. They exercise concurrency 1, 4, and 16. `BenchmarkAppDelivery` also exercises the complete Telegram delivery handler at concurrency 1, 16, and 64, with cached metadata, real PostgreSQL checkpoints, a controlled CDN delay, and local multipart uploads whose bytes are verified. It excludes identity generation and signing. These measurements describe local work and simulated upstream waits, not TikTok capacity. Use repeated trials before reporting changes. Keep registration and seed renewal outside upstream-API measurements.

Write benchmark output outside the repository, for example `/tmp/tt-scrap-bench.txt`.
