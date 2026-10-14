# Go project verification

## App short-link resolution (2026-10-14)

The running server logged one uncached resolution at 1,961 ms and another resolution at 0.193 ms. Those request-level logs cannot separate proxy connection time from TikTok response time; they are not a before/after benchmark.

`TestAppShortLinkStopsBeforePostPage` reproduced the unnecessary final-page fetch for both video and photo links. App mode now stops after a validated redirect reveals the post ID: the two-hop fixture makes two requests instead of three and preserves the resolution fields. Web mode still fetches the final page. Twenty concurrent calls for one short link share one upstream request. Tests also cover relative redirects, IDs embedded only in query strings, invalid destinations, redirect limits, and error statuses.

The short-link tests passed three race-test runs, and `make check` passed with isolated PostgreSQL 18. The production Docker build passed. The updated development server reused all 100 prepared identities with zero startup writes.

A live comparison used the two supplied public short links, five runs per version per link, alternating old/new order. Each pair used the same proxy address; five configured proxies were used across the pairs. Every run used a fresh resolver and transport, avoiding resolution-cache and connection-reuse differences. The retained web-mode redirect behavior is identical to the previous app resolver and served as the baseline; no web extraction was performed. The timer covered `ResolveURL`, including redirects, but excluded service construction, app metadata, media transfer, and Telegram upload. Host load was 0.37 across eight cores.

| Short code | Old median (range), ms | New median (range), ms |
| --- | --- | --- |
| `ZP83rLASJ` | 1,475 (1,340–1,572) | 718 (716–871) |
| `ZTyudHPLu` | 1,432 (1,300–2,483) | 746 (703–807) |

All 20 calls succeeded and returned the same source ID for each link. HTTP traces recorded two upstream requests before and one afterward, with one new connection in both versions. Removing the final-page request explains the approximately halved resolution latency; this is not a claim about total bot delivery time. [Raw samples](benchmarks/short-link-resolution-20261014.jsonl) include request counts, connections, source IDs, and durations.

Through the updated authenticated HTTP API, the first resolutions of these short links took 733 ms and 727 ms; their immediate cached repeats took under 1 ms. The supplied full video URL also returned its embedded ID without contacting upstream. These six HTTP smoke calls all returned 200 and are separate from the comparison above.

## Configuration and documentation review (2026-10-14)

Focused tests first reproduced acceptance of invalid Telegram base URLs and lower-level messages leaking through `LOG_LEVEL=CRITICAL`. Configuration now normalizes and validates the Telegram HTTP/HTTPS base URL before startup, and CRITICAL maps above ERROR in the Go logger. The tests pass, as does the complete `make check` with isolated PostgreSQL 18. A built-binary smoke confirmed invalid URL rejection and healthy startup/readiness/shutdown with no lower-level output at CRITICAL. Migration and app documentation now consistently describe eager startup preparation and metadata-only video quality selection.


## Startup preparation and API-based video selection (2026-10-14)

App extraction now selects video quality from API metadata without CDN probes. The focused extraction test failed before the change because it made two media requests and selected the probed rendition; it now passes with zero media requests and the API-ranked rendition. Local downloaded-file validation remains in place.

`make check` passed against an isolated PostgreSQL 18 container, including race tests, vet, module consistency, formatting, and the release build. New tests cover parallel per-proxy preparation, ten-second pacing, shared jobs, restart reuse, seed-only refresh, bounded retry (including waiting before rechecking cooldown), shutdown cancellation, endpoint authentication, and web-mode rejection. CI exposed a pacing edge case when request setup took variable time. A delayed-setup regression reproduced a 20 ms registration gap against a 100 ms test minimum. Preparation now waits from the previous attempt’s completion, preserving the minimum gap regardless of setup time. The extraction benchmark harness also passed a one-iteration correctness smoke after updating its expected zero-probe behavior; this is not a performance comparison.

A production Docker image built from a clean Git-index export and passed readiness, served OpenAPI, preparation authentication/web-mode rejection, and graceful shutdown checks. All pre-existing OpenAPI paths and component schemas were compared against the previous commit and remain unchanged; GET/POST `/v1/tiktok/identities/preparations` is additive.

On the live development server, port 8000 stayed closed during startup preparation. Ten parallel proxy workers prepared and persisted all 100 identities (10 per proxy), then the API opened. Authenticated GET reported `completed`, `ready=100`, `target=100`, `failed=0`; three simultaneous POSTs returned the same completed job without recreating identities. LAN readiness passed. A public test video also extracted and downloaded successfully through the live app API; the 1,549,289-byte response matched its declared size and SHA-256 header. No Telegram messages were sent.

## Earlier migration verification (2026-10-13)

Verified on 2026-10-13 with Go 1.27.2, CGO, libheif, and FFmpeg. The final repository checks ran from an export of the Git index into a clean directory, without local Python or Node tooling.

| Check | Result |
| --- | --- |
| `make check` | Formatting, module consistency, vet, race tests, and release build passed |
| Identity database tests | Passed against a disposable PostgreSQL 18 container |
| Production Docker build | Passed from the clean source export |
| Container startup | App and web modes passed readiness, authentication, validation, and built-in health checks |
| Public OpenAPI | Byte-identical to the original contract and the served container response |
| App startup | Zero identities registered before a request |
| Container user and shutdown | UID 10001; both modes exited successfully on SIGTERM |
| Compose configuration | Base file and optional proxy override validated |
| GitHub Actions workflows | Both passed actionlint |
| Source and credentials | All executable project source is Go; no configured private credentials found in the staged files or compressed reports |

The clean export caught and fixed a root-level credential ignore rule that previously hid the Go cookie source files. Both files are now part of the repository. The Docker build context allows only source and build inputs.

The race suite covers HTTP contracts, media conversion, streamed downloads, protocol vectors, and identity ownership, persistence, balancing, lazy renewal, recovery, cancellation, and checkpoint failures. Live upstream tests remain opt-in. No live Instagram request or Telegram upload is claimed; those integrations use controlled HTTP fixtures.

## Earlier live checks

Before the final repository cleanup, native app and web tests extracted and downloaded the same 1080×1920 public video. Both produced **5,597,812 bytes** with SHA-256 `708f4144d1c517d0e9e81c42fa0cd39f9f1d8403b13913c3a804a057f9c3a9bf`. A separate native app feed test returned six playable videos. The final cleanup changed Go module imports and project files, not extraction behavior.

The [Go/Python comparison](go-python-comparison.md) and [app optimization report](go-app-optimization.md) retain measured CPU, memory, latency, concurrency, failures, and limitations. Their binary hashes refer to the measured snapshots before the module rename. The reference implementations and old cross-runtime harnesses are archived outside this Go-only repository; see [baseline provenance](benchmark-baselines.md).

## PR review regression checks

All eight code findings from the initial PR review were reproduced before their fixes. The nine incoming comments included the review summary itself. Focused checks now cover:

| Behavior | Regression check |
| --- | --- |
| App CDN fallback keeps one identity and checkpoint, preserving cookies and subsequent use | `TestDownloadFallbackRetainsPinnedIdentity` |
| Adaptive video and audio start together; either failure cancels its sibling | `TestSeparateAudioMuxPreservesBothStreamsAndRejectsOversize`, `TestAdaptiveFailureCancelsSiblingAndCleansFiles` |
| Both tracks share the byte cap before muxing, including chunked responses | `TestAdaptiveTracksShareByteBudgetBeforeMux` |
| Failed attempts release their byte allowance before a fallback | `TestFailedDownloadReturnsByteBudgetBeforeFallback` |
| A live metadata follower retries after its leader disconnects | `TestMetadataFollowerRetriesAfterLeaderCancellation` |
| Relative redirects preserve HTTPS at an ingress and retain query parameters | `TestTrailingSlashRedirectPreservesPublicOrigin` |
| Direct CDN transfers retain proxy-bound app signing and redirect cookies | `TestAppDataOnlyUsesDirectCDNWithPinnedPlayRequest`, `TestDirectMediaTransportKeepsAppSigningAndCDNRedirectCookies` |
| Web retries retain the initial proxy, then use another configured proxy | `TestWebMediaRetryRotatesConfiguredProxies` |
| Small JPEG thumbnails require valid pixel data | `TestSmallJPEGThumbnailValidatesPixelStream` |

After these fixes, `make check` passed from a fresh Git-index export against disposable PostgreSQL 18, including race tests, vet, formatting, module consistency, and the release build. The production Docker build also passed. Container checks covered app mode with direct CDN configuration, readiness, relative redirects, unchanged OpenAPI bytes, and graceful shutdown. No live upstream requests or new performance measurements were run for these correctness fixes.

The next review's three code findings were also reproduced and fixed:

| Behavior | Regression check |
| --- | --- |
| A pinned slideshow transfer gets its full timeout after waiting for its identity; caller cancellation still stops queued work | `TestPinnedDownloadWaitDoesNotConsumeTransferTimeout` |
| Transfer deadlines remain enforced without cooling down a healthy identity or proxy | `TestPinnedDownloadTransferTimeoutRemainsEnforced` |
| Expired entries are purged before live LRU victims, including changed expiry, deletion, and clearing | `TestCacheExpiresRecentEntryBeforeEvictingLiveLRU`, `TestCacheExpirationTracksReplacementDeleteAndClear` |
| All URL routes enforce the original 2,083-byte input limit before parsing or caching | `TestURLLengthLimitAcrossPublicRoutes` |

Slideshow assets still take separate exclusive leases; waiting for a lease no longer consumes the transfer budget. URL length behavior, including whitespace and UTF-8 input, was checked against the archived Python `HttpUrl` validator. The full clean-export `make check` passed again with disposable PostgreSQL 18, as did the production Docker build. The built container passed readiness, its built-in health check, unchanged OpenAPI, the 2,083/2,084-byte URL boundary, and graceful shutdown. These checks used fixtures and direct-ID resolution without contacting TikTok, Instagram, or Telegram.
