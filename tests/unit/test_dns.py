from __future__ import annotations

import asyncio
import socket
import threading
from concurrent.futures import ThreadPoolExecutor

import pytest

from tt_scrap.dns import DNSRecovery

ADDRESS = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 443))]


async def stalled_dns(*args, **kwargs):
    await asyncio.Future()


async def test_healthy_native_dns_preserves_arguments_and_does_not_use_fallback(monkeypatch):
    calls = []

    async def native(*args, **kwargs):
        calls.append((args, kwargs))
        return ADDRESS

    def unexpected_fallback(*args):
        pytest.fail("Healthy native DNS must not run in the fallback executor")

    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", unexpected_fallback)
    with DNSRecovery():
        assert (
            await loop.getaddrinfo(
                b"localhost",
                b"443",
                family=socket.AF_INET,
                type=socket.SOCK_STREAM,
                proto=6,
                flags=socket.AI_ADDRCONFIG,
            )
            == ADDRESS
        )
    assert loop.getaddrinfo is native
    assert calls == [
        (
            (b"localhost", b"443"),
            {
                "family": socket.AF_INET,
                "type": socket.SOCK_STREAM,
                "proto": 6,
                "flags": socket.AI_ADDRCONFIG,
            },
        )
    ]


async def test_native_dns_errors_are_not_retried(monkeypatch):
    error = socket.gaierror(socket.EAI_NONAME, "does not exist")

    async def native(*args, **kwargs):
        raise error

    def unexpected_fallback(*args):
        pytest.fail("An authoritative DNS error is not a stuck resolver")

    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", unexpected_fallback)
    with DNSRecovery():
        with pytest.raises(socket.gaierror) as caught:
            await loop.getaddrinfo("nonexistent.invalid", 443)
    assert caught.value is error


async def test_cancelling_native_lookup_does_not_start_fallback(monkeypatch):
    entered = asyncio.Event()

    async def native(*args, **kwargs):
        entered.set()
        await asyncio.Future()

    def unexpected_fallback(*args):
        pytest.fail("Request cancellation must not launch DNS recovery")

    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", unexpected_fallback)
    with DNSRecovery():
        task = asyncio.create_task(loop.getaddrinfo("localhost", 443))
        await entered.wait()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task


async def test_cancelled_callers_share_running_lookup_and_do_not_free_worker(monkeypatch):
    loop = asyncio.get_running_loop()
    entered = asyncio.Event()
    release = threading.Event()
    calls = []

    def lookup(*args):
        calls.append(args)
        loop.call_soon_threadsafe(entered.set)
        assert release.wait(5), "Test must release the DNS worker"
        return ADDRESS

    monkeypatch.setattr(loop, "getaddrinfo", stalled_dns)
    monkeypatch.setattr(socket, "getaddrinfo", lookup)
    with DNSRecovery(fallback_after=0.005, workers=1):
        tasks = [asyncio.create_task(loop.getaddrinfo("first.test", 443)) for _ in range(20)]
        try:
            await asyncio.wait_for(entered.wait(), 1)
            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)
            assert calls == [("first.test", 443, 0, 0, 0, 0)]
            same = asyncio.create_task(loop.getaddrinfo("first.test", 443))
            other = asyncio.create_task(loop.getaddrinfo("second.test", 443))
            tasks.extend([same, other])
            await asyncio.sleep(0)
            assert not same.done()
            assert not other.done()
            assert len(calls) == 1
            release.set()
            assert await asyncio.wait_for(asyncio.gather(same, other), 1) == [ADDRESS, ADDRESS]
            assert calls == [
                ("first.test", 443, 0, 0, 0, 0),
                ("second.test", 443, 0, 0, 0, 0),
            ]
        finally:
            release.set()
            await asyncio.gather(*tasks, return_exceptions=True)


async def test_fallback_dns_error_does_not_poison_later_lookup(monkeypatch):
    calls = 0
    error = socket.gaierror(socket.EAI_AGAIN, "temporary failure")

    def lookup(*args):
        nonlocal calls
        calls += 1
        if calls == 1:
            raise error
        return ADDRESS

    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "getaddrinfo", stalled_dns)
    monkeypatch.setattr(socket, "getaddrinfo", lookup)
    with DNSRecovery(fallback_after=0.005, workers=1):
        with pytest.raises(socket.gaierror) as caught:
            await loop.getaddrinfo("localhost", 443)
        assert caught.value is error
        assert await asyncio.wait_for(loop.getaddrinfo("localhost", 443), 1) == ADDRESS
    assert calls == 2


async def test_native_resolver_is_retried_after_cooldown(monkeypatch, log_records):
    calls = 0

    async def native(*args, **kwargs):
        nonlocal calls
        calls += 1
        if calls == 1:
            await asyncio.Future()
        return ADDRESS

    fallback_calls = []

    def lookup(*args):
        fallback_calls.append(args)
        return ADDRESS

    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", lookup)
    with DNSRecovery(fallback_after=0.005, cooldown=0):
        assert await loop.getaddrinfo("localhost", 443) == ADDRESS
        assert await loop.getaddrinfo("localhost", 443) == ADDRESS
        assert await loop.getaddrinfo("localhost", 443) == ADDRESS
    assert calls == 3
    assert len(fallback_calls) == 1
    assert [record.event for record in log_records] == [
        "runtime.dns.fallback_started",
        "runtime.dns.fallback_succeeded",
        "runtime.dns.native_recovered",
    ]


@pytest.mark.parametrize("probe_succeeds", [True, False])
async def test_expired_cooldown_allows_one_probe_while_burst_uses_fallback(
    monkeypatch, probe_succeeds
):
    loop = asyncio.get_running_loop()
    entered = asyncio.Event()
    release = asyncio.Event()
    native_calls = []

    async def native(host, *args, **kwargs):
        native_calls.append(host)
        if host == "initial.test":
            raise TimeoutError
        entered.set()
        await release.wait()
        if not probe_succeeds:
            raise TimeoutError
        return ADDRESS

    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", lambda *args: ADDRESS)
    with DNSRecovery(fallback_after=5, cooldown=0):
        assert await loop.getaddrinfo("initial.test", 443) == ADDRESS
        probe = asyncio.create_task(loop.getaddrinfo("probe.test", 443))
        burst = []
        try:
            await asyncio.wait_for(entered.wait(), 1)
            burst = [
                asyncio.create_task(loop.getaddrinfo(f"burst-{index}.test", 443))
                for index in range(20)
            ]
            await asyncio.sleep(0)
            assert native_calls == ["initial.test", "probe.test"]
            assert await asyncio.wait_for(asyncio.gather(*burst), 1) == [ADDRESS] * 20
            assert not probe.done(), "Other callers must not wait for the native probe"
            release.set()
            assert await asyncio.wait_for(probe, 1) == ADDRESS
        finally:
            release.set()
            await asyncio.gather(probe, *burst, return_exceptions=True)


@pytest.mark.parametrize("outcome", ["cancel", "dns_error"])
async def test_interrupted_native_probe_allows_later_recovery(monkeypatch, outcome):
    loop = asyncio.get_running_loop()
    entered = asyncio.Event()
    release = asyncio.Event()
    native_calls = []
    error = socket.gaierror(socket.EAI_NONAME, "does not exist")

    async def native(host, *args, **kwargs):
        native_calls.append(host)
        if host == "initial.test":
            raise TimeoutError
        if host == "probe.test":
            entered.set()
            await release.wait()
            raise error
        return ADDRESS

    monkeypatch.setattr(loop, "getaddrinfo", native)
    monkeypatch.setattr(socket, "getaddrinfo", lambda *args: ADDRESS)
    with DNSRecovery(fallback_after=5, cooldown=0):
        assert await loop.getaddrinfo("initial.test", 443) == ADDRESS
        probe = asyncio.create_task(loop.getaddrinfo("probe.test", 443))
        try:
            await asyncio.wait_for(entered.wait(), 1)
            if outcome == "cancel":
                probe.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await probe
            else:
                release.set()
                with pytest.raises(socket.gaierror) as caught:
                    await probe
                assert caught.value is error
            assert await asyncio.wait_for(loop.getaddrinfo("recovered.test", 443), 1) == ADDRESS
            assert native_calls == ["initial.test", "probe.test", "recovered.test"]
        finally:
            release.set()
            await asyncio.gather(probe, return_exceptions=True)


def test_real_dns_resolves_while_default_executor_is_occupied():
    async def scenario():
        loop = asyncio.get_running_loop()
        loop.set_default_executor(ThreadPoolExecutor(max_workers=1))
        entered = asyncio.Event()
        release = threading.Event()

        def blocked_io():
            loop.call_soon_threadsafe(entered.set)
            assert release.wait(5), "Test must release the default executor"

        worker = loop.run_in_executor(None, blocked_io)
        try:
            await entered.wait()
            with DNSRecovery(fallback_after=0.005):
                addresses = await asyncio.wait_for(
                    loop.getaddrinfo(
                        "localhost", 443, family=socket.AF_INET, type=socket.SOCK_STREAM
                    ),
                    1,
                )
                assert (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 443)) in addresses
                assert not worker.done()
        finally:
            release.set()
            await worker

    asyncio.run(scenario())
