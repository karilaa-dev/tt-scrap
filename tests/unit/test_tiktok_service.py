from __future__ import annotations

from typing import Any

import pytest

from tt_scrap.assets import AssetFactory
from tt_scrap.cache import CacheStore
from tt_scrap.platforms.tiktok.service import (
    TikTokService,
    extract_video_url,
    select_video_source,
)
from tt_scrap.proxy import ProxyManager


class FakeContext:
    def __init__(self) -> None:
        self.referer_url = "https://www.tiktok.com/@_/video/123"
        self.proxy_slot = 0
        self.closed = False

    def cookies_for(self, media_url: str) -> dict[str, str]:
        return {"session": f"cookie-for-{media_url.rsplit('/', 1)[-1]}"}

    def close(self) -> None:
        self.closed = True


class FakeAdapter:
    def __init__(self, payload: dict[str, Any]) -> None:
        self.payload = payload
        self.calls = 0
        self.contexts: list[FakeContext] = []

    async def resolve_url(self, url: str, proxy_session) -> str:
        return "https://www.tiktok.com/@creator/video/123"

    def extract_id(self, url: str) -> str:
        return "123"

    async def extract(self, url: str, video_id: str, proxy_session):
        self.calls += 1
        context = FakeContext()
        self.contexts.append(context)
        return self.payload, context

    async def close(self) -> None:
        return None


def make_service(settings, payload: dict[str, Any]):
    cache = CacheStore(settings.cache_ttl_seconds, settings.cache_max_entries)
    service = object.__new__(TikTokService)
    service.settings = settings
    service.cache = cache
    service.assets = AssetFactory(cache)
    service.proxy_manager = ProxyManager()
    service.adapter = FakeAdapter(payload)
    return service, cache


def test_video_url_fallback() -> None:
    assert extract_video_url({"downloadAddr": "https://cdn/video"}) == "https://cdn/video"
    assert extract_video_url({"playAddr": "https://cdn/play", "bitrateInfo": []}) == (
        "https://cdn/play"
    )


def test_video_source_selects_known_quality_tag_and_preserves_mirrors() -> None:
    source = select_video_source(
        {
            "playAddr": "https://cdn/regular",
            "width": 720,
            "height": 1280,
            "bitrateInfo": [
                {
                    "Bitrate": 2_500_000,
                    "GearName": "normal_720_0",
                    "PlayAddr": {
                        "Width": 720,
                        "Height": 1280,
                        "UrlKey": "video_h264_720p_2500000",
                        "UrlList": ["https://cdn/h264-1", "https://cdn/h264-2"],
                    },
                },
                {
                    "GearName": "adapt_lowest_1080_1",
                    "PlayAddr": {
                        "Width": 1080,
                        "Height": 1920,
                        "UrlKey": "video_bytevc1_1080p_1900000",
                        "UrlList": ["https://cdn/hevc-1", "https://cdn/hevc-2"],
                    },
                },
            ],
        }
    )

    assert source is not None
    assert source.url == "https://cdn/hevc-1"
    assert source.alternate_urls == ["https://cdn/hevc-2"]
    assert (source.width, source.height) == (1080, 1920)


def test_video_source_falls_back_when_known_quality_tag_is_absent() -> None:
    source = select_video_source(
        {
            "playAddr": "https://cdn/regular",
            "bitrateInfo": [
                {
                    "GearName": "normal_720_0",
                    "PlayAddr": {
                        "Width": 720,
                        "Height": 1280,
                        "UrlList": ["https://cdn/720"],
                    },
                },
                {
                    "GearName": "some_other_1080",
                    "PlayAddr": {
                        "Width": 1080,
                        "Height": 1920,
                        "UrlKey": "video_h264_1080p_3000000",
                        "UrlList": ["https://cdn/1080"],
                    },
                },
            ],
        }
    )

    assert source and source.url == "https://cdn/regular"


def test_video_source_adds_highest_bitrate_adaptive_audio() -> None:
    source = select_video_source(
        {
            "playAddr": "https://cdn/regular-with-audio",
            "width": 720,
            "height": 1280,
            "bitrateAudioInfo": [
                {
                    "Bitrate": 64_000,
                    "AudioQuality": 6,
                    "UrlList": {"MainUrl": "https://cdn/audio-low"},
                },
                {
                    "Bitrate": 96_000,
                    "AudioQuality": 7,
                    "UrlList": {
                        "MainUrl": "https://cdn/audio-primary",
                        "BackupUrl": "https://cdn/audio-backup",
                    },
                },
            ],
            "bitrateInfo": [
                {
                    "GearName": "adapt_lowest_1080_1",
                    "PlayAddr": {
                        "Width": 1080,
                        "Height": 1920,
                        "UrlList": ["https://cdn/silent-1080"],
                    },
                }
            ],
        }
    )

    assert source is not None
    assert source.url == "https://cdn/silent-1080"
    assert source.audio_url == "https://cdn/audio-primary"
    assert source.alternate_audio_urls == ["https://cdn/audio-backup"]
    assert (source.width, source.height) == (1080, 1920)


def test_video_source_falls_back_when_adaptive_audio_is_missing() -> None:
    source = select_video_source(
        {
            "playAddr": "https://cdn/regular-with-audio",
            "width": 720,
            "height": 1280,
            "bitrateAudioInfo": [{"Bitrate": 96_000, "UrlList": {}}],
            "bitrateInfo": [
                {
                    "GearName": "adapt_lowest_1080_1",
                    "PlayAddr": {
                        "Width": 1080,
                        "Height": 1920,
                        "UrlList": ["https://cdn/silent-1080"],
                    },
                }
            ],
        }
    )

    assert source and source.url == "https://cdn/regular-with-audio"


@pytest.mark.asyncio
async def test_video_response_is_normalized_cached_and_closes_context(settings) -> None:
    payload = {
        "video": {
            "playAddr": "https://video.cdn.test/media",
            "cover": "https://image.cdn.test/cover",
            "width": 1080,
            "height": 1920,
            "duration": 15,
        },
        "stats": {"diggCount": 12, "playCount": 34},
        "music": {
            "playUrl": "https://audio.cdn.test/music",
            "title": "Track",
            "authorName": "Artist",
            "duration": 15,
            "coverLarge": "https://image.cdn.test/music-cover",
        },
    }
    service, _cache = make_service(settings, payload)

    response = await service.extract_url("https://www.tiktok.com/@creator/video/123")
    cached = await service.extract_url("https://www.tiktok.com/@creator/video/123")
    tracked = await service.extract_url("https://www.tiktok.com/@other/video/123?is_from_webapp=1")

    assert response == cached
    assert tracked.source_id == response.source_id
    assert tracked.source_url.endswith("?is_from_webapp=1")
    assert response.content_type == "video"
    assert response.media[0].download_url.startswith("/v1/assets/")
    assert response.width == 1080
    assert response.likes == 12
    assert response.music and response.music.title == "Track"
    assert "cdn.test" not in response.model_dump_json()
    assert service.adapter.calls == 1
    assert service.adapter.contexts[0].closed


@pytest.mark.asyncio
async def test_video_response_uses_selected_quality_source(settings) -> None:
    payload = {
        "video": {
            "playAddr": "https://video.cdn.test/regular",
            "width": 720,
            "height": 1280,
            "duration": 15,
            "bitrateAudioInfo": [
                {
                    "Bitrate": 96_000,
                    "UrlList": {
                        "MainUrl": "https://audio.cdn.test/best-primary",
                        "BackupUrl": "https://audio.cdn.test/best-backup",
                    },
                }
            ],
            "bitrateInfo": [
                {
                    "GearName": "adapt_lowest_1080_1",
                    "PlayAddr": {
                        "Width": 1080,
                        "Height": 1920,
                        "UrlKey": "video_bytevc1_1080p_1900000",
                        "UrlList": [
                            "https://video.cdn.test/best-primary",
                            "https://video.cdn.test/best-backup",
                        ],
                    },
                }
            ],
        }
    }
    service, cache = make_service(settings, payload)

    response = await service.extract_url("https://www.tiktok.com/@creator/video/123")
    token = response.media[0].download_url.rsplit("/", 1)[-1]
    context = await cache.get_asset(token)

    assert (response.width, response.height) == (1080, 1920)
    assert context.upstream_url == "https://video.cdn.test/best-primary"
    assert context.alternate_upstream_urls == ["https://video.cdn.test/best-backup"]
    assert context.audio is not None
    assert context.audio.upstream_url == "https://audio.cdn.test/best-primary"
    assert context.audio.alternate_upstream_urls == ["https://audio.cdn.test/best-backup"]


@pytest.mark.asyncio
async def test_slideshow_preserves_order(settings) -> None:
    payload = {
        "imagePost": {
            "images": [
                {"imageURL": {"urlList": ["https://cdn.test/one"]}},
                {"imageURL": {"urlList": ["https://cdn.test/two"]}},
            ]
        },
        "stats": {"diggCount": 1, "playCount": 2},
    }
    service, _cache = make_service(settings, payload)
    response = await service.extract_url("https://www.tiktok.com/@creator/photo/123")

    assert response.content_type == "slideshow"
    assert [asset.position for asset in response.media] == [0, 1]
    assert [asset.filename for asset in response.media] == ["123_1.jpg", "123_2.jpg"]
    assert service.adapter.contexts[0].closed


@pytest.mark.asyncio
async def test_music_response_has_audio_and_cover_assets(settings) -> None:
    payload = {
        "music": {
            "playUrl": "https://cdn.test/audio",
            "title": "Sound",
            "authorName": "Creator",
            "duration": 9,
            "coverThumb": "https://cdn.test/cover",
        }
    }
    service, _cache = make_service(settings, payload)
    response = await service.extract_music(123)

    assert response.audio.kind == "audio"
    assert response.cover and response.cover.kind == "cover"
    assert response.title == "Sound"
    assert "cdn.test" not in response.model_dump_json()
    assert service.adapter.contexts[0].closed
