from __future__ import annotations

import pytest
import respx
from httpx import Response

from tt_scrap.cache import CacheStore
from tt_scrap.errors import ContentDeletedError
from tt_scrap.platforms.instagram import InstagramService

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
                ]
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
        assert [item.media_type for item in response.media] == ["image", "video"]
        assert response.media[1].thumbnail is not None
        assert "cdn.test" not in response.model_dump_json()
        assert route.call_count == 1
    finally:
        await service.close()


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
