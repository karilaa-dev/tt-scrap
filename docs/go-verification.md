# Go project verification


## Startup preparation and API-based video selection (2026-10-14)

App extraction now selects video quality from API metadata without CDN probes. The focused extraction test failed before the change because it made two media requests and selected the probed rendition; it now passes with zero media requests and the API-ranked rendition. Local downloaded-file validation remains in place.

`make check` passed against an isolated PostgreSQL 18 container, including race tests, vet, module consistency, formatting, and the release build. New tests cover parallel per-proxy preparation, ten-second pacing, shared jobs, restart reuse, seed-only refresh, bounded retry (including waiting before rechecking cooldown), shutdown cancellation, endpoint authentication, and web-mode rejection. The extraction benchmark harness also passed a one-iteration correctness smoke after updating its expected zero-probe behavior; this is not a performance comparison.

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
