from __future__ import annotations

import io
import json
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from tt_scrap.cache import CacheStore
from tt_scrap.media import ConvertedImage, DownloadedAsset
from tt_scrap.models import (
    AssetDescriptor,
    AssetFetchContext,
    TikTokExtractionResponse,
    TikTokMusicMetadata,
    TikTokTelegramDeliveryRequest,
)
from tt_scrap.telegram import TelegramCallResponse, TelegramDeliveryService


class FakeDownloader:
    def __init__(self, payloads: dict[str, tuple[bytes, str]]) -> None:
        self.payloads = payloads
        self.calls: list[str] = []

    async def download(
        self, context: AssetFetchContext, *, compute_sha256: bool = True
    ) -> DownloadedAsset:
        assert not compute_sha256
        self.calls.append(context.upstream_url)
        payload, content_type = self.payloads[context.upstream_url]
        return DownloadedAsset(io.BytesIO(payload), len(payload), None, content_type)


class FakeImages:
    def __init__(self) -> None:
        self.photo_conversions = 0
        self.thumbnail_conversions = 0

    async def read_file(self, file) -> bytes:
        file.seek(0)
        value = file.read()
        file.seek(0)
        return value

    async def convert_photo(self, data: bytes, filename: str) -> ConvertedImage:
        self.photo_conversions += 1
        return ConvertedImage(b"\xff\xd8\xffconverted", "converted.jpg", "image/jpeg", 10, 10)

    async def native_photo_is_compliant(
        self, file, size, detected_content_type, declared_content_type
    ) -> bool:
        return declared_content_type in {None, detected_content_type}

    async def prepare_thumbnail(self, data: bytes, filename: str) -> ConvertedImage:
        self.thumbnail_conversions += 1
        return ConvertedImage(b"\xff\xd8\xffthumbnail", "thumbnail.jpg", "image/jpeg", 10, 10)


class FailingImages(FakeImages):
    def __init__(self, *, photo: bool = False, thumbnail: bool = False) -> None:
        super().__init__()
        self.fail_photo = photo
        self.fail_thumbnail = thumbnail

    async def convert_photo(self, data: bytes, filename: str) -> ConvertedImage:
        if self.fail_photo:
            raise ValueError("corrupt image")
        return await super().convert_photo(data, filename)

    async def prepare_thumbnail(self, data: bytes, filename: str) -> ConvertedImage:
        if self.fail_thumbnail:
            raise ValueError("corrupt thumbnail")
        return await super().prepare_thumbnail(data, filename)


class FakeTelegramClient:
    configured = True

    def __init__(self, statuses: list[int] | None = None) -> None:
        self.statuses = statuses or []
        self.calls: list[tuple[str, dict[str, Any], dict[str, bytes]]] = []

    async def call(self, method, fields, uploads) -> TelegramCallResponse:
        uploaded: dict[str, bytes] = {}
        for upload in uploads:
            upload.file.seek(0)
            uploaded[upload.field_name] = upload.file.read()
        copied_fields = json.loads(json.dumps(fields))
        self.calls.append((method, copied_fields, uploaded))
        status = (
            self.statuses[len(self.calls) - 1] if len(self.calls) <= len(self.statuses) else 200
        )
        body = (
            b'{"ok":true,"result":[]}'
            if status < 400
            else b'{"ok":false,"error_code":429,"description":"retry"}'
        )
        return TelegramCallResponse(method, status, body, "application/json")


class FakeTikTok:
    def __init__(self, extraction: TikTokExtractionResponse) -> None:
        self.extraction = extraction

    async def get_extraction(self, extraction_id: str) -> TikTokExtractionResponse:
        assert extraction_id == self.extraction.extraction_id
        return self.extraction

    async def extract_url(self, url: str, *, refresh: bool = False) -> TikTokExtractionResponse:
        return self.extraction

    async def get_cached_video(self, video_id: int) -> TikTokExtractionResponse | None:
        return self.extraction

    async def extract_music(self, video_id: int, *, refresh: bool = False):
        raise AssertionError("cached music should be reused")


async def descriptor(cache: CacheStore, name: str, kind: str, position: int = 0) -> AssetDescriptor:
    token = await cache.store_asset(
        AssetFetchContext(
            platform="tiktok",
            upstream_url=name,
            filename=f"{name}.bin",
            kind=kind,
        )
    )
    return AssetDescriptor(
        asset_id=token,
        kind=kind,
        position=position,
        download_url=f"/v1/assets/{token}",
        filename=f"{name}.bin",
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )


def request(delivery: str = "media", **telegram: Any) -> TikTokTelegramDeliveryRequest:
    return TikTokTelegramDeliveryRequest.model_validate(
        {
            "source": {"extraction_id": "extraction-1"},
            "delivery": delivery,
            "telegram": {"chat_id": 123, **telegram},
        }
    )


def service(
    settings, cache, extraction, downloader, client, images=None
) -> TelegramDeliveryService:
    return TelegramDeliveryService(
        settings,
        cache,
        FakeTikTok(extraction),
        downloader,
        images or FakeImages(),
        client,
    )


@pytest.mark.asyncio
async def test_video_upload_infers_metadata_and_attaches_thumbnail(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video")
    cover = await descriptor(cache, "cover", "cover")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/video/123",
        resolved_url="https://www.tiktok.com/@a/video/123",
        content_type="video",
        media=[video],
        cover=cover,
        width=1080,
        height=1920,
        duration_seconds=20,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {"video": (b"video-data", "video/mp4"), "cover": (b"cover-data", "image/png")}
    )
    client = FakeTelegramClient()

    outcome = await service(settings, cache, extraction, downloader, client).deliver(
        request(caption="hello")
    )

    assert outcome.calls[0].ok
    method, fields, uploads = client.calls[0]
    assert method == "sendVideo"
    assert fields["duration"] == 20
    assert fields["width"] == 1080
    assert fields["height"] == 1920
    assert fields["supports_streaming"] is True
    assert fields["caption"] == "hello"
    assert fields["thumbnail"] == "attach://thumbnail_file"
    assert fields["cover"] == "attach://thumbnail_file"
    assert uploads == {
        "video_file": b"video-data",
        "thumbnail_file": b"\xff\xd8\xffthumbnail",
    }


@pytest.mark.asyncio
async def test_video_document_mode_skips_cover_and_metadata(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video")
    cover = await descriptor(cache, "cover", "cover")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/video/123",
        resolved_url="https://www.tiktok.com/@a/video/123",
        content_type="video",
        media=[video],
        cover=cover,
        width=1080,
        height=1920,
        duration_seconds=20,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader({"video": (b"video-data", "video/mp4")})
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(
        request("document", caption="file")
    )

    method, fields, uploads = client.calls[0]
    assert method == "sendDocument"
    assert downloader.calls == ["video"]
    assert fields["disable_content_type_detection"] is True
    assert not {"duration", "width", "height", "thumbnail"}.intersection(fields)
    assert uploads["document_file"] == b"video-data"


@pytest.mark.asyncio
async def test_slideshow_is_partitioned_without_single_item_tail(settings) -> None:
    cache = CacheStore(600, 100)
    media = [await descriptor(cache, f"image-{index}", "image", index) for index in range(11)]
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=media,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {f"image-{index}": (b"\xff\xd8\xffimage", "image/jpeg") for index in range(11)}
    )
    client = FakeTelegramClient()

    outcome = await service(settings, cache, extraction, downloader, client).deliver(request())

    assert len(outcome.calls) == 2
    assert [len(call[1]["media"]) for call in client.calls] == [9, 2]
    assert all(item["type"] == "photo" for call in client.calls for item in call[1]["media"])


@pytest.mark.asyncio
async def test_slideshow_stops_after_first_telegram_failure(settings) -> None:
    cache = CacheStore(600, 100)
    media = [await descriptor(cache, f"image-{index}", "image", index) for index in range(21)]
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=media,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {f"image-{index}": (b"\xff\xd8\xffimage", "image/jpeg") for index in range(21)}
    )
    client = FakeTelegramClient([200, 429, 200])

    outcome = await service(settings, cache, extraction, downloader, client).deliver(request())

    assert len(outcome.calls) == 2
    assert len(client.calls) == 2
    assert outcome.calls[0].ok
    assert not outcome.calls[1].ok


@pytest.mark.asyncio
async def test_cached_audio_uses_send_audio_metadata(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video")
    audio = await descriptor(cache, "audio", "audio")
    cover = await descriptor(cache, "cover", "cover")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/video/123",
        resolved_url="https://www.tiktok.com/@a/video/123",
        content_type="video",
        media=[video],
        music=TikTokMusicMetadata(
            title="Track",
            author="Artist",
            duration_seconds=15,
            cover=cover,
            audio=audio,
        ),
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {"audio": (b"audio-data", "audio/mpeg"), "cover": (b"cover-data", "image/png")}
    )
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(request("audio"))

    method, fields, uploads = client.calls[0]
    assert method == "sendAudio"
    assert fields["duration"] == 15
    assert fields["title"] == "Track"
    assert fields["performer"] == "Artist"
    assert uploads["audio_file"] == b"audio-data"


@pytest.mark.asyncio
async def test_thumbnail_failure_does_not_fail_video_delivery(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video")
    cover = await descriptor(cache, "cover", "cover")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/video/123",
        resolved_url="https://www.tiktok.com/@a/video/123",
        content_type="video",
        media=[video],
        cover=cover,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {"video": (b"video-data", "video/mp4"), "cover": (b"bad-cover", "image/heic")}
    )
    client = FakeTelegramClient()

    await service(
        settings,
        cache,
        extraction,
        downloader,
        client,
        FailingImages(thumbnail=True),
    ).deliver(request())

    method, fields, uploads = client.calls[0]
    assert method == "sendVideo"
    assert "thumbnail" not in fields
    assert set(uploads) == {"video_file"}


@pytest.mark.asyncio
async def test_corrupt_unsupported_slide_fails_before_first_album(settings) -> None:
    cache = CacheStore(600, 100)
    media = [await descriptor(cache, f"image-{index}", "image", index) for index in range(2)]
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=media,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {f"image-{index}": (b"BMcorrupt", "image/bmp") for index in range(2)}
    )
    client = FakeTelegramClient()

    with pytest.raises(ValueError, match="corrupt image"):
        await service(
            settings,
            cache,
            extraction,
            downloader,
            client,
            FailingImages(photo=True),
        ).deliver(request())

    assert client.calls == []


@pytest.mark.asyncio
async def test_slideshow_documents_preserve_original_bytes_without_image_work(settings) -> None:
    cache = CacheStore(600, 100)
    media = [await descriptor(cache, f"image-{index}", "image", index) for index in range(2)]
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=media,
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    originals = {"image-0": b"BMfirst", "image-1": b"BMsecond"}
    downloader = FakeDownloader({name: (data, "image/bmp") for name, data in originals.items()})
    images = FailingImages(photo=True, thumbnail=True)
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client, images).deliver(
        request("document")
    )

    method, fields, uploads = client.calls[0]
    assert method == "sendMediaGroup"
    assert [item["type"] for item in fields["media"]] == ["document", "document"]
    assert list(uploads.values()) == [b"BMfirst", b"BMsecond"]
    assert images.photo_conversions == 0
    assert images.thumbnail_conversions == 0
