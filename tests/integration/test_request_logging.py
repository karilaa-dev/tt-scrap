from __future__ import annotations

import asyncio
import io
import json
import logging
import re
from unittest.mock import AsyncMock

import httpx
import pytest
import respx
from fastapi import Request
from PIL import Image
from pydantic import SecretStr
from starlette.responses import StreamingResponse

from tt_scrap.app import create_app
from tt_scrap.errors import TelegramTimeoutError
from tt_scrap.logging import (
    JsonFormatter,
    bind_request_context,
    record_recovery,
    request_id_var,
    request_log_var,
)
from tt_scrap.media import ImagePreparationService
from tt_scrap.telegram import TelegramCallResponse, TelegramDeliveryOutcome

API_URL = "https://instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com/convert"


@pytest.fixture
def logging_app(settings, monkeypatch):
    settings.telegram_bot_token = SecretStr("private-test-bot-token")
    monkeypatch.setattr(ImagePreparationService, "warm", AsyncMock())
    return create_app(settings)


@pytest.fixture
async def logging_client(logging_app, log_records):
    async with logging_app.router.lifespan_context(logging_app):
        log_records.clear()
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=logging_app, raise_app_exceptions=False),
            base_url="http://test",
            headers={
                "Authorization": "Bearer test-api-key-that-is-long-enough",
                "X-Request-ID": "request-log-test",
            },
        ) as client:
            yield client


@pytest.mark.parametrize("level", [logging.INFO, logging.DEBUG])
async def test_healthy_probes_are_debug_only(logging_client, log_records, caplog, level):
    with caplog.at_level(level, logger="tt_scrap"):
        for path in ("/health/live", "/health/ready"):
            response = await logging_client.get(path)
            assert response.status_code == 200
            assert response.headers["x-request-id"] == "request-log-test"
    assert len(log_records) == (2 if level == logging.DEBUG else 0)
    assert all(record.levelno == logging.DEBUG for record in log_records)


async def test_failed_readiness_remains_visible(logging_app, logging_client, log_records):
    logging_app.state.settings.rapidapi_key = SecretStr("")
    response = await logging_client.get("/health/ready")
    assert response.status_code == 503
    assert len(log_records) == 1
    record = log_records[0]
    assert record.levelno == logging.ERROR
    assert record.error_code == "service_not_configured"


@pytest.mark.parametrize("unauthorized", [False, True])
async def test_expected_errors_have_one_summary(logging_client, log_records, unauthorized):
    headers = {"Authorization": "Bearer wrong"} if unauthorized else {}
    response = await logging_client.post(
        "/v1/tiktok/extractions", headers=headers, json={"url": "invalid-url"}
    )
    assert response.status_code == (401 if unauthorized else 422)
    assert len(log_records) == 1
    record = log_records[0]
    assert record.event == "http.request.completed"
    assert record.levelno == logging.WARNING
    assert record.error_code == ("authentication_required" if unauthorized else "validation_error")
    assert record.platform == "tiktok"
    assert record.success is False


async def test_unexpected_error_keeps_correlation_and_timing(
    logging_app, logging_client, log_records
):
    @logging_app.get("/logging-probe")
    async def fail():
        raise RuntimeError("synthetic failure")

    response = await logging_client.get("/logging-probe")
    assert response.status_code == 500
    assert response.headers["x-request-id"] == response.json()["error"]["request_id"]
    assert response.headers["server-timing"].startswith("app;dur=")
    assert len(log_records) == 1
    record = log_records[0]
    assert record.event == "http.request.completed"
    assert record.levelno == logging.ERROR
    assert record.error_code == "internal_error"
    assert record.error_type == "RuntimeError"
    payload = json.loads(JsonFormatter().format(record))
    assert payload["request_id"] == "request-log-test"
    assert "RuntimeError: synthetic failure" in payload["exception"]
    assert request_log_var.get() is None
    assert request_id_var.get() == "-"


async def test_stream_failure_has_correlated_fallback(logging_app, logging_client, log_records):
    @logging_app.get("/logging-stream")
    async def stream():
        async def chunks():
            yield b"partial"
            raise RuntimeError("stream failed")

        return StreamingResponse(chunks())

    await logging_client.get("/logging-stream")
    failures = [record for record in log_records if record.levelno >= logging.ERROR]
    assert len(failures) == 1
    assert failures[0].event == "http.request.unhandled_error"
    assert failures[0].request_id == "request-log-test"
    assert failures[0].exc_info


async def test_concurrent_requests_share_only_their_own_child_task_counters(
    logging_app, logging_client, log_records
):
    both_started = asyncio.Event()
    started = 0

    @logging_app.get("/logging-concurrency/{source_id}")
    async def concurrent(source_id: str, request: Request):
        nonlocal started
        assert request.state.log_context is request_log_var.get()
        bind_request_context(platform="tiktok", source_id=source_id)
        started += 1
        if started == 2:
            both_started.set()
        await both_started.wait()

        async def child():
            await asyncio.sleep(0)
            record_recovery("retry_count")

        await asyncio.gather(*(child() for _ in range(int(source_id))))
        return {"ok": True}

    responses = await asyncio.wait_for(
        asyncio.gather(
            logging_client.get("/logging-concurrency/2", headers={"X-Request-ID": "first"}),
            logging_client.get("/logging-concurrency/3", headers={"X-Request-ID": "second"}),
        ),
        timeout=1,
    )
    assert all(response.status_code == 200 for response in responses)
    assert len(log_records) == 2
    by_id = {record.request_id: record for record in log_records}
    assert (by_id["first"].source_id, by_id["first"].retry_count) == ("2", 2)
    assert (by_id["second"].source_id, by_id["second"].retry_count) == ("3", 3)
    assert all(record.path == "/logging-concurrency/{source_id}" for record in log_records)
    assert request_log_var.get() is None


@respx.mock
@pytest.mark.parametrize("retry", [False, True])
async def test_extraction_and_cache_hit_each_emit_one_info_summary(
    logging_client, log_records, retry
):
    upstream = respx.get(API_URL).mock(
        side_effect=([httpx.Response(503)] if retry else [])
        + [
            httpx.Response(
                200, json={"media": [{"type": "image", "url": "https://cdn.test/photo"}]}
            )
        ],
    )
    for cache_hit in (False, True):
        log_records.clear()
        response = await logging_client.post(
            "/v1/instagram/extractions", json={"url": "https://www.instagram.com/p/ABC123/"}
        )
        assert response.status_code == 200
        assert [record.event for record in log_records] == [
            "http.request.completed",
        ]
        assert all(record.levelno == logging.INFO for record in log_records)
        assert log_records[-1].cache_hit is cache_hit
        assert log_records[-1].source_id == "ABC123"
        assert getattr(log_records[-1], "retry_count", 0) == int(retry and not cache_hit)
    assert upstream.call_count == (2 if retry else 1)


@respx.mock
async def test_exhausted_retries_emit_one_error_summary(logging_app, logging_client, log_records):
    upstream = respx.get(API_URL).respond(503)
    response = await logging_client.post(
        "/v1/instagram/extractions", json={"url": "https://www.instagram.com/p/ABC123/"}
    )
    assert response.status_code == 502
    assert len(log_records) == 1
    record = log_records[0]
    assert record.levelno == logging.ERROR
    assert record.error_code == "upstream_network_error"
    assert record.retry_count == upstream.call_count - 1
    assert upstream.call_count == logging_app.state.settings.instagram_max_attempts


@respx.mock
async def test_debug_retains_stage_details_without_request_secrets(
    logging_client, log_records, caplog
):
    respx.get(API_URL).respond(
        200, json={"media": [{"type": "image", "url": "https://cdn.test/private-signed-url"}]}
    )
    with caplog.at_level(logging.DEBUG, logger="tt_scrap"):
        response = await logging_client.post(
            "/v1/instagram/extractions", json={"url": "https://www.instagram.com/p/ABC123/"}
        )
    assert response.status_code == 200
    assert [record.event for record in log_records] == [
        "instagram.upstream.completed",
        "instagram.extraction.completed",
        "http.request.completed",
    ]
    assert log_records[0].levelno == logging.DEBUG
    output = "\n".join(JsonFormatter().format(record) for record in log_records)
    assert "private-signed-url" not in output
    assert "private-test-bot-token" not in output
    assert "test-api-key-that-is-long-enough" not in output
    assert all(record.request_id == "request-log-test" for record in log_records)


@pytest.mark.parametrize("status,partial", [(200, False), (429, False), (429, True)])
async def test_telegram_rejections_are_not_logged_as_success(
    logging_app, logging_client, log_records, monkeypatch, status, partial
):
    body = json.dumps(
        {
            "ok": False,
            "error_code": 429,
            "description": "try later",
            "parameters": {"retry_after": 5},
        }
    ).encode()
    failed = TelegramCallResponse("sendMediaGroup", status, body, "application/json")
    calls = [failed]
    if partial:
        calls.insert(
            0,
            TelegramCallResponse(
                "sendMediaGroup", 200, b'{"ok":true,"result":[]}', "application/json"
            ),
        )
    monkeypatch.setattr(
        logging_app.state.telegram_delivery,
        "deliver",
        AsyncMock(return_value=TelegramDeliveryOutcome(calls)),
    )
    response = await logging_client.post(
        "/v1/tiktok/telegram-deliveries",
        json={
            "source": {"url": "https://www.tiktok.com/@a/video/123"},
            "telegram": {"chat_id": 123},
            "delivery": "media",
        },
    )
    assert response.status_code == (207 if partial else status)
    if not partial:
        assert response.content == body
    assert len(log_records) == 1
    record = log_records[0]
    assert record.levelno == logging.WARNING
    assert record.success is False
    assert record.partial is partial
    assert record.telegram_error_code == 429
    assert record.telegram_description == "try later"
    assert record.telegram_retry_after == 5
    assert record.telegram_method == "sendMediaGroup"
    assert record.batch_index == (2 if partial else 1)


@respx.mock
@pytest.mark.parametrize("item_count", [1, 11, 21])
@pytest.mark.parametrize("failure", [None, "rejected", "timeout"])
async def test_delivery_info_volume_does_not_grow_with_album_size(
    logging_app, logging_client, log_records, monkeypatch, item_count, failure
):
    kind = "video" if item_count == 1 else "image"
    respx.get(API_URL).respond(
        200,
        json={
            "media": [
                {"type": kind, "url": f"https://cdn.test/media/{index}"}
                for index in range(item_count)
            ]
        },
    )
    image = io.BytesIO()
    Image.new("RGB", (16, 16)).save(image, format="JPEG")
    respx.get(re.compile(r"https://cdn\.test/media/\d+")).respond(
        200,
        content=image.getvalue() if kind == "image" else b"\x00\x00\x00\x18ftypisomvideo",
        headers={"Content-Type": "image/jpeg" if kind == "image" else "video/mp4"},
    )

    upload_calls = 0

    async def upload(method, fields, uploads):
        nonlocal upload_calls
        upload_calls += 1
        if failure and upload_calls == (1 if item_count == 1 else 2):
            if failure == "timeout":
                raise TelegramTimeoutError("Telegram upload timed out")
            return TelegramCallResponse(
                method,
                200,
                b'{"ok":false,"error_code":400,"description":"rejected"}',
                "application/json",
            )
        return TelegramCallResponse(method, 200, b'{"ok":true,"result":[]}', "application/json")

    monkeypatch.setattr(logging_app.state.telegram_client, "call", upload)
    response = await logging_client.post(
        "/v1/instagram/telegram-deliveries",
        json={
            "source": {"url": "https://www.instagram.com/p/ABC123/"},
            "telegram": {"chat_id": 123},
            "delivery": "media",
        },
    )
    if failure:
        assert response.status_code == (
            504 if failure == "timeout" else 207 if item_count > 1 else 200
        )
        assert [record.event for record in log_records] == [
            "http.request.completed",
        ]
        summary = log_records[-1]
        assert summary.levelno == (logging.ERROR if failure == "timeout" else logging.WARNING)
        assert summary.success is False
        assert summary.telegram_method == ("sendVideo" if item_count == 1 else "sendMediaGroup")
        if item_count > 1:
            assert summary.batch_index == 2
            assert summary.batch_count == (2 if item_count == 11 else 3)
        if failure == "rejected":
            assert summary.partial is (item_count > 1)
            assert summary.telegram_error_code == 400
        else:
            assert summary.error_code == "telegram_timeout"
            if item_count > 1:
                assert summary.partial is True
                assert summary.call_count == 1
        return
    assert response.status_code == 200
    assert [record.event for record in log_records] == [
        "http.request.completed",
    ]
    assert all(record.levelno == logging.INFO for record in log_records)
    summary = log_records[-1]
    assert summary.success is True
    assert summary.media_count == item_count
    assert summary.delivery == "media"
    assert summary.source_id == "ABC123"
    assert summary.call_count == {1: 1, 11: 2, 21: 3}[item_count]


async def test_body_parsing_failure_keeps_default_response_and_records_stage(
    logging_client, log_records
):
    response = await logging_client.post(
        "/v1/instagram/extractions", content=b"\xff", headers={"Content-Type": "application/json"}
    )
    assert response.status_code == 400
    assert response.json() == {"detail": "There was an error parsing the body"}
    assert len(log_records) == 1
    assert log_records[0].failure_stage == "http.body_parsing"
    assert log_records[0].failure_reason == "body_read_failed"
    assert log_records[0].upstream_error_type == "UnicodeDecodeError"


@respx.mock
async def test_upstream_failure_summary_is_sanitized(logging_client, log_records):
    respx.get(API_URL).respond(503, text="https://private-url.test/?secret=do-not-log")
    response = await logging_client.post(
        "/v1/instagram/extractions", json={"url": "https://www.instagram.com/p/failure/"}
    )
    assert response.status_code == 502
    summary = log_records[-1]
    assert summary.failure_stage == "instagram.upstream"
    assert summary.upstream_status_code == 503
    assert summary.failure_reason == "http_status"
    assert summary.extraction_queue_wait_ms >= 0
    assert "do-not-log" not in JsonFormatter().format(summary)


async def test_invalid_json_keeps_validation_response_and_records_stage(
    logging_client, log_records
):
    response = await logging_client.post(
        "/v1/tiktok/extractions", content=b"{", headers={"Content-Type": "application/json"}
    )
    assert response.status_code == 422
    assert response.json() == {
        "error": {
            "code": "validation_error",
            "message": "Request validation failed",
            "request_id": "request-log-test",
        }
    }
    assert len(log_records) == 1
    assert log_records[0].failure_stage == "http.body_parsing"
    assert log_records[0].failure_reason == "invalid_json"


@respx.mock
@pytest.mark.parametrize("status,ok", [(400, False), (200, False), (200, True)])
async def test_real_telegram_response_records_failure_context_without_changing_response(
    logging_app, logging_client, log_records, monkeypatch, status, ok
):
    from aiohttp import web

    respx.get(API_URL).respond(
        200, json={"media": [{"type": "image", "url": "https://cdn.test/photo"}]}
    )
    photo = io.BytesIO()
    Image.new("RGB", (16, 16)).save(photo, format="JPEG")
    respx.get("https://cdn.test/photo").respond(
        200, content=photo.getvalue(), headers={"Content-Type": "image/jpeg"}
    )
    body = (
        b'{"ok":true,"result":[]}'
        if ok
        else b'{"ok":false,"error_code":400,"description":"rejected"}'
    )
    calls = 0

    async def upload(request):
        nonlocal calls
        calls += 1
        reader = await request.multipart()
        while part := await reader.next():
            await part.read()
        return web.Response(status=status, body=body, content_type="application/json")

    application = web.Application()
    application.router.add_post("/botreview-test/sendPhoto", upload)
    runner = web.AppRunner(application, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    monkeypatch.setattr(logging_app.state.telegram_client, "_base_url", f"http://127.0.0.1:{port}")
    monkeypatch.setattr(logging_app.state.telegram_client, "_token", "review-test")
    try:
        response = await logging_client.post(
            "/v1/instagram/telegram-deliveries",
            json={
                "source": {"url": "https://www.instagram.com/p/ABC123/"},
                "telegram": {"chat_id": 123},
                "delivery": "media",
            },
        )
        assert response.status_code == status
        assert response.content == body
        assert calls == 1
        assert len(log_records) == 1
        summary = log_records[0]
        assert summary.event == "http.request.completed"
        assert summary.success is ok
        if ok:
            assert not hasattr(summary, "failure_stage")
        else:
            assert summary.levelno == logging.WARNING
            assert summary.failure_stage == "telegram.api_call"
            assert summary.upstream_status_code == status
            assert summary.failure_reason == "upstream_rejected"
            assert summary.telegram_error_code == 400
    finally:
        await runner.cleanup()
