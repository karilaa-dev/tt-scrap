# Go app optimization

The app method now runs related quality probes concurrently under one exclusive identity lease and saves their state once. Protocol clients and cookie jars stay in RAM between operations; unchanged cookie snapshots are reused until a cookie changes or expires. Cached dimensions avoid acquiring another media lease. Identity recovery, proxy binding, cancellation and durable completion are preserved.

The optimization left the web implementation, shared HTTP transport, downloader and OpenAPI document unchanged. The later repository cleanup only renamed Go imports in those implementations.

## Results, 2026-10-13

Five alternating controlled trials covered concurrency 1, 4 and 16. With three renditions and 20 ms per simulated upstream response, concurrency-four extraction throughput increased from **29.9 to 67.0 requests/second (2.24×)**. Trial ranges were 25.9–32.9 before and 61.8–69.9 after. Allocated memory fell from **112,028 to 82,822 bytes/extraction**, and identity writes fell from **four to two**: one after metadata and one after the complete probe group. These are fixture results using the real configured PostgreSQL database, not a TikTok capacity estimate.

Live tests used the same proxy, four public videos and ten copied, prewarmed identities per isolated database. Each cell below is the median of five per-trial median request times; brackets show their range. Full endpoint timing includes normal signing and persistence. Registration and SDK renewal were prepared outside measurement. This is not an isolated upstream-HTTP comparison.

| Live operation | Concurrency | Before | After |
| --- | ---: | ---: | ---: |
| Metadata endpoint | 1 | 2.473 s [2.225–2.561] | 1.615 s [1.291–1.868] |
| Metadata endpoint | 4 | 2.900 s [2.635–3.450] | 2.071 s [1.792–2.259] |
| Download only | 4 | 0.800 s [0.685–1.127] | 0.903 s [0.519–2.746] |
| Metadata + download | 1 | 2.709 s [2.208–2.812] | 1.807 s [1.638–2.110] |
| Metadata + download | 4 | 2.595 s [2.037–2.802] | 2.139 s [1.563–2.543] |

Typical live metadata latency fell **29–35%** in this sample. At concurrency four, combined CPU per four requests fell from **0.20 to 0.16 seconds** (ranges 0.18–0.22 and 0.14–0.19). Peak PSS increased slightly, **29.7 to 31.1 MiB**, consistent with keeping clients resident and overlapping probes; this is not a resident-memory reduction.

**Tail latency and full-download throughput did not show a reliable improvement.** A four-request combined batch took a median 2.955 seconds before versus 2.978 after. The optimized run included 14.6- and 16.5-second outliers, one in metadata and one in download; the baseline had a 15.2-second combined request at concurrency one. Download-only latency also varied substantially. Median request gains should not be interpreted as eliminating these waits.

All **240 timed operations passed**, with identical downloaded hashes, sizes, dimensions, codecs and audio. Two unmeasured warmups timed out, one at the first run's deadline; eight scheduled operations were therefore not attempted. They remain in the raw report. A bounded continuation completed the fifth round and replaced the missing optimized metadata trial. The live comparison window was about 11½ minutes; controlled trials took 69 seconds.

## Evidence and checks

The baseline profile showed low CPU utilization while database checkpoints serialized progress. Warm signing separately measured about 13 microseconds/request, so signing algorithms were left unchanged. The larger gains come from overlapping network probes and removing redundant client reconstruction and checkpoints, rather than weakening media validation.

`go test -race -count=1 ./...` passed **112 test cases**; three opt-in live tests were skipped in that command. `go vet`, the release build, and the Docker build passed. New tests cover parallel probes, exclusive ownership, concurrent cookie persistence, cache expiry, cancellation, bounded signing/device recovery, leaked-body cleanup and checkpoint failures. These checks describe the measured snapshot before the Go module path was renamed. Current repository checks are recorded in [verification](go-verification.md).

Both benchmark databases retained their original ten device IDs and SDK seeds. Both databases and all four private connection/session files were deleted after measurement.

Raw results: [controlled extraction](benchmarks/go-app-optimization-local.json.gz), [live comparison](benchmarks/go-app-optimization-live.json.gz), [identity audit](benchmarks/go-app-optimization-audit.json.gz). The [benchmark source](../internal/identity/app_benchmark_test.go) can be run with `go test ./internal/identity -run '^$' -bench BenchmarkAppExtraction -benchmem`; it creates and removes isolated schemas using `TIKTOK_APP_TEST_DB_URL` or `DB_URL`. The historical cross-runtime harness is archived outside this Go-only repository; see [baseline provenance](benchmark-baselines.md). Binary hashes are recorded in the live artifact. The raw JSON files are gzip-compressed without changing their contents.
