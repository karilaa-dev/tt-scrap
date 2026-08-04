"""Run opt-in end-to-end checks against a running tt-scrap instance."""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import os
from typing import Any

import httpx


async def verify_asset(client: httpx.AsyncClient, descriptor: dict[str, Any]) -> tuple[int, str]:
    response = await client.get(descriptor["download_url"])
    response.raise_for_status()
    digest = hashlib.sha256(response.content).hexdigest()
    assert response.content
    assert response.headers["x-content-sha256"] == digest
    assert int(response.headers["content-length"]) == len(response.content)
    return len(response.content), response.headers.get("content-type", "")


async def extract_and_verify(client: httpx.AsyncClient, path: str, body: dict[str, Any]) -> None:
    response = await client.post(path, json=body)
    response.raise_for_status()
    payload = response.json()
    serialized = response.text.lower()
    assert "rapidapi-key" not in serialized
    assert "sessionid" not in serialized
    assets: list[dict[str, Any]] = []
    if "media" in payload:
        for item in payload["media"]:
            assets.append(item.get("asset", item))
            if item.get("thumbnail"):
                assets.append(item["thumbnail"])
    if payload.get("cover"):
        assets.append(payload["cover"])
    if payload.get("audio"):
        assets.append(payload["audio"])
    music = payload.get("music") or {}
    if music.get("cover"):
        assets.append(music["cover"])
    assert assets
    results = await asyncio.gather(*(verify_asset(client, item) for item in assets))
    print(f"{path}: {payload.get('content_type', 'music')} {results}")


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default=os.getenv("TT_SCRAP_URL", "http://127.0.0.1:8000"))
    parser.add_argument("--tiktok-video", default=os.getenv("LIVE_TIKTOK_VIDEO_URL"))
    parser.add_argument("--tiktok-short", default=os.getenv("LIVE_TIKTOK_SHORT_URL"))
    parser.add_argument("--tiktok-slideshow", default=os.getenv("LIVE_TIKTOK_SLIDESHOW_URL"))
    parser.add_argument("--tiktok-age-restricted", default=os.getenv("LIVE_TIKTOK_AGE_URL"))
    parser.add_argument("--instagram-video", default=os.getenv("LIVE_INSTAGRAM_VIDEO_URL"))
    parser.add_argument("--instagram-image", default=os.getenv("LIVE_INSTAGRAM_IMAGE_URL"))
    parser.add_argument("--instagram-carousel", default=os.getenv("LIVE_INSTAGRAM_CAROUSEL_URL"))
    args = parser.parse_args()
    api_key = os.environ["TT_SCRAP_API_KEY"]
    async with httpx.AsyncClient(
        base_url=args.base_url,
        headers={"Authorization": f"Bearer {api_key}"},
        timeout=httpx.Timeout(180),
    ) as client:
        for value in (
            args.tiktok_video,
            args.tiktok_short,
            args.tiktok_slideshow,
            args.tiktok_age_restricted,
        ):
            if value:
                await extract_and_verify(
                    client, "/v1/tiktok/extractions", {"url": value, "refresh": True}
                )
        if args.tiktok_video:
            extracted = await client.post("/v1/tiktok/extractions", json={"url": args.tiktok_video})
            extracted.raise_for_status()
            await extract_and_verify(
                client,
                "/v1/tiktok/music",
                {"video_id": int(extracted.json()["source_id"])},
            )
        for value in (
            args.instagram_video,
            args.instagram_image,
            args.instagram_carousel,
        ):
            if value:
                await extract_and_verify(
                    client, "/v1/instagram/extractions", {"url": value, "refresh": True}
                )


if __name__ == "__main__":
    asyncio.run(main())
