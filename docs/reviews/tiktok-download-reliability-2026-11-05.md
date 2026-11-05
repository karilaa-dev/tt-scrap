# TikTok download reliability implementation report

## Evidence and scope

The worktree and fetched main branch started at `91b9aec`. The shared-deadline
weakness described in the production investigation was still present; relevant
range-integrity, cleanup, identity-wait isolation and Telegram final-byte-gate
protections already existed and were retained. No repository AGENTS.md was present
in the checkout or parent chain; the supplied access restrictions were followed.

The supplied investigation found 122 preparation-stage upstream timeouts, before
Telegram attempts, at roughly the 60-second download limit. This supports fixing
budget starvation by a stalled source, but does not establish which network phase
caused the production failures. The concentrated identity failures on one route are
not proof that route caused the timeouts. No production inspection, account
changes, proxy edits, restarts, deployment, release or real Telegram sends were
performed in this task. All network/database testing used local controlled fixtures.

## Changes

- Each media request now has a header-wait limit and a cancellable body-read stall
  limit. Defaults are ten seconds each within the existing 60-second transfer
  limit, not a longer global timeout. Limits cover identity-backed and anonymous
  downloads and every parallel range request. A body making progress retains the
  total budget; local hashing/spooling/validation between reads is not charged as
  a network stall. Request contexts/timer callbacks are stopped and joined; failed
  buffers close before source fallback. Range retries retain the original strong
  validator, final address and offsets.
- Explicit stall causes survive identity batch accounting and cool down the route
  when the operation fails, without penalizing the identity session. Caller
  cancellation and the total budget remain neutral. Successful fallback clears
  operation failure as before. A canceled optional request cannot mask a later
  real failure. Per-hop transport attribution follows app-to-CDN redirects; direct
  CDN network failures do not penalize a metadata proxy. Existing content-specific
  restrictions/recovery behavior remains covered by the identity suite.
- Diagnostics use request/transfer IDs, source indices, attempt numbers, fixed
  phase names, status codes, durations and anonymous numeric route IDs. Header
  failures distinguish connection-pool wait, DNS, connect, TLS and response
  headers via httptrace. Body and media-validation failures have separate events.
  Retry/failure events are Info/Warn; successful admissions, route choices and
  header timing are Debug. No URLs, headers, credentials, cookies, tokens, media
  contents or Telegram payloads are added to these diagnostics. A regression test
  supplies signed URL/auth/cookie/error-string secrets and checks they are absent.
- The agreed opt-in bot protocol enforces a maximum 15-second remaining operation
  budget starting at HTTP admission, including pipeline/download/identity waits,
  extraction, preparation and retry delays. A central controller owns at most
  three preparation retry epochs across nested stages and successive calls. The
  bot passes decreasing budget and the continued attempt cursor; there is no
  nested three-by-three retry multiplier. Retry events are emitted only at real
  retry boundaries, after backoff; no elapsed-time counter exists. Optional
  thumbnail retries are suppressed. Header/body stalls are capped at three seconds
  in this mode. Success returns immediately; allowance exhaustion avoids needless
  backoff. Legacy JSON clients retain their bodies, status codes and total limits.
- NDJSON emits a commit notification before permitting a complete Telegram send.
  Failed progress writes cancel work and keep a streamed upload gate closed.
  Terminal outcomes distinguish uncommitted, confirmed, rejected, partial and
  unknown sends. Malformed/5xx Telegram replies after commit are unknown, not safe
  rejections. A complete upload followed by a lost response is never resent.
  Missing terminal events always mean unknown for an invoked delivery, even if
  the client did not observe a commit event.

The exact request headers, event schemas, continuation rules and bot-side UI
responsibilities are in [the protocol](../tiktok-operation.md). Shared NDJSON
fixtures were coordinated directly with the companion tt-bot implementation.
This checkout contains no tt-bot edits.

## Verification

The initial controlled header/body/partial-body stall tests failed on the old
transfer path: the total budget expired and the working alternate received zero
requests. With the guards, the alternate completes and failed spool memory is
released. Further tests cover:

- Slow progressing reads surviving beyond the stall interval, while the total
  deadline still cuts off prolonged downloads; caller cancellation makes no
  alternate request.
- A partially received range stalling and resuming exactly at its last byte with
  the pinned ETag; existing changed-representation, redirect-target, checksum,
  bounded-concurrency, disk-spool and cleanup regressions remain passing.
- Identity header/body stalls, successful fallback, caller/total deadline
  neutrality, direct CDN neutrality and redirect attribution.
- Exact synthetic-clock 15-second limits across queue/backoff and two sequential
  calls; continued attempt cursor, bounded concurrent retry ownership, and no
  progress or commit after terminal state.
- Ordered real media retries and commit/terminal events; unchanged ordinary JSON;
  invalid operation headers; pipeline/identity wait cancellation.
- Streamed failed preparation followed by exactly one completed local multipart
  upload; safe pre-commit timeout; ambiguous complete upload with no resend;
  malformed/5xx reply ambiguity; disconnect at commit with no complete upload.

Commands run successfully:

```text
make check
  formatting, go mod tidy -diff, go vet ./...
  go test -race -count=1 ./...
  CGO production build
make lint
  staticcheck v0.8.2
  govulncheck v1.9.0
```

Database tests ran against a disposable PostgreSQL 18 container on localhost,
using isolated test schemas. They did not silently skip for lack of a database.
The vulnerability scan found no called vulnerabilities (it noted three findings
in required modules that this code does not call). Live/account/Telegram tests
requiring explicit live flags were not enabled.

## Remaining uncertainties and operational tradeoffs

- Local fixtures prove recovery/control-flow safety, not the production network
  root cause or the real-world incident reduction. No live proxy/CDN benchmarking
  or post-deployment claims are made.
- Ten-second normal stall limits and the three-second operation cap are policy
  choices. A path that makes no observable read progress for longer is retried;
  deployment telemetry should inform tuning. Header timeout covers the entire
  open operation, including redirects/bootstrap, and trace phases are coarse
  diagnostics rather than a full packet timeline.
- Fifteen seconds cannot guarantee three attempts or successful large downloads.
  Queueing/extraction or a progressing transfer may consume the whole allowance.
  Native CPU-bound decoding is joined for cleanup and may finish after context
  expiration; the bot must enforce its own visible timer independently.
- Continuation is stateless. The bot must carry budget/cursor across sequential
  calls and avoid parallel forks/resetting it. There is no durable delivery
  idempotency record or reconciliation endpoint; missing terminal/unknown/partial
  outcomes cannot safely authorize a blind resend.
- Reverse proxies must allow flushing NDJSON. Event writes have a bounded deadline
  and cancel work if the reader stalls. A lost commit notification cannot be used
  as proof of rollback.
- MESSAGE_ID_INVALID is a bot destination issue; user deletion is context, not a
  universal diagnosis. Same-message text, counter edits, Try again UI and stopping
  edits after destination loss belong to the companion tt-bot change.
