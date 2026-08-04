"""Small authenticated concurrency benchmark for cached extraction responses."""

from __future__ import annotations

import argparse
import asyncio
import os
import statistics
import time

import httpx


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("url")
    parser.add_argument("--base-url", default="http://127.0.0.1:8000")
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=20)
    args = parser.parse_args()
    semaphore = asyncio.Semaphore(args.concurrency)
    latencies: list[float] = []
    async with httpx.AsyncClient(
        base_url=args.base_url,
        headers={"Authorization": f"Bearer {os.environ['TT_SCRAP_API_KEY']}"},
        timeout=120,
    ) as client:
        warmup = await client.post("/v1/tiktok/extractions", json={"url": args.url})
        warmup.raise_for_status()

        async def one() -> None:
            async with semaphore:
                started = time.perf_counter()
                response = await client.post("/v1/tiktok/extractions", json={"url": args.url})
                response.raise_for_status()
                latencies.append((time.perf_counter() - started) * 1000)

        await asyncio.gather(*(one() for _ in range(args.requests)))
    ordered = sorted(latencies)
    p95 = ordered[max(0, int(len(ordered) * 0.95) - 1)]
    print(
        f"requests={len(ordered)} mean_ms={statistics.mean(ordered):.1f} "
        f"p95_ms={p95:.1f} max_ms={max(ordered):.1f}"
    )


if __name__ == "__main__":
    asyncio.run(main())
