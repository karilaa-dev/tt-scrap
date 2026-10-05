from __future__ import annotations

import asyncio
import socket

import httpx
import pytest
from aiohttp import web
from pydantic import SecretStr
from uvicorn import Config

from tt_scrap.app import create_app


def test_uvicorn_auto_loop_starts_with_unmodified_native_dns(settings):
    uvloop = pytest.importorskip("uvloop")
    app = create_app(settings)
    config = Config(app, loop="auto")

    async def scenario():
        loop = asyncio.get_running_loop()
        assert isinstance(loop, uvloop.Loop)
        native_dns = loop.getaddrinfo
        async with app.router.lifespan_context(app):
            addresses = await asyncio.wait_for(
                loop.getaddrinfo("localhost", 443, family=socket.AF_INET, type=socket.SOCK_STREAM),
                2,
            )
            assert (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 443)) in addresses
            async with httpx.AsyncClient(
                transport=httpx.ASGITransport(app), base_url="http://test"
            ) as client:
                assert (await client.get("/health/live")).status_code == 200
                assert (await client.get("/health/ready")).status_code == 200
        assert loop.getaddrinfo == native_dns

    with asyncio.Runner(loop_factory=config.get_loop_factory()) as runner:
        runner.run(scenario())


@pytest.mark.parametrize("loop_name", ["asyncio", "uvloop"])
def test_clients_recover_from_stalled_shared_dns(settings, monkeypatch, loop_name):
    factory = (
        asyncio.new_event_loop
        if loop_name == "asyncio"
        else pytest.importorskip("uvloop").new_event_loop
    )

    async def scenario():
        received = []

        async def handler(request):
            received.append((request.method, request.path))
            return web.json_response({"ok": True})

        upstream = web.Application()
        upstream.router.add_route("*", "/{path:.*}", handler)
        runner = web.AppRunner(upstream)
        await runner.setup()
        site = web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]
        base_url = f"http://localhost:{port}"
        settings.telegram_api_base_url = base_url
        settings.telegram_bot_token = SecretStr("dns-test-token")
        settings.telegram_upload_timeout_seconds = 2.5
        stalled_calls = 0

        async def stalled_dns(*args, **kwargs):
            nonlocal stalled_calls
            stalled_calls += 1
            await asyncio.Future()

        loop = asyncio.get_running_loop()
        monkeypatch.setattr(loop, "getaddrinfo", stalled_dns)
        app = create_app(settings)
        try:
            async with app.router.lifespan_context(app):

                async def tiktok_request():
                    async with app.state.tiktok.adapter._clients.acquire(None) as client:
                        return (await client.get(f"{base_url}/tiktok", timeout=2.5)).json()

                async def instagram_request():
                    return (
                        await app.state.instagram._http.get(f"{base_url}/instagram", timeout=2.5)
                    ).json()

                results = await asyncio.gather(
                    tiktok_request(),
                    instagram_request(),
                    app.state.telegram_client.call("getMe", {}, []),
                    return_exceptions=True,
                )
                assert results[:2] == [{"ok": True}, {"ok": True}], results
                assert results[2].ok, results
                assert stalled_calls > 0, "The requests must exercise DNS, not numeric IPs"
                calls_before = stalled_calls
                # Force a new TCP connection while the native resolver remains stuck.
                async with app.state.tiktok.adapter._new_http_client(None) as fresh:
                    assert (await fresh.get(f"{base_url}/fresh", timeout=2.5)).json() == {
                        "ok": True
                    }
                assert stalled_calls == calls_before
            assert loop.getaddrinfo is stalled_dns
        finally:
            await runner.cleanup()
        assert sorted(received) == [
            ("GET", "/fresh"),
            ("GET", "/instagram"),
            ("GET", "/tiktok"),
            ("POST", "/botdns-test-token/getMe"),
        ]

    with asyncio.Runner(loop_factory=factory) as runner:
        runner.run(scenario())
