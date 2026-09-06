from __future__ import annotations

import asyncio

import httpx
import pytest

from tt_scrap.platforms.tiktok.http import ResolverClients


@pytest.mark.asyncio
async def test_failed_client_drains_active_user_while_new_requests_use_fresh_pool() -> None:
    created: list[httpx.AsyncClient] = []

    def factory(proxy):
        client = httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(200)))
        created.append(client)
        return client

    clients = ResolverClients(factory)
    try:
        async with clients.acquire("proxy-a") as active:
            with pytest.raises(httpx.ConnectTimeout):
                async with clients.acquire("proxy-a") as failed:
                    assert failed is active
                    raise httpx.ConnectTimeout("TLS failed")
            assert not active.is_closed
            async with clients.acquire("proxy-a") as fresh:
                assert fresh is not active
                assert (await fresh.get("https://test/")).status_code == 200
            assert (await active.get("https://test/")).status_code == 200
        assert active.is_closed
        async with clients.acquire("proxy-a") as reused:
            assert reused is fresh
        assert len(created) == 2
    finally:
        await clients.close()
    assert all(client.is_closed for client in created)


@pytest.mark.asyncio
async def test_cancellation_retires_client_without_closing_other_proxy() -> None:
    clients = ResolverClients(lambda proxy: httpx.AsyncClient())
    entered = asyncio.Event()
    cancelled_client = None

    async def request():
        nonlocal cancelled_client
        async with clients.acquire("proxy-a") as client:
            cancelled_client = client
            entered.set()
            await asyncio.Future()

    try:
        async with clients.acquire("proxy-b") as other:
            task = asyncio.create_task(request())
            await entered.wait()
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
            assert cancelled_client.is_closed
            assert not other.is_closed
            async with clients.acquire("proxy-a") as fresh:
                assert fresh is not cancelled_client
    finally:
        await clients.close()


@pytest.mark.asyncio
async def test_old_failure_does_not_retire_new_generation() -> None:
    clients = ResolverClients(lambda proxy: httpx.AsyncClient())
    try:
        with pytest.raises(httpx.ReadError):
            async with clients.acquire(None) as old:
                with pytest.raises(httpx.ConnectError):
                    async with clients.acquire(None):
                        raise httpx.ConnectError("first failure")
                async with clients.acquire(None) as fresh:
                    assert fresh is not old
                raise httpx.ReadError("late failure from old generation")
        async with clients.acquire(None) as current:
            assert current is fresh
    finally:
        await clients.close()
