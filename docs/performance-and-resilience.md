# Go performance and resilience

The service uses pooled native HTTP transports, bounded work queues, metadata request coalescing, TTL/LRU caches and streamed media files. Caches track expiry separately from access order, purging expired entries before evicting live data without scanning the live cache. Request URLs retain the original 2,083-byte limit before parsing or caching. App identity selection and health checks run in RAM; PostgreSQL is read at startup and receives completed operation state afterward. A periodic ownership-connection ping detects an idle database failure.

An identity stays on one proxy and is exclusively leased while its request or media stream uses mutable session state. Ten slots per proxy are prepared before HTTP startup by one worker per proxy address, with a ten-second gap after each preparation attempt completes. Existing fresh registrations are reused. Three-hour signing renewal during normal operation and recovery remain lazy. Explicit signature rejection renews the seed; an invalid registration recreates the device in the same slot. Rate limits cool down an identity, and network failures cool down its proxy. Recovery retries once per identity operation. Single-process ownership and revision checks stop conflicting writers.

App extraction ranks video candidates using API-reported resolution, frame rate, and bitrate without CDN quality probes. Cached clients preserve cookie jars between operations. Cookie snapshots rebuild only after a change or expiry. The [earlier app comparison](go-app-optimization.md) measured the previous build with probes and does not establish latency for this change.

App downloads keep one identity lease across fallback URLs and adaptive tracks, then save one completion checkpoint. Adaptive video and audio download concurrently, cancel together on failure, and share one cumulative byte budget before muxing. Failed attempts release their budget after temporary-file cleanup. With `PROXY_DATA_ONLY=true`, CDN bytes use a direct transport while signed app endpoints keep their identity proxy. Web media retries rotate through the configured proxies after the initial sticky choice fails.

Slideshow assets sharing an identity wait for its exclusive lease before starting their transfer timeout. Caller cancellation still applies while queued, and local cancellation or transfer deadlines do not mark the identity or proxy unhealthy.

A canceled metadata leader releases its flight so still-active followers can retry. Trailing-slash redirects use relative locations to preserve the public HTTPS origin behind an ingress. Thumbnail pass-through validates the complete JPEG stream.

Downloads use reusable copy buffers and temporary files, with actual size and checksum verification. Multipart Telegram uploads stream those files and provide an exact Content-Length. Native image conversion has a separate concurrency limit. Telegram album preparation completes before uploads start; uncertain upload failures are not automatically retried.

The default extraction/download limits are 32/64, with 128 connections per host and 1,024 queued requests. Actual capacity depends on proxy count, upstream latency, media sizes, disk throughput and memory for decoded images. `MAX_ASSET_BYTES`, image concurrency and `GOMEMLIMIT` allow deployment-specific bounds.

Live measurements, failures and limitations are in [the Go/Python comparison](go-python-comparison.md). Run `go test -race ./...` with an isolated test database to verify concurrent identity use, failure persistence, ownership loss, queue cancellation, API compatibility and streamed media behavior.
