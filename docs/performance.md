# Telegram delivery optimizations

Telegram delivery skips the SHA-256 calculation used only by raw asset responses.
The downloader still validates size, media type, MP4 metadata, and range identity.
Raw downloads retain their checksum and ETag. Single-response raw downloads hash
incoming bytes directly; ranged downloads hash their ordered spool prefix.

Fixed RAM spools can copy ready bytes directly in chunks of at most 128 KiB.
Borrowed memory remains charged to its budget until the write returns, including
cancellation, buffer closure, and recovered output panics. Growing and disk spools
retain buffered reads. Responses with known lengths up to 1 KiB use exact-size
receive scratch; larger responses retain the existing pool.

Image conversion reuses privately decoded RGBA/NRGBA pixels where possible and
scales directly into one padded canvas. Pixel compositing and prepared JPEG bytes
remain unchanged. Converted sources close when their replacement is ready, and
completed album batches release their buffers before the next upload.

## Behavior and compatibility

- Once media becomes upload-ready, an optional cover gets up to 50 ms of selection
  grace within its existing total timeout. Slower covers may be omitted. Cleanup
  joins canceled work before releasing the identity lease, so a decoder that
  cannot stop immediately can still delay completion.
- The final 64 KiB upload gate, full album preflight, captions, ordering, identity
  recovery, cancellation, and resource limits are preserved.
- Existing downloader implementations remain compatible through the original
  interface. Telegram uses the optional delivery capability when available.
- No API schema, environment-variable, database, or deployment migration is needed.

## Measured results

These measurements compare commit `7ea514022336776215c043aa44a507f47e97a01d`
with the optimized production sources using **Go 1.27.2 on both sides**. The PR
uses Go 1.27.3 to resolve the existing GO-2026-6809 vulnerability-check failure;
the numbers below are not new measurements of that toolchain patch.

Five interleaved runs per side on a shared eight-CPU Linux host. Values are
median [minimum–maximum]. Allocation volume is distinct from peak RSS.

| Workload / metric | Original | Optimized |
|---|---:|---:|
| Four 16 MiB video deliveries, RAM spool: worker CPU ms, including warmup | 216.90 [211.42–224.88] | 157.34 [153.27–186.24] |
| Same workload: peak worker RSS MiB | 82.96 [82.81–83.25] | 82.14 [82.01–82.53] |
| Stalled optional cover: complete local delivery ms/op | 1501.34 [1500.95–1502.14] | 51.32 [51.18–51.39] |
| Oriented 1080p JPEG conversion: MiB allocated/op | 21.590 [21.590–21.591] | 13.675 [13.675–13.675] |

Worker CPU medians fell 16–35% across the eight transfer scenarios. Paced video
latency was largely unchanged because source/upload pacing remained the limit.
Unpaced disk and some CDN-limited disk runs had higher latency; those results are
noisy and do not establish a uniform latency improvement. A small upload-limited
RAM RSS increase reversed in five additional paired runs. Across the initial and
targeted follow-up schedules, 520 deliveries passed exact-byte verification with
zero recorded failures.

The tiny-album case retains a CPU/memory tradeoff. Five 1-second confirmations of
21 tiny GIF-to-JPEG images showed 10.277 → 11.565 CPU-ms/album (+12.5%) alongside
26.79 → 23.28 MiB peak RSS (−13.1%). Native JPEG albums showed +9.5% CPU and −14.4%
RSS. These figures include the local source/sink fixtures. Another five-run timing
confirmation measured 2.913 [2.855–3.124] → 3.312 [3.175–3.385] ms for converted
albums. Profiles and GC traces found more collections after the live heap shrank.
Three isolated rollbacks did not recover a clear latency gain. The selected
version therefore does not promise lower resource use for every workload.

The transfer fixture excludes extraction, proxy/identity waits, media conversion,
and Telegram TLS/rate limits. It forces equal RAM/disk storage modes and equal
parallelism; pacing is per connection. Process resources include one untimed
warmup and startup. Latency excludes warmup. RSS excludes kernel sockets and page
cache. Native preparation benchmarks include their local HTTP fixtures and report
per-run means, not request percentiles. A successful live test-bot send was a
separate smoke check, not a latency comparison. Production bots were not used.

Compact measurement records:

- [Transfer process runs](benchmarks/telegram-transfer.csv): initial schedule and
  both targeted follow-ups, with request timings and verified/source counts.
- [Preparation runs](benchmarks/preparation.csv): image, cover, album, and image
  delivery measurements, including the longer album confirmation.
- [Album process runs](benchmarks/album-process.csv): CPU, RSS, and allocation
  evidence for the tradeoff above.
- [Metadata and hashes](benchmarks/metadata.json): original revision, measured
  toolchain, fixture settings, and optimized production-source hashes.

Exploratory language prototypes, live-test helpers, profiles, and full raw reports
remain local under ignored `experiments/`; they are not application dependencies.

## Repeat local checks

`make check` runs formatting, module checks, vet, race tests, and an optimized
build. `make lint` runs staticcheck and govulncheck. Identity tests need the local
PostgreSQL fixture described in [CONTRIBUTING.md](../CONTRIBUTING.md); never use a
production database.

The retained benchmark hooks use loopback fixtures and need no credentials:

```sh
go test ./internal/media -run '^$' -bench '^BenchmarkPrepare$' -benchmem
go test ./internal/platform -run '^$' -bench '^BenchmarkDownload(Local|SmallKnown)$' -benchmem
go test ./internal/spool -run '^$' -bench '^BenchmarkSpoolHash$' -benchmem
go test ./internal/httpapi -run '^$' -bench 'Benchmark(VideoCoverPreparation|AlbumPreparationRetention|ImageDelivery|ThrottledVideoDelivery)$' -benchmem
```

Use identical hooks, fixture bytes, toolchain, build flags, and settings when
comparing revisions. Alternate separately built original/candidate binaries at
least five times; do not run profilers or other benchmarks concurrently. The
`ImageDelivery` hook was moved from the local experiment into `internal/httpapi`
after measurement; its JPEG/source hashes were generated from the original
implementation. Known/chunked raw-download latency can be dominated by local
transport stalls and is not used for speedup claims.
