Implemented performance improvements
====================================

These changes address the September 6 pool-timeout incident and additional work
found during implementation. They are implemented in the local checkout and have
not been deployed. The [original diagnosis](performance-diagnosis-2026-09-06.md)
records the production evidence and its limits.

| Change | Result |
| --- | --- |
| Recover failed resolver clients | Failed TLS/proxy connections no longer permanently consume all pool capacity. New requests get a fresh client; other active users finish before the old client closes. Healthy clients retain connection reuse. |
| Bound resolution time | The total budget includes the resolution key lock, application semaphore, redirect hops, and retries. HTTPX pool waits no longer consume 15 seconds per attempt. |
| Preserve error types | Exhausted pool acquisition returns HTTP 503 `service_busy`; timeouts return HTTP 504 `upstream_timeout`; other transport failures return HTTP 502 `upstream_network_error`. Invalid redirects and permanent missing-link responses fail without repeated attempts. |
| Cache successful URL mappings longer | URL-to-ID mappings live for 600 seconds by default; extraction metadata keeps its 60-second lifetime. Refresh still bypasses the mapping cache. |
| Share concurrent failed extraction work | Callers already waiting for the same extraction share its failed result. The failure disappears when the group drains, so a later request can retry. |
| Enforce video/audio byte budgets during preparation | Known oversized responses are rejected before their bodies are consumed. Separate video/audio tracks share one budget, including unknown-length responses. A failed track cancels its sibling and retries release earlier byte reservations. The actual remuxed size is checked again. |
| Overlap thumbnail work with video setup | Thumbnail preparation starts before waiting for video response headers. The cover budget also applies to spooled video and audio preparation, and failed media cancels optional cover work immediately. |
| Clean up cancelled work | Cancelled slideshow waiters release their group bookkeeping. Cancelled remuxes kill and reap FFmpeg and close the output spool. |
| Add useful diagnostics | Resolver logs include proxy slot IDs, retirement events, active user counts, HTTPX/httpcore versions, and effective limits. Missing assets produce bounded normalization reason codes without logging the source payload. |

The important defaults are:

| Setting | Default |
| --- | ---: |
| `URL_RESOLVE_TIMEOUT_SECONDS` | 12 seconds |
| `URL_RESOLVE_POOL_TIMEOUT_SECONDS` | 1 second |
| `TIKTOK_RESOLUTION_CACHE_TTL_SECONDS` | 600 seconds |
| `TIKTOK_INFO_CACHE_TTL_SECONDS` | 60 seconds |
| `TELEGRAM_THUMBNAIL_WAIT_SECONDS` | 1.5 seconds |

The thumbnail budget starts alongside upstream setup. Set it to zero to skip
optional covers. These defaults work without editing an existing `.env`; explicit
environment overrides continue to apply. New settings are documented in
[`.env.example`](../.env.example) and [README.md](../README.md).

The delivery byte budget applies to copied/remuxed video and audio. Images may
shrink during conversion, so their final upload-size check remains authoritative.
Direct asset downloads keep their separate global size policy. The combined input
track budget is conservative, and the final output is checked because MP4 overhead
can increase its size. Oversized media is rejected earlier; this change does not
automatically lower video quality or bypass configured media proxies.

Validation performed:

- All 160 non-live tests passed, including the real local CONNECT/TLS reproduction.
- The TLS probe now resolves a healthy URL after two stalled handshakes. Its legacy
  control still exhausts a two-slot pool with sequential requests.
- Concurrent client tests verify that failed or cancelled requests do not close an
  unrelated active request, and a late failure cannot retire a newer client.
- A deterministic concurrent-extraction test went from eight provider calls for
  eight callers to one call. A subsequent caller can still retry.
- Event-based thumbnail tests confirm setup overlaps and failed video does not wait
  for a blocked cover.
- Size tests verify rejection before body consumption, shared budgets with and
  without length headers, retry reservation cleanup, and final remux-size checks.
- Ruff formatting/lint, mypy, and source-distribution/wheel builds passed.
- Docker validation could not run because access to the Docker socket was denied.

The standalone probe is:

```bash
uv run python scripts/diagnostics/repro_tiktok_proxy_pool.py
```

Add `--legacy` to demonstrate the original failure, which deliberately exits
nonzero. The probe uses a local proxy and an ephemeral test certificate generated
by `openssl`. It sends no traffic to TikTok or Telegram. Production media throughput
and latency have not been remeasured; compare resolution success, timeouts by proxy
slot, and delivery p50/p95 after rollout. Client-side retries must also respect a
total budget, particularly now that transient failures receive their correct codes.
