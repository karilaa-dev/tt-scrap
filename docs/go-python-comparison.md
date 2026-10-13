# Go and Python comparison

Go substantially reduced local CPU and memory in this workload. End-to-end latency was close and dominated by upstream waiting; these short runs do not establish a maximum throughput or a general latency winner.

Measurement ran on 2026-10-13 from 05:13:52 to 05:31:52 UTC: **18 minutes**, including setup between phases. Go 1.27.2 was compared with the restored Python 3.13.14 service, whose app backend uses Node 24.18.0. All sides used the same proxy and four public feed videos, with concurrency **1 and 4**. Registration and signing-seed renewal were prepared before measurement.

## Public service: concurrency four

Latency is the median of per-trial median request times, in seconds. CPU is process-tree CPU-seconds for a batch of four requests; RAM is the median peak proportional set size (PSS), which apportions shared pages. CPU and RAM below measure extraction followed by the full download.

| Implementation | Metadata endpoint | Download only | Both consecutively | CPU / four | RAM | Complete combined trials |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Go app | 2.465 s | 0.873 s | 2.477 s | 0.190 s | 30.4 MiB | 3 |
| Python app | 2.849 s | 0.732 s | 2.518 s | 0.510 s | 270.6 MiB | 1 |
| Python web | 2.130 s | 1.483 s | 2.420 s | 0.310 s | 166.9 MiB | 3 |
| Go web, exploratory | 1.469 s | 1.639 s | 2.074 s | 0.180 s | 22.5 MiB | 1 |

The Go app combined latency range was **2.446–2.510 s**, CPU **0.170–0.210 s**, and peak PSS **30.0–30.4 MiB**. Python web latency ranged **2.189–3.107 s**, CPU **0.310–0.320 s**, and PSS **166.1–173.9 MiB**. Python app had only one complete concurrency-four combined trial, so its figures cannot establish repeatability. Peak RSS was 32.7 MiB for Go app, 334.8 MiB for Python app, and 205.2 MiB for Python web.

At concurrency one, median combined latency was **2.356 s** for Go app (range 2.142–3.195), **2.210 s** for Python app (1.968–2.451; two complete trials), and **1.905 s** for Python web (1.864–2.199). Concurrency-four batch throughput was 1.326, 1.354, and 1.599 completed videos/second respectively. These are observed four-request batches, not sustained capacity limits.

The metadata endpoint includes normal per-request signing, quality probes, normalization and persistence. Download-only preparation happens outside its timer. “Both” is measured independently; it is not the sum of the other columns. Every response body is consumed inside the timer. Hash, length, MP4 packets, audio and dimensions are checked afterward.

## Upstream API only: signing excluded

App registration, renewal, request signing, DB work and the preparation coordinator are excluded from these measurements. The measured processes are the Go HTTP executor, the Python app service's original Node HTTP engine, and the Python web extractor. Each ran five trials at each concurrency; all **120 measured requests succeeded**.

| Engine, concurrency four | Median request | Four-request batch median [range] | CPU / four [range] | Peak PSS [range] |
| --- | ---: | ---: | ---: | ---: |
| Go app HTTP | 0.853 s | 1.133 s [1.043–1.233] | 0.0205 s [0.0174–0.0209] | 14.9 MiB [13.6–15.2] |
| Python app's Node HTTP | 1.195 s | 1.301 s [1.199–1.606] | 0.0386 s [0.0358–0.0452] | 39.9 MiB [39.8–40.0] |
| Python web extractor | 1.456 s | 1.575 s [1.528–2.077] | 0.2090 s [0.2007–0.2340] | 71.2 MiB [70.7–73.2] |

At concurrency one, median requests were 0.709 s, 0.714 s and 1.368 s respectively. The corrected app trials alternated implementations; Python web was measured in an earlier block and does different upstream work. Its cross-method latency comparison is exploratory. Native Go request-signing correctness and its production cost are covered separately by protocol tests and the public-service measurements.

## Limits and failures

- Whole-service attempts: Go app **72/72**, Python web **72/72**, and supplementary Go web **12/12** succeeded. Python app succeeded **69/72** actual attempts; three downloads timed out after 45 seconds. An additional unmeasured warmup timed out, preventing four scheduled combined requests. Failed batches remain in the raw results and are excluded from success-only resource summaries.
- App implementations downloaded identical media: **6,528,176 bytes** for four videos. Both web implementations downloaded **9,034,293 bytes**, 38.4% more, because one rendition used H.264 instead of HEVC. Web versus app download time therefore compares different byte counts.
- One Python app metadata trial overlapped an unrelated Docker build. Its raw result is retained and marked excluded; a clean replacement is included. No other failed or slow trials were silently removed.
- The initial isolated-app preparation failed on an omitted empty `deviceToken`; no app HTTP was measured in that pilot. Serialization and startup backfill are fixed, and all app API trials were rerun successfully. The [pilot record](benchmarks/go-api-pilot.json.gz) is retained.
- Three rotating whole-service rounds and one exploratory Go web round fit the requested time budget. They are sufficient to show a large memory difference, but too few to rank small latency differences reliably.
- CPU/RAM include server child processes, including Python's Node worker, but exclude PostgreSQL and the benchmark controller. Memory sampling every 50 ms can miss short peaks. Go app used about 6.4% of one core during the combined concurrency-four batch; negligible client queue time and low CPU support upstream waiting as the observed limiter. Higher concurrency and saturation were not measured.
- The ten copied device IDs and signing seeds remained present in both isolated databases, with no seed renewal during measurement. Two Go identity JSON records differed; exact timestamp formatting was not normalized before database cleanup, so these differences cannot be classified as verified recreations. Both temporary databases and private connection files were removed.

Raw evidence: [public service](benchmarks/go-python-service.json.gz), [isolated API](benchmarks/go-python-api-only.json.gz), [identity audit](benchmarks/go-python-identity-audit.json.gz). [Baseline provenance](benchmark-baselines.md) records the reference source hash and measurement setup. The raw JSON files are gzip-compressed without changing their contents. The interactive [comparison table](go-python-comparison.html) exposes both concurrency settings and each measured stage.
