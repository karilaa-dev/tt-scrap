# TikTok operation protocol

This opt-in protocol gives tt-bot one **15-second total** user-operation budget and
at most **three attempts**, including queueing, identity waits, resolution,
extraction, media preparation, and retry backoff. Success returns immediately.
Existing clients without operation headers retain the existing JSON API and limits.
No Telegram UI behavior is implemented here.

## Requests and continuation

Supported POST paths: `/v1/tiktok/resolutions`, `/v1/tiktok/extractions`,
`/v1/tiktok/music`, `/v1/tiktok/telegram-deliveries`. Authentication and request JSON
are unchanged. Add:

```http
X-TT-Operation-Budget-Ms: 15000
X-TT-Operation-Attempt: 1
Accept: application/x-ndjson
```

Budget is an integer from 1 through 15000; attempt is 1 through 3, default 1.
Invalid or unsupported operation headers return the normal JSON 422 error before
streaming. A stream requires a budget header. Authentication errors remain JSON.
Use exactly `application/x-ndjson` to opt in; ordinary JSON remains available.

The bot starts a monotonic deadline **before its own queue**, subtracts elapsed
wall time before every call, and passes the remaining milliseconds (round down).
Do not make another call when none remain. Carry the last server attempt unchanged
into the next call; moving from resolution to extraction to delivery is not a retry.
Do not run calls concurrently using independent copies of the cursor. The server
starts one parent deadline at request admission; nested stages cannot extend it.
Time spent transmitting a request to the server is still governed by the bot's
local deadline. There is no server operation registry or durable idempotency key.
The headers are continuation information, not a cross-request transaction.

The backend owns automatic preparation retries. One shared controller grants at
most two retry transitions across all nested stages and assets. An attempt is a
**preparation retry epoch that resumes a failed stage**, not a replay of the whole
operation or a count of HTTP requests. Initial requests for other assets and
parallel byte ranges belong to the current epoch. Resolution, metadata, identity
recovery, source fallback and range resumption consume the same allowance when
retried. Optional thumbnail work gets no retries in this mode. Existing local
retry limits may stop earlier. The bot must not wrap these calls in another
three-attempt loop. If a call fails, its terminal event ends that operation; a user
pressing Try again starts a new operation only when duplicate-safe.

Header/body stalls are capped at three seconds per request in operation mode;
the shared parent deadline can expire sooner. Body progress can use the remaining
operation time, so a large progressing download need not be abandoned just to
show all three counters. Queueing or a long extraction can consume the budget
before another retry is possible. The bot must never synthesize attempt changes
from elapsed time.

## NDJSON events

HTTP status is 200 once streaming starts. Each complete UTF-8 JSON line is one
event, flushed immediately. Proxy response buffering is disabled by header;
reverse proxies must also permit streaming. Event ordering is serialized, including
parallel media workers. There is no heartbeat or elapsed-time counter.

```json
{"type":"attempt","attempt":1,"max_attempts":3,"stage":"admission"}
{"type":"attempt","attempt":2,"max_attempts":3,"stage":"media"}
{"type":"delivery_commit","attempt":2,"max_attempts":3,"stage":"upload"}
{"type":"terminal","attempt":2,"max_attempts":3,"status":200,"outcome":"succeeded","error_code":"","delivery_state":"confirmed","safe_to_retry":false,"result":{"ok":true,"result":{"message_id":1}}}
```

The first event reports the continued attempt. Subsequent `attempt` events occur
only at an actual retry boundary, after backoff, immediately before the work is
rerun. Stages currently include `admission`, `resolution`, `extraction`,
`identity_recovery`, `media`, and `media_range`; clients should tolerate new stages.
The bot only updates the attempt-counter line for strictly increasing attempts;
it retains the fixed localized loading text. A new call's admission event can
repeat the current number.

`delivery_commit` is emitted before a streamed upload's final-byte gate opens, or
before a fully prepared upload is invoked. A write/flush failure cancels work and
keeps a streamed gate shut. Several album batches can emit several commit events.
This notification is deliberately conservative: it indicates that delivery can
become irreversible, not that Telegram has accepted anything. Progress consumers
must keep reading promptly; blocked event writes are bounded and cancel work.

Exactly one terminal event is emitted on a working connection, after worker and
buffer cleanup. No progress follows it. Terminal `status` is the original JSON
HTTP status, and `result` is the original JSON body (or null if it was not JSON).
The separate `outcome` is `succeeded`, `failed`, `canceled`, `rejected`, or `partial`.
`error_code` is a stable API code, e.g. `upstream_timeout`, `request_canceled`, or
`telegram_network_error`; do not interpret raw message text as retry guidance.
Cancellation/timeout may prevent the terminal event from reaching the bot.

| delivery_state | Meaning | safe_to_retry on failure |
| --- | --- | --- |
| not_started | No Telegram send invoked | true |
| uncommitted | Stream started, final-byte gate never released | true |
| unknown | Commit permitted, no conclusive Telegram outcome | false |
| confirmed | Telegram acknowledged a successful send | false |
| rejected | Parsed Telegram ok:false with a 4xx error code and non-5xx HTTP status; no earlier success | true |
| partial | Some album batches succeeded before failure/rejection | false |

`safe_to_retry` describes **duplicate safety only**, not whether another attempt
will succeed or whether the destination still exists. Successful operations set
it false. A destination rejection can be duplicate-safe but terminal for the UI.

**An invoked delivery with no terminal event is always unknown**, even if the bot
never received `delivery_commit`. The event could have been lost while the send
continued. HTTP abort, timeout, or absence of the commit event does not prove a
send failed. Never automatically resend an unknown/partial delivery (including a
cache-chat storage send). There is no promise of rollback or exactly-once delivery.
Malformed Telegram replies and 5xx responses after commit remain unknown, even when a legacy delivery log labels the response rejected. The service never retries a Telegram send whose outcome may be successful.

## Ordinary JSON with a shared budget

Omit the NDJSON Accept header but keep the budget/attempt headers. The response body
and HTTP status remain unchanged. Additional headers report:

- `X-TT-Operation-Attempt`: final cursor for the next call.
- `X-TT-Delivery-State`: the state above.
- `X-TT-Safe-To-Retry`: `true` or `false` with the same duplicate-safety meaning.

These clients cannot display intermediate progress; use NDJSON when counters matter.
Missing outcome headers from older servers must never be interpreted as safe delivery.

## Bot responsibilities and boundaries

The bot owns the visible 15-second timer, same-message failure/Try again UI,
serialization of Telegram edits, and destination-loss classification. On an invalid
or gone inline destination, stop edits and cancel outstanding HTTP work. Do not
invalidate media caches or penalize identities on caller cancellation. User deletion
is one possible explanation for MESSAGE_ID_INVALID, not proof for every occurrence.
Do not offer a blind resend after ambiguous delivery just to satisfy the normal
failure-button flow; distinguish that outcome in the UI.

Cancellation reaches network requests, queues, identity leases, backoff and spool
readers. CPU-bound native decoding cannot be forcibly interrupted; the service joins
cleanup before returning. The bot's visible deadline must not wait for server cleanup.
A timed-out or disconnected request may therefore finish cleanup after its budget.

Wire examples live in `docs/fixtures/operation-*.ndjson`. Regression coverage is in
`internal/httpapi/operation_test.go`, `internal/core/operation_test.go`, and the
platform/identity timeout tests. All Telegram interactions in these tests are local
fixtures; they send no real messages.
