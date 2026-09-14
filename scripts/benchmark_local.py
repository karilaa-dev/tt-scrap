"""Reproducible local workload; no credentials or external services are used.

Compare two source trees in alternating fresh processes:
  python scripts/benchmark_local.py --baseline-root /path/to/baseline --runs 5
The existing benchmark.py remains available for authenticated live cached extraction.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import io
import json
import logging
import os
import platform
import random
import resource
import statistics
import subprocess
import sys
import tempfile
import time
from collections import Counter, defaultdict
from datetime import UTC, datetime, timedelta
from pathlib import Path
from urllib.parse import urlparse


def distribution(values):
    ordered = sorted(values)
    return (
        {
            label: round(ordered[max(0, int(len(ordered) * percentile) - 1)], 3)
            for label, percentile in [("p50_ms", 0.5), ("p95_ms", 0.95), ("p99_ms", 0.99)]
        }
        if ordered
        else {}
    )


def disk_writes():
    return int(
        dict(line.split(": ") for line in Path("/proc/self/io").read_text().splitlines())[
            "write_bytes"
        ]
    )


async def worker(args):
    # Select code before importing the application, including spawned image workers.
    sys.path.insert(0, os.path.join(args.source_root, "src"))
    import httpx
    from aiohttp import web
    from PIL import Image

    from tt_scrap.app import create_app
    from tt_scrap.assets import AssetFactory
    from tt_scrap.cache import CacheStore
    from tt_scrap.config import Settings
    from tt_scrap.errors import NetworkError, UpstreamTimeoutError
    from tt_scrap.media import AssetDownloader
    from tt_scrap.models import (
        AssetFetchContext,
        AuxiliaryAssetFetchContext,
        TikTokExtractionResponse,
    )
    from tt_scrap.platforms.instagram import InstagramService
    from tt_scrap.platforms.tiktok.http import ResolverClients
    from tt_scrap.platforms.tiktok.service import TikTokService
    from tt_scrap.proxy import ProxyManager
    from tt_scrap.telegram import TelegramUpload

    settings = Settings(
        _env_file=None,
        tt_scrap_api_key="local-benchmark-api-key",
        rapidapi_key="local-provider",
        telegram_bot_token="local",
        proxy_file="",
        log_level="INFO",
        download_retry_base_delay=0,
        instagram_retry_delay_seconds=0,
        image_conversion_workers=2,
    )
    payloads = {"document": b"document-bytes" * (320 * 1024), "disk": b"disk-bytes" * (1800 * 1024)}
    for kind in ("JPEG", "BMP"):
        output = io.BytesIO()
        Image.frombytes("L", (512, 512), random.Random(0).randbytes(512 * 512)).convert("RGB").save(
            output, format=kind
        )
        payloads[kind.lower()] = output.getvalue()
    with tempfile.TemporaryDirectory() as directory:
        for name, inputs, codec in [
            (
                "video",
                ["-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25"],
                ["-c:v", "libx264", "-preset", "ultrafast", "-threads", "1"],
            ),
            (
                "audio",
                ["-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100"],
                ["-c:a", "aac"],
            ),
        ]:
            path = Path(directory) / f"{name}.mp4"
            await asyncio.to_thread(
                subprocess.run,
                [
                    "ffmpeg",
                    "-nostdin",
                    "-loglevel",
                    "error",
                    *inputs,
                    "-t",
                    "2",
                    *codec,
                    "-y",
                    str(path),
                ],
                check=True,
            )
            payloads[name] = await asyncio.to_thread(path.read_bytes)
    received = Counter()
    expected_hashes = {hashlib.sha256(value).hexdigest() for value in payloads.values()}
    spool_io = Counter()
    original_rollover = tempfile.SpooledTemporaryFile.rollover
    original_write = tempfile.SpooledTemporaryFile.write

    def rollover(spool):
        if not spool._rolled:
            spool_io["rollovers"] += 1
            spool_io["write_bytes"] += spool._file.getbuffer().nbytes
        return original_rollover(spool)

    def write(spool, data):
        if spool._rolled:
            spool_io["write_bytes"] += len(data)
        return original_write(spool, data)

    # Keep the exact spool type so the candidate's memory fast path stays enabled.
    tempfile.SpooledTemporaryFile.rollover = rollover
    tempfile.SpooledTemporaryFile.write = write
    slow_delay = 0.12

    async def cdn(request):
        name = request.match_info["name"]
        await asyncio.sleep(slow_delay if name == "slow" else 0.002)
        if name == "missing":
            return web.Response(status=404)
        data = payloads["jpeg" if name == "slow" else name]
        content_type = {
            "jpeg": "image/jpeg",
            "bmp": "image/bmp",
            "video": "video/mp4",
            "audio": "audio/mp4",
        }.get(name, "application/octet-stream")
        if name in {"document", "disk"}:
            response = web.StreamResponse(headers={"Content-Type": content_type})
            await response.prepare(request)
            for offset in range(0, len(data), 256 * 1024):
                await response.write(data[offset : offset + 256 * 1024])
            await response.write_eof()
            return response
        return web.Response(body=data, content_type=content_type)

    async def telegram(request):
        reader = await request.multipart()
        while part := await reader.next():
            if part.filename:
                digest, prefix = hashlib.sha256(), bytearray()
                while chunk := await part.read_chunk(256 * 1024):
                    digest.update(chunk)
                    if len(prefix) < 32:
                        prefix.extend(chunk[:32])
                # Conversion is validated as JPEG; unchanged media must match exactly.
                assert digest.hexdigest() in expected_hashes or prefix.startswith(b"\xff\xd8\xff")
                received["files"] += 1
            else:
                await part.read()
        received["calls"] += 1
        return web.Response(body=b'{"ok":true,"result":[]}', content_type="application/json")

    upstream = web.Application()
    upstream.router.add_get("/cdn/{name}", cdn)
    upstream.router.add_post("/botlocal/{method}", telegram)
    runner = web.AppRunner(upstream, access_log=None)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    base = f"http://127.0.0.1:{site._server.sockets[0].getsockname()[1]}"
    settings.telegram_api_base_url = base
    app = create_app(settings)
    # Both versions write production INFO logs to the same local null device.
    # Stalled-output behavior is measured independently by regression tests.
    for handler in logging.getLogger().handlers:
        sink = getattr(handler, "sink", handler)
        sink.setStream(await asyncio.to_thread(open, os.devnull, "w"))
    provider_calls = Counter()

    async def provider(request):
        slug = urlparse(request.url.params["url"]).path.strip("/").split("/")[-1]
        kind = slug.split("_")[0]
        provider_calls[kind] += 1
        await asyncio.sleep(0.006)
        if kind == "invalid":
            return httpx.Response(404)
        names = {
            "album": ["jpeg", "bmp", "jpeg", "jpeg", "jpeg", "jpeg", "jpeg", "jpeg"],
            "relay": ["video"],
            "document": ["document"],
            "disk": ["disk"],
            "failedalbum": ["missing", "slow", "slow", "slow"],
            "slow": ["slow"],
        }.get(kind, ["jpeg"])
        return httpx.Response(
            200,
            json={
                "media": [
                    {
                        "type": "video" if name in {"video", "document", "disk"} else "image",
                        "url": f"{base}/cdn/{name}",
                    }
                    for name in names
                ]
            },
        )

    async with app.router.lifespan_context(app):
        await app.state.image_preparation.warm()
        await app.state.instagram._http.aclose()
        app.state.instagram._http = httpx.AsyncClient(transport=httpx.MockTransport(provider))
        async with httpx.AsyncClient(
            transport=httpx.ASGITransport(app=app, raise_app_exceptions=False),
            base_url="http://benchmark",
            headers={"Authorization": "Bearer local-benchmark-api-key"},
        ) as api:
            serial = 0

            async def operation(kind):
                nonlocal serial
                serial += 1
                if kind == "relay":
                    async with app.state.asset_downloader.stream(
                        AssetFetchContext(
                            platform="instagram",
                            upstream_url=f"{base}/cdn/video",
                            filename="video.mp4",
                            kind="video",
                        )
                    ) as streamed:
                        assert streamed is not None
                        response = await app.state.telegram_client.call(
                            "sendVideo",
                            {"chat_id": 123},
                            [
                                TelegramUpload(
                                    "video",
                                    streamed.chunks,
                                    "video.mp4",
                                    "video/mp4",
                                    streamed.size,
                                ),
                            ],
                        )
                        assert response.ok
                        return response.status_code
                if kind == "slideshow":
                    identity = f"slideshow-{serial}"
                    expires = datetime.now(UTC) + timedelta(minutes=1)
                    assets = AssetFactory(app.state.cache)
                    media = [
                        await assets.create(
                            AssetFetchContext(
                                platform="tiktok",
                                upstream_url=f"{base}/cdn/{name}",
                                filename=f"image-{index}.jpg",
                                kind="image",
                                extraction_id=identity,
                            ),
                            position=index,
                            expires_at=expires,
                        )
                        for index, name in enumerate(["jpeg", "bmp", "jpeg", "jpeg"])
                    ]
                    extraction = TikTokExtractionResponse(
                        extraction_id=identity,
                        source_id="123",
                        source_url="https://www.tiktok.com/@a/photo/123",
                        resolved_url="https://www.tiktok.com/@a/photo/123",
                        content_type="slideshow",
                        media=media,
                        expires_at=expires,
                    )
                    await app.state.cache.set_model(
                        app.state.cache.metadata_key("tiktok-extraction", identity), extraction
                    )
                    response = await api.post(
                        "/v1/tiktok/telegram-deliveries",
                        json={
                            "source": {"extraction_id": identity},
                            "telegram": {"chat_id": 123},
                            "delivery": "media",
                        },
                    )
                    return response.status_code
                if kind == "remux":
                    asset = await app.state.asset_downloader.download(
                        AssetFetchContext(
                            platform="instagram",
                            upstream_url=f"{base}/cdn/video",
                            filename="video.mp4",
                            kind="video",
                            audio=AuxiliaryAssetFetchContext(upstream_url=f"{base}/cdn/audio"),
                        )
                    )
                    try:
                        data = await asyncio.to_thread(asset.file.read)
                        assert hashlib.sha256(data).hexdigest() == asset.sha256
                        assert data[4:8] == b"ftyp"
                    finally:
                        await asyncio.to_thread(asset.file.close)
                    return 200
                slug = "cached" if kind == "cached" else f"{kind}_{serial}"
                source = f"https://www.instagram.com/p/{slug}/"
                if kind in {"cached", "uncached", "invalid"}:
                    response = await api.post("/v1/instagram/extractions", json={"url": source})
                else:
                    response = await api.post(
                        "/v1/instagram/telegram-deliveries",
                        json={
                            "source": {"url": source},
                            "telegram": {"chat_id": 123},
                            "delivery": "document" if kind in {"document", "disk"} else "media",
                        },
                    )
                return response.status_code

            async def measure(kinds, count, concurrency):
                latencies, lag, statuses = defaultdict(list), [], Counter()
                semaphore = asyncio.Semaphore(concurrency)
                done = False

                async def ticker():
                    while not done:
                        started = time.perf_counter()
                        await asyncio.sleep(0.005)
                        lag.append(max(0, time.perf_counter() - started - 0.005) * 1000)

                async def one(index):
                    kind = kinds[index % len(kinds)]
                    async with semaphore:
                        started = time.perf_counter()
                        try:
                            status = await operation(kind)
                            statuses[str(status)] += 1
                        except Exception as exc:
                            statuses[type(exc).__name__] += 1
                        latencies[kind].append((time.perf_counter() - started) * 1000)

                before = disk_writes()
                spool_before = spool_io.copy()
                started = time.perf_counter()
                ticker_task = asyncio.create_task(ticker())
                await asyncio.gather(*(one(index) for index in range(count)))
                seconds = time.perf_counter() - started
                done = True
                await ticker_task
                values = [value for group in latencies.values() for value in group]
                return {
                    **distribution(values),
                    "requests": count,
                    "throughput_rps": round(count / seconds, 3),
                    "statuses": dict(statuses),
                    "by_scenario": {key: distribution(group) for key, group in latencies.items()},
                    "event_loop_lag": {
                        **distribution(lag),
                        "max_ms": round(max(lag, default=0), 3),
                    },
                    "disk_write_bytes": disk_writes() - before,
                    "spool_write_bytes": spool_io["write_bytes"] - spool_before["write_bytes"],
                    "spool_rollovers": spool_io["rollovers"] - spool_before["rollovers"],
                    "peak_rss_mib": round(
                        resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 3
                    ),
                }

            kinds = [
                "cached",
                "uncached",
                "relay",
                "album",
                "document",
                "disk",
                "remux",
                "slideshow",
            ]
            for kind in dict.fromkeys(kinds):
                await operation(kind)
            healthy = await measure(kinds, args.requests, args.concurrency)
            mixed = await measure(
                [*kinds, "invalid", "slow", "failedalbum"], args.requests, args.concurrency * 2
            )

    # Isolated fairness probe at deliberately small configured caps (2 / 1).
    small = settings.model_copy(update={"download_concurrency": 2, "slideshow_concurrency": 1})
    downloader = AssetDownloader(small, ProxyManager())
    album_started = asyncio.Event()

    async def transfer(context, proxy, spool, upstream_url, **kwargs):
        if context.extraction_id == "album":
            album_started.set()
            await asyncio.sleep(0.1)
        spool.write(b"data")
        return "image/jpeg", 4, None, 4, b"data"

    downloader._download_once = transfer

    def context(group):
        return AssetFetchContext(
            platform="instagram",
            upstream_url="https://cdn.test/image",
            filename="image.jpg",
            kind="image",
            extraction_id=group,
        )

    tasks = [asyncio.create_task(downloader.download(context("album")))]
    await album_started.wait()
    tasks += [asyncio.create_task(downloader.download(context("album"))) for _ in range(3)]
    await asyncio.sleep(0)
    started = time.perf_counter()
    other = await downloader.download(context("other"))
    unrelated_ms = (time.perf_counter() - started) * 1000
    other.file.close()
    for result in await asyncio.gather(*tasks):
        result.file.close()
    await downloader.close()

    # Socket teardown latency is longer than the configured request deadline.
    resolver = TikTokService(
        settings.model_copy(update={"url_resolve_timeout_seconds": 0.02}),
        CacheStore(60, 100),
        ProxyManager(),
    )

    class SlowClient:
        async def aclose(self):
            await asyncio.sleep(0.1)

    resolver.adapter._clients = ResolverClients(lambda proxy: SlowClient())

    async def pending(*args):
        await asyncio.Future()

    resolver.adapter._follow_tiktok_redirects = pending
    started = time.perf_counter()
    try:
        await resolver.resolve_url("https://vt.tiktok.com/bench/")
    except UpstreamTimeoutError:
        pass
    timeout_ms = (time.perf_counter() - started) * 1000
    await resolver.close()

    # All requests overlap before the provider sequence fails.
    instagram = InstagramService(settings, CacheStore(60, 100))
    failure_calls = 0

    async def fail(*args):
        nonlocal failure_calls
        failure_calls += 1
        await asyncio.sleep(0.03)
        raise NetworkError("synthetic failure")

    instagram._extract_uncached = fail
    started = time.perf_counter()
    failures = await asyncio.gather(
        *(instagram.extract_url("https://www.instagram.com/p/failure/") for _ in range(8)),
        return_exceptions=True,
    )
    assert all(isinstance(failure, NetworkError) for failure in failures)
    shared_ms = (time.perf_counter() - started) * 1000
    await instagram.close()
    await runner.cleanup()
    return {
        "healthy": healthy,
        "mixed_burst": mixed,
        "probes": {
            "unrelated_download_ms": round(unrelated_ms, 3),
            "resolver_timeout_ms": round(timeout_ms, 3),
            "shared_failure_ms": round(shared_ms, 3),
            "provider_failure_sequences": failure_calls,
        },
        "telegram_received": dict(received),
        "provider_calls": dict(provider_calls),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-root", default=str(Path(__file__).resolve().parents[1]))
    parser.add_argument("--baseline-root")
    parser.add_argument("--runs", type=int, default=5)
    parser.add_argument("--requests", type=int, default=96)
    parser.add_argument("--concurrency", type=int, default=8)
    parser.add_argument("--output", default="benchmark-results.json")
    parser.add_argument("--worker", action="store_true")
    args = parser.parse_args()
    if args.worker:
        Path(args.output).write_text(json.dumps(asyncio.run(worker(args)), indent=2) + "\n")
        return
    report = {
        "environment": {
            "python": sys.version,
            "platform": platform.platform(),
            "cpu_count": os.cpu_count(),
        },
        "parameters": vars(args),
        "runs": defaultdict(list),
    }
    with tempfile.TemporaryDirectory() as directory:
        for run in range(args.runs):
            versions = [("candidate", args.source_root)]
            if args.baseline_root:
                versions.insert(0, ("baseline", args.baseline_root))
            if run % 2:
                versions.reverse()
            for label, root in versions:
                output = str(Path(directory) / f"{label}-{run}.json")
                subprocess.run(
                    [
                        sys.executable,
                        str(Path(__file__).resolve()),
                        "--worker",
                        "--source-root",
                        root,
                        "--requests",
                        str(args.requests),
                        "--concurrency",
                        str(args.concurrency),
                        "--output",
                        output,
                    ],
                    check=True,
                    env={**os.environ, "PYTHONPATH": str(Path(root).resolve() / "src")},
                )
                result = json.loads(Path(output).read_text())
                report["runs"][label].append(result)
                print(
                    f"{label} run {run + 1}: healthy p95={result['healthy']['p95_ms']} ms; "
                    f"{result['healthy']['statuses']}",
                    flush=True,
                )
    report["medians"] = {
        label: {
            phase: {
                key: statistics.median(result[phase][key] for result in runs)
                for key in (
                    "p50_ms",
                    "p95_ms",
                    "p99_ms",
                    "throughput_rps",
                    "disk_write_bytes",
                    "peak_rss_mib",
                )
            }
            for phase in ("healthy", "mixed_burst")
        }
        for label, runs in report["runs"].items()
    }
    if args.baseline_root:
        report["healthy_p95_change_percent"] = round(
            (
                report["medians"]["candidate"]["healthy"]["p95_ms"]
                / report["medians"]["baseline"]["healthy"]["p95_ms"]
                - 1
            )
            * 100,
            3,
        )
    Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    print(
        json.dumps(
            {
                "medians": report["medians"],
                "healthy_p95_change_percent": report.get("healthy_p95_change_percent"),
            },
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
