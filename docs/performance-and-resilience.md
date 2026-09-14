# Performance and resilience changes

The public API remains unchanged. Download and delivery errors keep their existing
status codes and bodies, including Telegram passthrough and partially completed
albums. The checked-in OpenAPI schema is unchanged. Media selection, document
bytes, image conversion rules, captions, reply parameters, album order, and cache
TTLs retain their existing behavior.

The supplied logs exposed two application problems beyond expected upstream
failures: file cleanup could fail after Telegram had already accepted an album,
and delayed cleanup or occupied queue slots could prolong unrelated requests.
The log review found eight closed-file 500 responses after accepted albums and
117 unhandled aiohttp cleanup futures. Invalid, deleted, private, restricted, and
inaccessible sources will still produce errors. A CDN 403/404 still permits the
existing alternate-CDN/proxy recovery; it does not prove that a post was deleted.

## What changed

- Multipart payloads borrow their files and calculate size without `fileno()`.
  Small spools stay in memory. The delivery service closes each file once, after
  outstanding worker I/O has drained. Cleanup failures cannot replace an accepted
  Telegram response.
- Required rollover, disk cleanup, file reads, remux checksums, and yt-dlp cleanup
  run off the event loop. Cancellation waits for outstanding file operations.
  FFmpeg retains stream-copy behavior. File uploads retain 64 KiB reads and writes;
  disk reads are grouped into jobs of at most four chunks, with 256 KiB of bounded
  read-ahead. Downloads and relays retain `DOWNLOAD_CHUNK_BYTES`.
- Album permits are acquired before global download permits, including relay
  opening and consumption. Separate audio/video transfers keep sharing the global
  transfer cap.
- Album collection preserves input ordering and the first failure by input order.
  Once that failure is established, remaining work is cancelled and completed
  resources are closed. Every album is fully prepared before the first Telegram
  call. Already running file operations drain before their files are closed.
- Failed resolver pools retire immediately. Active users keep their leases, new
  requests get a replacement pool, and tracked pool cleanup drains at shutdown.
  Cleanup no longer extends the configured resolution deadline.
- Overlapping Instagram requests share a failed provider sequence. New requests
  arriving after that failure may retry; there is no persistent failure cache.
- INFO contains request summaries and lifecycle messages. Summaries include safe
  failure details and maximum queue waits by stage. A bounded 10,000-record logging
  queue prefers warnings/errors on overflow and reports dropped counts. Loop lag
  above one second emits at most one warning per minute.

## Reproduce the measurements

The local benchmark exercises cached and uncached Instagram extraction, HTTP relay,
Instagram carousels, TikTok slideshows, memory/disk document spools, real FFmpeg
remuxing with checksum verification, invalid sources, slow CDN responses, failed
albums, and burst traffic. CDN and Telegram servers run locally. Provider responses
are simulated. It does not measure TikTok's live metadata extraction or internet
latency. The existing `scripts/benchmark.py` remains available for live cached
extraction measurements.

Create a baseline checkout, then use the same Python environment for both versions:

```bash
git worktree add --detach /tmp/tt-scrap-baseline 2429756
uv run python scripts/benchmark_local.py \
  --baseline-root /tmp/tt-scrap-baseline \
  --runs 5 --requests 96 --concurrency 8 \
  --output docs/performance-results.json
```

Runs alternate baseline/candidate order in fresh processes. Each run warms the
healthy paths, executes 96 healthy operations with eight callers, then 96 mixed
operations with sixteen callers. Production concurrency defaults remain in effect;
the benchmark explicitly uses two image workers and zero retry backoff for local
failure fixtures. Failure probes separately set download limits to 2 global / 1
per album and the resolver deadline to 20 ms, with 100 ms socket cleanup.

Latency measures each operation after the benchmark's caller semaphore, including
application queue time. Throughput includes the entire batch. Loop lag samples a
5 ms timer. Memory is peak RSS of the API/benchmark process, including local
upstream fixtures; image worker RSS is excluded. `disk_write_bytes` comes from
`/proc/self/io`. `spool_write_bytes` also counts logical spill writes and rollover
copies, so tmpfs-backed temporary files are visible. Logical spool counters exclude
FFmpeg's direct output writes. Rollovers count files, not syscalls.

The benchmark checks received media hashes, accepts converted JPEGs, and verifies
remux output checksums. The test suite checks detailed media conversion behavior,
Telegram parameters, response bodies, ordering, and partial-delivery structure.

## Measured results

Measured on 2026-09-14 with Python 3.13.14, Linux x86-64, eight visible CPUs,
and tmpfs temporary storage. Values below are medians across five runs per version.
The baseline is commit `2429756`; the candidate is the source snapshot recorded in
[the full results](performance-results.json).

| Metric | Baseline | Candidate |
| --- | ---: | ---: |
| Healthy p50, ms | 69.2 | 79.7 |
| Healthy p95, ms | 1,004.8 | 867.5 |
| Healthy p99, ms | 1,083.4 | 899.5 |
| Healthy operations/s | 38.9 | 44.0 |
| Healthy peak parent RSS, MiB | 201.8 | 214.2 |
| Healthy loop lag p95, ms | 5.15 | 6.18 |
| Healthy loop lag p99, ms | 7.39 | 9.36 |
| Healthy maximum loop lag, ms | 10.48 | 12.81 |
| Healthy logical spool writes, MiB | 282.5 | 213.2 |
| Healthy spool rollovers | 180.0 | 48.0 |
| Mixed burst p50, ms | 153.0 | 139.4 |
| Mixed burst p95, ms | 1,335.6 | 1,123.3 |
| Mixed burst p99, ms | 1,945.9 | 1,516.0 |
| Mixed burst operations/s | 44.9 | 53.1 |
| Mixed burst peak parent RSS, MiB | 243.5 | 265.9 |
| Mixed burst loop lag p95, ms | 9.99 | 9.95 |
| Mixed burst loop lag p99, ms | 12.38 | 12.28 |
| Mixed burst maximum loop lag, ms | 15.35 | 15.75 |
| Mixed burst logical spool writes, MiB | 213.0 | 159.9 |
| Mixed burst spool rollovers | 143.0 | 36.0 |

The healthy p95 gate passes: 13.7% lower latency against the allowed 5%
regression. Healthy throughput increased 13.2%. All 480 healthy operations
succeeded for each version. Every mixed run returned the expected 80 successes,
eight invalid-source 404s, and eight asset-failure 502s, with no unexpected 500s.

Temporary files used tmpfs, so `/proc/self/io` reported zero physical storage
writes for both versions. Logical spool writes fell because small multipart uploads
no longer force rollover. The benchmark does not establish physical-disk savings.

Healthy p50, relay/remux latency under contention, and parent RSS increased.
Keeping small files in memory trades spill writes for resident memory. The benchmark
does not isolate the cause of each latency regression. The figures below show that
the improvement is not uniform across request types.

| Scenario p95, ms | Healthy baseline | Healthy candidate | Burst baseline | Burst candidate |
| --- | ---: | ---: | ---: | ---: |
| cached | 19.8 | 21.1 | 41.5 | 40.9 |
| uncached | 28.9 | 32.6 | 53.0 | 50.9 |
| relay | 25.7 | 35.8 | 65.5 | 65.5 |
| slideshow | 84.0 | 72.7 | 166.7 | 124.4 |
| album | 182.7 | 137.2 | 356.7 | 280.0 |
| document | 361.7 | 254.5 | 768.6 | 459.8 |
| disk | 1083.4 | 899.5 | 1945.9 | 1516.0 |
| remux | 87.5 | 117.1 | 141.2 | 171.4 |
| invalid | n/a | n/a | 53.0 | 54.9 |
| slow | n/a | n/a | 264.8 | 297.8 |
| failedalbum | n/a | n/a | 303.8 | 256.4 |

| Isolated probe | Baseline | Candidate |
| --- | ---: | ---: |
| Unrelated download behind album, ms | 301.204 | 0.045 |
| Resolver with slow cleanup, ms | 120.669 | 20.436 |
| Eight overlapping Instagram failures, ms | 242.183 | 30.528 |
| Provider failure sequences for those eight callers | 8.000 | 1.000 |

The queue and failure-delay probes improved. These are simulated conditions with
controlled waits, not estimates of internet or production latency.

Validation: 225 tests pass, compared with 197 at baseline. Ruff, Ruff formatting,
and mypy pass. Generated baseline/candidate OpenAPI documents and `openapi.json`
have identical SHA-256 hashes. Regression coverage includes real multipart uploads,
memory/disk spools, repeated cancellation, worker drainage, early Telegram responses,
cleanup after accepted/partial delivery, album failures at different positions,
shared Instagram retry sequences and later success, resolver replacement/shutdown,
body-parsing responses, logging overflow, and loop-lag rate limiting.

## Deployment notes

Build and restart using the existing single-worker deployment. No endpoint, schema,
dependency, environment-variable, or cache migration is required. Keep the existing
concurrency defaults: 64 transfers, 8 items per album, 32 delivery pipelines,
20 Telegram uploads, and 4 Instagram provider calls. Explicit configuration still
wins. Download timeouts are unchanged.

Watch request latency and stage queue waits at INFO. Investigate
`runtime.event_loop.stalled` alongside host CPU, memory pressure, storage latency,
and container throttling. Check `logging.records_dropped` for a blocked collector.
Track `media.cleanup.failed`, resolver retirement/cleanup warnings, and Telegram
partial outcomes. Small uploads remaining in memory can increase RSS while reducing
spill writes; compare memory under the real production mix before changing caps.

Invalid/inaccessible-content errors remain expected. This change introduces no
automatic Telegram resends or persistent failure cache. A worker already blocked
in filesystem I/O still has to finish before cleanup. Local measurements cannot
establish whether the remaining production stalls are host or upstream delays.

Rollback uses the previous image with the same configuration and one worker.
Restarting either version discards process-local caches and temporary asset tokens,
as before. No deployment was performed as part of this change.
