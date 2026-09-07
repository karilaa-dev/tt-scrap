"""Offline resolver regression probe. Default must succeed after two TLS failures.

--legacy disables application pool recovery to reproduce the original failure.
--recycle replaces the client before the healthy request.
--cleanup-tunnel applies an in-process diagnostic cleanup to httpcore only.
Neither mode changes application or dependency files.
"""

import argparse
import asyncio
import logging
import ssl
import subprocess
import tempfile
from contextlib import asynccontextmanager
from pathlib import Path
from time import perf_counter

import httpcore
import httpx

from tt_scrap.config import Settings
from tt_scrap.platforms.tiktok.adapter import TikTokAdapter
from tt_scrap.platforms.tiktok.http import ResolverClients
from tt_scrap.proxy import ProxyChoice, ProxyManager, ProxySession

logging.disable(logging.CRITICAL)
SHORT_URL = "https://vt.tiktok.com/LOCAL_DIAGNOSTIC/"
FULL_URL = "https://www.tiktok.com/@diagnostic/video/1234567890123456789"


async def main(args):
    if args.cleanup_tunnel:
        from httpcore._async.http_proxy import AsyncTunnelHTTPConnection

        original = AsyncTunnelHTTPConnection.handle_async_request

        async def with_cleanup(self, request):
            try:
                return await original(self, request)
            except BaseException:
                if not self._connected:
                    await self.aclose()
                raise

        AsyncTunnelHTTPConnection.handle_async_request = with_cleanup

    with tempfile.TemporaryDirectory(prefix="tt-scrap-local-tls-") as directory:
        cert, key = Path(directory) / "cert.pem", Path(directory) / "key.pem"
        await asyncio.to_thread(
            subprocess.run,
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-keyout",
                str(key),
                "-out",
                str(cert),
                "-days",
                "1",
                "-subj",
                "/CN=vt.tiktok.com",
                "-addext",
                "subjectAltName=DNS:vt.tiktok.com,DNS:www.tiktok.com",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        tls_server = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls_server.load_cert_chain(cert, key)
        tls_client = ssl.create_default_context(cafile=str(cert))
        handlers = set()
        connect_requests = 0

        async def proxy(reader, writer):
            nonlocal connect_requests
            task = asyncio.current_task()
            handlers.add(task)
            try:
                await reader.readuntil(b"\r\n\r\n")
                connect_requests += 1
                should_stall = connect_requests <= 2
                writer.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
                await writer.drain()
                if should_stall:
                    while await reader.read(4096):
                        pass
                    return
                await writer.start_tls(tls_server)
                while True:
                    request = await reader.readuntil(b"\r\n\r\n")
                    if b"LOCAL_DIAGNOSTIC" in request:
                        response = (
                            f"HTTP/1.1 302 Found\r\nLocation: {FULL_URL}\r\n"
                            "Content-Length: 0\r\n\r\n"
                        ).encode()
                    else:
                        response = b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                    writer.write(response)
                    await writer.drain()
            except (asyncio.IncompleteReadError, ConnectionError):
                pass
            finally:
                writer.close()
                handlers.discard(task)

        server = await asyncio.start_server(proxy, "127.0.0.1", 0)
        proxy_url = f"http://127.0.0.1:{server.sockets[0].getsockname()[1]}"
        settings = Settings(
            _env_file=None,
            tt_scrap_api_key="local-diagnostic-key",
            proxy_file="",
            ytdlp_cookies="",
            http_max_connections=2,
            url_resolve_max_retries=1,
        )
        manager = ProxyManager()
        adapter = TikTokAdapter(settings, manager)

        clients = []

        def new_client():
            client = httpx.AsyncClient(
                proxy=proxy_url,
                trust_env=False,
                verify=tls_client,
                timeout=httpx.Timeout(0.1),
                limits=httpx.Limits(max_connections=2),
            )

            clients.append(client)
            return client

        adapter._new_http_client = lambda proxy: new_client()
        if args.legacy:

            class LegacyClients:
                def __init__(self):
                    self.client = new_client()

                @asynccontextmanager
                async def acquire(self, proxy):
                    yield self.client

                async def close(self):
                    await self.client.aclose()

            adapter._clients = LegacyClients()
        failures = []
        resolved = None
        started = perf_counter()
        print(
            f"httpx={httpx.__version__} httpcore={httpcore.__version__}; "
            "concurrency=1; pool_capacity=2"
        )
        try:
            for i in range(3):
                if args.recycle and i == 2:
                    await adapter._clients.close()
                    adapter._clients = ResolverClients(lambda proxy: new_client())
                    print("Replaced saturated proxy client before request 3")
                session = ProxySession(manager, choice=ProxyChoice(slot=0, url=proxy_url))
                try:
                    resolved = await adapter.resolve_url(SHORT_URL, session)
                    print(f"request={i + 1} resolved=True")
                except Exception as exc:
                    cause = type(exc.__cause__).__name__
                    failures.append(cause)
                    pool = clients[-1]._transport_for_url(httpx.URL(SHORT_URL))._pool
                    states = [
                        (c.is_idle(), c.is_closed(), c.is_available()) for c in pool.connections
                    ]
                    print(
                        f"request={i + 1} error={type(exc).__name__} cause={cause} "
                        f"pool_entries={len(states)} states_idle_closed_available={states}"
                    )
            print(
                f"CONNECT requests received={connect_requests}; "
                f"elapsed_ms={(perf_counter() - started) * 1000:.1f}"
            )
        finally:
            await adapter.close()
            server.close()
            await server.wait_closed()
            pending = list(handlers)
            for task in pending:
                task.cancel()
            await asyncio.gather(*pending, return_exceptions=True)
        assert failures[:2] == ["ConnectTimeout", "ConnectTimeout"], failures
        assert resolved == FULL_URL, (
            "Healthy request failed after transient TLS failures poisoned the resolver pool"
        )
        print("PASS: healthy resolution succeeds after two transient TLS failures")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--legacy", action="store_true")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--recycle", action="store_true")
    mode.add_argument("--cleanup-tunnel", action="store_true")
    asyncio.run(main(parser.parse_args()))
