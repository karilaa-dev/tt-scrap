from __future__ import annotations

import asyncio

import pytest
import respx
from httpx import Response

from tt_scrap.cache import CacheStore
from tt_scrap.errors import ContentDeletedError, RateLimitError
from tt_scrap.platforms.instagram import InstagramService, extract_instagram_media_id

API_URL = "https://instagram-downloader-download-instagram-stories-videos4.p.rapidapi.com/convert"


def make_service(settings) -> InstagramService:
    cache = CacheStore(settings.cache_ttl_seconds, settings.cache_max_entries)
    return InstagramService(settings, cache)


@pytest.mark.asyncio
@respx.mock
async def test_mixed_carousel_is_normalized_and_cached(settings) -> None:
    route = respx.get(API_URL).mock(
        return_value=Response(
            200,
            json={
                "owner": {"username": "instagram_creator"},
                "media": [
                    {
                        "type": "image",
                        "url": "https://cdn.test/image",
                        "quality": "1080p",
                    },
                    {
                        "type": "video",
                        "url": "https://cdn.test/video",
                        "thumbnail": "https://cdn.test/thumb",
                    },
                ],
            },
        )
    )
    service = make_service(settings)
    try:
        url = "https://www.instagram.com/p/ABC123/"
        response = await service.extract_url(url)
        cached = await service.extract_url(url)
        tracked = await service.extract_url(f"{url}?igsh=tracking")

        assert response == cached
        assert tracked.content_type == response.content_type
        assert tracked.source_url.endswith("?igsh=tracking")
        assert response.content_type == "carousel"
        assert response.source_id == "ABC123"
        assert response.creator_username == "instagram_creator"
        assert [item.media_type for item in response.media] == ["image", "video"]
        assert [item.asset.filename for item in response.media] == [
            "ABC123_1.jpg",
            "ABC123_2.mp4",
        ]
        assert response.media[1].thumbnail is not None
        assert response.media[1].thumbnail.filename == "ABC123_2_thumbnail.jpg"
        assert await service.get_extraction(response.extraction_id) == response
        assert "cdn.test" not in response.model_dump_json()
        assert route.call_count == 1
    finally:
        await service.close()


@pytest.mark.asyncio
@respx.mock
async def test_single_video_uses_instagram_shortcode_as_filename(settings) -> None:
    respx.get(API_URL).mock(
        return_value=Response(
            200,
            json={
                "media": [
                    {
                        "type": "video",
                        "url": "https://cdn.test/video",
                        "thumbnail": "https://cdn.test/thumb",
                    }
                ]
            },
        )
    )
    service = make_service(settings)
    try:
        response = await service.extract_url(
            "https://www.instagram.com/p/DaJJCIVEn2n/?igsh=OGJnMm55YmpjaXl3"
        )

        assert response.media[0].asset.filename == "DaJJCIVEn2n.mp4"
        assert response.media[0].thumbnail is not None
        assert response.media[0].thumbnail.filename == "DaJJCIVEn2n_thumbnail.jpg"
    finally:
        await service.close()


@pytest.mark.parametrize(
    ("url", "expected"),
    [
        ("https://www.instagram.com/p/DaJJCIVEn2n/", "DaJJCIVEn2n"),
        ("https://www.instagram.com/reel/ABC_123-x/", "ABC_123-x"),
        ("https://www.instagram.com/stories/creator/987654321/", "987654321"),
    ],
)
def test_extract_instagram_media_id(url: str, expected: str) -> None:
    assert extract_instagram_media_id(url) == expected


@pytest.mark.asyncio
@respx.mock
async def test_instagram_404_is_not_retried(settings) -> None:
    route = respx.get(API_URL).mock(return_value=Response(404))
    service = make_service(settings)
    try:
        with pytest.raises(ContentDeletedError):
            await service.extract_url("https://www.instagram.com/reel/ABC123/")
        assert route.call_count == 1
    finally:
        await service.close()


@pytest.mark.asyncio
@respx.mock
async def test_instagram_5xx_is_retried(settings) -> None:
    route = respx.get(API_URL).mock(
        side_effect=[
            Response(503),
            Response(200, json={"media": [{"type": "image", "url": "https://cdn/a"}]}),
        ]
    )
    service = make_service(settings)
    try:
        response = await service.extract_url("https://www.instagram.com/p/ABC123/")
        assert response.content_type == "image"
        assert route.call_count == 2
    finally:
        await service.close()


@pytest.mark.asyncio
async def test_concurrent_refreshes_share_one_provider_request(settings, monkeypatch) -> None:
    service = make_service(settings)
    calls = 0

    async def fake_rapidapi(source_url: str) -> dict[str, object]:
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.01)
        return {"media": [{"type": "image", "url": "https://cdn.test/image"}]}

    monkeypatch.setattr(service, "_rapidapi", fake_rapidapi)
    try:
        results = await asyncio.gather(
            *[
                service.extract_url("https://www.instagram.com/p/ABC123/", refresh=True)
                for _ in range(8)
            ]
        )

        assert calls == 1
        assert len({result.extraction_id for result in results}) == 1
    finally:
        await service.close()


@pytest.mark.asyncio
async def test_instagram_provider_concurrency_has_dedicated_cap(settings, monkeypatch) -> None:
    settings.instagram_concurrency = 4
    settings.extraction_concurrency = 32
    service = make_service(settings)
    active = 0
    peak = 0

    async def fake_get(*args, **kwargs) -> Response:
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)
        active -= 1
        return Response(200, json={"media": [{"type": "image", "url": "https://cdn/a"}]})

    monkeypatch.setattr(service._http, "get", fake_get)
    try:
        await asyncio.gather(
            *[
                service.extract_url(f"https://www.instagram.com/p/POST{index}/")
                for index in range(8)
            ]
        )
        assert peak == 4
    finally:
        await service.close()


@pytest.mark.asyncio
async def test_rate_limit_backoff_releases_provider_slot(settings, monkeypatch) -> None:
    settings.instagram_concurrency = 1
    settings.instagram_max_attempts = 2
    settings.instagram_retry_delay_seconds = 0
    service = make_service(settings)
    rate_limited = asyncio.Event()
    rate_calls = 0

    async def fake_get(*args, params, **kwargs) -> Response:
        nonlocal rate_calls
        if "/RATE/" in params["url"] and rate_calls == 0:
            rate_calls += 1
            rate_limited.set()
            return Response(429, headers={"Retry-After": "0.1"})
        return Response(200, json={"media": [{"type": "image", "url": "https://cdn/a"}]})

    monkeypatch.setattr(service._http, "get", fake_get)
    try:
        retrying = asyncio.create_task(service.extract_url("https://www.instagram.com/p/RATE/"))
        await asyncio.wait_for(rate_limited.wait(), timeout=1)
        unrelated = await asyncio.wait_for(
            service.extract_url("https://www.instagram.com/p/FAST/"),
            timeout=0.08,
        )
        await retrying
        assert unrelated.source_id == "FAST"
    finally:
        await service.close()


@pytest.mark.asyncio
async def test_provider_retry_after_is_bounded_by_total_backoff_budget(
    settings, monkeypatch
) -> None:
    settings.instagram_max_attempts = 3
    settings.instagram_retry_delay_seconds = 0
    settings.instagram_request_timeout_seconds = 0.01
    service = make_service(settings)
    calls = 0

    async def fake_get(*args, **kwargs) -> Response:
        nonlocal calls
        calls += 1
        return Response(429, headers={"Retry-After": "30"})

    monkeypatch.setattr(service._http, "get", fake_get)
    started = asyncio.get_running_loop().time()
    try:
        with pytest.raises(RateLimitError):
            await service.extract_url("https://www.instagram.com/p/RATE/")
    finally:
        await service.close()

    assert asyncio.get_running_loop().time() - started < 0.1
    assert calls == 2
