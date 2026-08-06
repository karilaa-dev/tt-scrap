from __future__ import annotations

import io
import json

import pytest
from aiohttp import web
from pydantic import SecretStr

from tt_scrap.telegram import TelegramClient, TelegramUpload


@pytest.mark.asyncio
async def test_client_streams_multipart_and_serializes_telegram_fields(settings) -> None:
    received: dict[str, bytes] = {}

    async def handler(request: web.Request) -> web.Response:
        reader = await request.multipart()
        while part := await reader.next():
            received[part.name] = await part.read()
        return web.json_response({"ok": True, "result": {"message_id": 1}})

    application = web.Application()
    application.router.add_post("/botruntime-secret/sendVideo", handler)
    runner = web.AppRunner(application)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    sockets = site._server.sockets  # type: ignore[union-attr]
    port = sockets[0].getsockname()[1]
    client = TelegramClient(
        settings.model_copy(
            update={
                "telegram_bot_token": SecretStr("runtime-secret"),
                "telegram_api_base_url": f"http://127.0.0.1:{port}",
            }
        )
    )
    try:
        response = await client.call(
            "sendVideo",
            {
                "chat_id": 123,
                "supports_streaming": True,
                "reply_markup": {"inline_keyboard": []},
                "video": "attach://video_file",
            },
            [TelegramUpload("video_file", io.BytesIO(b"video"), "video.mp4", "video/mp4")],
        )
    finally:
        await client.close()
        await runner.cleanup()

    assert response.ok
    assert received["chat_id"] == b"123"
    assert received["supports_streaming"] == b"true"
    assert json.loads(received["reply_markup"]) == {"inline_keyboard": []}
    assert received["video"] == b"attach://video_file"
    assert received["video_file"] == b"video"
