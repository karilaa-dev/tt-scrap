Performance diagnosis from the September 6 live logs
==================================================

This is the original diagnosis of `e6074a9`. The follow-up implementation is
described in [performance improvements](performance-improvements.md). References
to code line numbers below refer to that original checkout.

The largest delay occurs while resolving TikTok short links, before metadata
extraction or video transfer. The local checkout reproduces a proxy connection-pool
leak that closely matches the live failures. Fix that first, then address oversized
videos and the smaller costs below.

Reviewed checkout: `e6074a9`, release 1.3.0. The lockfile and installed environment
both use HTTPX 0.28.1 and httpcore 1.0.9. The logs do not identify the deployed commit,
dependency versions, or effective configuration, so those still need comparison
with the live image. The initial investigation added a report and an offline diagnostic. Runtime fixes
were subsequently implemented locally; nothing has been deployed.

The [scraper attachment](/home/agent/.t3/userdata/attachments/451422ba-9ef1-4b8c-bafd-6a6959d1ad6c-af283113-051f-4e8e-9b8a-9fcfeedf67be-txt.txt)
covers 22:47:29.739–22:49:02.078 UTC. The
[bot attachment](/home/agent/.t3/userdata/attachments/451422ba-9ef1-4b8c-bafd-6a6959d1ad6c-63300323-26ed-4b6a-887b-84c3fbe6eeff-txt.txt)
covers 22:36:25.315–22:38:52.210 UTC. They have no shared request IDs and cannot be
combined into individual end-to-end traces. Counts below describe this short sample,
not long-term service rates. Requests can start before or finish after the excerpt.

| Observation in the scraper log | Count | Measured time |
| --- | ---: | --- |
| Short-link attempts failing with `PoolTimeout` | 107 | 15.002–15.367 s each |
| Resolution requests returning HTTP 400 | 33 of 49 completed resolution requests | 45.011–45.377 s |
| Successful short-link resolution on attempt 1 | 6 | 1.947–2.199 s |
| Successful short-link resolution on attempt 2 | 5 | 16.781–17.025 s |
| Successful short-link resolution on attempt 3 | 5 | 31.792–31.988 s |
| Successful metadata retrieval | 18 | Median 1.550 s, maximum 2.219 s |
| Completed video relays | 7 | 2.506–4.823 s, including overlapping delivery work |
| Successful Telegram API calls, all media/platforms in this excerpt | 11 | Median 0.529 s, maximum 2.359 s |

The bot separately reports 53 `Invalid or expired TikTok link` failures and one
private-content failure. It contains no stage timings sufficient to measure bot
queue latency. Cache markers alone are not an end-to-end success count.

**1. Repair the resolver's proxy pool lifecycle. Highest priority.**

Relevant code is [adapter.py:159](../src/tt_scrap/platforms/tiktok/adapter.py#L159),
where `_proxy_http` retains a client per proxy indefinitely. These pools are
separate from the curl sessions used to download TikTok media. A broken resolver
pool can therefore coexist with healthy video transfers and metadata retrieval.

All 107 failed resolution attempts are `PoolTimeout`, with `proxy_used=true`.
Their application semaphore wait is at most 0.013 ms. HTTPX defines this error
as waiting for a pool connection, which explains why the application queue metric
looks healthy while the request stalls. See the
[HTTPX timeout documentation](https://www.python-httpx.org/advanced/timeouts/).

The offline probe exercises the real `TikTokAdapter.resolve_url` and HTTPX proxy
transport against a local CONNECT proxy. It stalls two TLS handshakes, then serves
valid HTTPS redirects. With pool capacity two and only one request at a time:

```text
request 1: ConnectTimeout; one pool entry, unavailable and not closed
request 2: ConnectTimeout; two pool entries, unavailable and not closed
request 3: PoolTimeout; the now-healthy proxy receives no new CONNECT request
```

Closing failed tunnels in a diagnostic wrapper makes request 3 succeed and leaves
zero retained entries after each failure. Replacing the saturated client before
request 3 also makes it succeed. The mechanism is in
`httpcore._async.http_proxy.AsyncTunnelHTTPConnection`: a failed TLS upgrade can
leave its underlying CONNECT connection marked active. This is also described in
the upstream [httpcore report #921](https://github.com/encode/httpcore/discussions/921).

The defect is confirmed locally. It is the leading explanation for production,
but the excerpt starts with already-exhausted pools and does not show the original
TLS failures or per-proxy pool state. Genuine live overload is not fully excluded.
The long-lived pool was introduced by commit `7a26112`, making that change a useful
regression boundary; this is not proof of when the live incident started.

For a small mitigation, give each short-link resolution its own proxy client,
reuse that client across its redirect hops, and close it afterward. This trades
connection reuse for bounded lifetime. Restarting a live instance can temporarily
clear poisoned pools, but it also clears this service's in-memory extraction IDs
and asset tokens. Increasing `HTTP_MAX_CONNECTIONS` only postpones this leak.

For the durable fix, use a verified transport cleanup fix or replace the resolver
transport while preserving redirect validation. A dependency upgrade should pass
the reproduction before being considered a fix. If keeping shared pools, implement
per-proxy recovery with client leases/generations: stop assigning new work to a
retired client and drain existing users before closing it. An unconditional
`aclose()` in a concurrent request's error handler can abort unrelated requests.
The diagnostic cleanup wrapper is evidence, not a reviewed production patch.

**2. Bound resolution latency and preserve the real error. Highest priority.**

[adapter.py:165](../src/tt_scrap/platforms/tiktok/adapter.py#L165) sets
`Timeout(15, connect=5, read=10)`, leaving pool acquisition at 15 seconds.
The three-attempt loop rotates proxies without repairing a failed pool.
[adapter.py:208](../src/tt_scrap/platforms/tiktok/adapter.py#L208) then converts
every exhausted HTTP failure into `InvalidLinkError`, HTTP 400.

That explains the observed 15/30/45-second increments and the misleading bot
message. It also hides the distinction between an invalid user link and service
failure. Forty-five seconds is this failure pattern, not an overall request cap:
redirect hops, retries, and waits for the keyed lock can add more time.

Add explicit, configurable pool and total resolution budgets. A 0.5–1-second pool
wait and an 8–10-second total budget are starting points to validate against live
traffic, not measured optimal settings. Start the total deadline at the service
entry so it includes keyed-lock waits, semaphore waits, every hop, and retries.
Pair a short pool timeout with recovery; lowering it alone makes failures faster.

Return a transient service-capacity error for pool exhaustion, `UpstreamTimeoutError`
for upstream timeouts, and `NetworkError` for other transport failures. Reserve
`invalid_link` for actual URL/redirect validation problems. Classify upstream HTTP
statuses separately and avoid retrying permanent validation failures. Add per-proxy
cooldowns so new requests do not immediately revisit a known unhealthy client.
Review tt-bot's read-only request retries when changing these statuses so nested
retry loops do not multiply the total budget. Keep Telegram delivery retry rules
separate because an ambiguous delivery can already have sent a message.

**3. Apply delivery-size limits before downloading and remuxing. High priority.**

Scraper lines 224–235 show request `d2aebad3-5805-4859-a6c7-ba6186ca51d1` downloading
78,675,057 video bytes and 7,824,685 audio bytes. The tracks take 19.980 seconds,
remuxing takes 1.052 seconds, and the resulting 86,794,375-byte MP4 is rejected
locally by `telegram.api_call.rejected`. The request returns HTTP 413 after
21.100 seconds. Telegram never receives that upload.

The late check is in
[telegram/client.py:146](../src/tt_scrap/telegram/client.py#L146).
[downloader.py:835](../src/tt_scrap/media/downloader.py#L835) consumes the entire
spooled response before inspecting `Content-Length`. It only enforces
`MAX_ASSET_BYTES` during transfer, whose default is zero, and does not receive the
Telegram delivery budget. The relay path already performs a header-based asset
limit check, but separate tracks bypass relay.

Pass a request-specific delivery byte budget into preparation. Read known
`Content-Length` values before consuming bodies, cancel sibling track downloads
on rejection, and enforce a combined streaming budget for separate tracks plus
remux overhead. Keep an actual final-output check because headers and estimates
can be missing or inaccurate. Apply the Telegram limit only to Telegram delivery,
so direct asset downloads retain their own policy.

Better still, retain candidate sizes/bitrates in `VideoSource` and select the best
candidate that fits the delivery budget. Size estimates require verification;
retry a smaller suitable representation when the actual size exceeds the limit.
Simply disabling the upload limit does not solve this failure for a destination
that cannot accept the file.

**4. Offer a faster video-selection policy and reduce optional cover waits.**

[service.py:246](../src/tt_scrap/platforms/tiktok/service.py#L246) prioritizes maximum
pixel resolution and then quality score. It does not rank by transfer size or
whether remuxing is needed. The successful separate-track request
`3b964265-781e-433d-b044-b429b6320503` takes 13.300 seconds to download its tracks,
1.231 seconds to remux, and 0.568 seconds for the Telegram call. Its video track is
25,277,053 bytes. This is primarily transfer time. More FFmpeg workers would not
remove it; the measured remux queue wait is 0.009 ms.

Consider a delivery policy that favors a suitable muxed representation and smaller
files, with highest-quality mode still available. Compare formats for the same
post before claiming a speedup; the excerpt contains no alternate-format timings.
Existing relaying and concurrent track downloads already help and should be kept.

[telegram/service.py:446](../src/tt_scrap/telegram/service.py#L446) waits for a cover
after opening the video stream and before calling Telegram. Five relays hit the
1.5-second cover deadline and discard the cover. `TELEGRAM_THUMBNAIL_WAIT_SECONDS=0`
is an existing option to use Telegram-generated previews for relayed videos.
A smaller nonzero budget is another option. This can advance upload start by up
to the cover wait; the end-to-end saving can be smaller because work overlaps.

All TikTok transfers in the sample use proxies. A controlled comparison of
`PROXY_DATA_ONLY=true` can measure direct CDN transfer speed while retaining proxies
for resolution/metadata. That setting exists at
[downloader.py:183](../src/tt_scrap/media/downloader.py#L183). Its benefit and success
rate are unmeasured here; verify signed URLs work from the direct egress before
rolling it out. It does not repair the HTTPX resolver pool.

**5. Reduce repeat resolution work after recovery. Medium priority.**

[adapter.py:211](../src/tt_scrap/platforms/tiktok/adapter.py#L211) follows the short
link and then requests the canonical post. The existing successful redirect test
asserts both requests. Extraction later fetches metadata separately.

If `/resolutions` is intended only to identify a post, stop after a validated HTTPS
TikTok redirect containing a canonical video/photo ID and check content availability
during extraction. This removes one request in the normal short-to-canonical case.
It changes the current removed-post behavior, which has an explicit test, so revise
the endpoint contract deliberately and preserve host/scheme/hop validation. The
current code already avoids reading the final HTML body; there is no body download
optimization left to claim here.

The URL-to-ID mapping currently shares the 60-second metadata TTL at
[service.py:437](../src/tt_scrap/platforms/tiktok/service.py#L437). Give successful
resolution mappings their own longer, bounded TTL and normalize equivalent short
URLs. Preserve significant parameters. Metadata and signed media URLs can retain
their shorter freshness requirements. This reduces repeated proxy work for cacheable
links, but the sample does not establish how often the same short URL recurs.

**6. Explain normalization failures and expose pool health.**

Four extraction requests for source `7604110433594838292` retrieve metadata
successfully, then return HTTP 502. Their metadata calls consume about 6.28 seconds
in total across a 9.81-second interval. See scraper lines 178–180, 191–193,
200–202, and 236–238. These are distinct HTTP requests; the logs alone cannot say
whether they are automatic bot retries or separate user attempts.

The failure happens after metadata retrieval and before a successful extraction
response. Missing video/image assets in
[service.py:647](../src/tt_scrap/platforms/tiktok/service.py#L647) are plausible, but
the payload and reason are absent, so an exact normalization defect is unproven.
[app.py:239](../src/tt_scrap/app.py#L239) logs the generic exception class/code and
omits its reason.

Add a bounded reason code such as `missing_video_asset` or `empty_slideshow`, source
ID, and safe metadata-shape fields. Capture a sanitized failing fixture to write a
specific regression test. Share the outcome of concurrent same-key failures or use
a brief transient backoff so failed normalization does not repeatedly fetch the
same metadata. Do not cache a transport failure as a permanent invalid link.

For the primary incident, add proxy slot IDs, client generation, in-flight resolver
count, pool capacity/state, timeout phase, and redirect-hop count. The existing
`queue_wait_ms` measures the application semaphore, not HTTPX pool waiting. Record
effective nonsecret concurrency/timeouts, build SHA, and dependency versions at
startup. Capture a continuous interval from startup through the first TLS failure
and subsequent pool timeout. No proxy credentials, cookies, or signed URLs are
needed for these measurements.

Reproduction and validation
---------------------------

Run from the repository root with the existing environment. The diagnostic needs
`openssl` to create an ephemeral certificate and only contacts a local test proxy.
It does not read the project's `.env`, cookie file, or proxy file. It uses test
settings and closes its clients/server. The two TLS delays are 100 ms each.

```bash
.venv/bin/python scripts/diagnostics/repro_tiktok_proxy_pool.py --legacy
.venv/bin/python scripts/diagnostics/repro_tiktok_proxy_pool.py --recycle
.venv/bin/python scripts/diagnostics/repro_tiktok_proxy_pool.py --cleanup-tunnel
.venv/bin/pytest -q tests/unit/test_concurrency.py tests/unit/test_tiktok_adapter.py
```

Original diagnostic results: baseline fails with `ConnectTimeout, ConnectTimeout, PoolTimeout`;
both diagnostic recovery modes pass and resolve the healthy URL. The focused
existing suite passes all 16 tests. Its pool test mocks `_follow_tiktok_redirects`,
so it never reaches the failing tunnel lifecycle.

The recovery modes validate the mechanism under sequential calls, not safe concurrent
pool retirement. Before shipping, extend the regression to failures and cancellation
under concurrent calls, then verify against the production dependency versions.
After rollout, track resolution success, pool timeouts by proxy, resolution p50/p95,
and delivery latency separately. The first-attempt 1.95–2.20-second resolutions show
the available healthy-path baseline; they are not a promised production SLA.
