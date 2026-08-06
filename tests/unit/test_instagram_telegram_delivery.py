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
    InstagramExtractionResponse,
    InstagramMediaItem,
    InstagramTelegramDeliveryRequest,
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
        return DownloadedAsset(
            io.BytesIO(payload),
            len(payload),
            None,
            content_type,
            context.declared_content_type,
        )


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
        body = b'{"ok":true,"result":[]}' if status < 400 else b'{"ok":false,"error_code":429}'
        return TelegramCallResponse(method, status, body, "application/json")


class FakeInstagram:
    def __init__(self, extraction: InstagramExtractionResponse) -> None:
        self.extraction = extraction
        self.url_calls: list[tuple[str, bool]] = []
        self.id_calls: list[str] = []

    async def extract_url(self, url: str, *, refresh: bool = False) -> InstagramExtractionResponse:
        self.url_calls.append((url, refresh))
        return self.extraction

    async def get_extraction(self, extraction_id: str) -> InstagramExtractionResponse:
        self.id_calls.append(extraction_id)
        return self.extraction


class UnusedTikTok:
    pass


async def descriptor(
    cache: CacheStore,
    name: str,
    kind: str,
    position: int,
    *,
    declared_content_type: str | None = None,
) -> AssetDescriptor:
    token = await cache.store_asset(
        AssetFetchContext(
            platform="instagram",
            upstream_url=name,
            filename=f"{name}.bin",
            kind=kind,
            extraction_id="instagram-extraction",
            declared_content_type=declared_content_type,
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


def request(
    delivery: str = "media", *, url: bool = False, **telegram: Any
) -> InstagramTelegramDeliveryRequest:
    source = (
        {"url": "https://www.instagram.com/p/ABC123/"}
        if url
        else {"extraction_id": "instagram-extraction"}
    )
    return InstagramTelegramDeliveryRequest.model_validate(
        {
            "source": source,
            "delivery": delivery,
            "telegram": {"chat_id": 123, **telegram},
        }
    )


def service(
    settings,
    cache,
    extraction,
    downloader,
    client,
    images=None,
) -> tuple[TelegramDeliveryService, FakeInstagram]:
    instagram = FakeInstagram(extraction)
    delivery = TelegramDeliveryService(
        settings,
        cache,
        UnusedTikTok(),  # type: ignore[arg-type]
        downloader,
        images or FakeImages(),
        client,
        instagram=instagram,  # type: ignore[arg-type]
    )
    return delivery, instagram


@pytest.mark.asyncio
async def test_single_instagram_image_is_converted_and_sent_as_photo(settings) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image", "image", 0)
    extraction = InstagramExtractionResponse(
        extraction_id="instagram-extraction",
        source_url="https://www.instagram.com/p/ABC123/",
        content_type="image",
        media=[InstagramMediaItem(position=0, media_type="image", asset=image)],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader({"image": (b"BMunsupported", "image/bmp")})
    images = FakeImages()
    client = FakeTelegramClient()
    delivery, instagram = service(settings, cache, extraction, downloader, client, images)

    await delivery.deliver_instagram(request(caption="hello"))

    method, fields, uploads = client.calls[0]
    assert method == "sendPhoto"
    assert fields["caption"] == "hello"
    assert uploads["photo_file"] == b"\xff\xd8\xffconverted"
    assert images.photo_conversions == 1
    assert instagram.id_calls == ["instagram-extraction"]


@pytest.mark.asyncio
async def test_single_instagram_video_uses_converted_thumbnail(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video", 0, declared_content_type="video/mp4")
    thumbnail = await descriptor(cache, "thumbnail", "thumbnail", 0)
    extraction = InstagramExtractionResponse(
        extraction_id="instagram-extraction",
        source_url="https://www.instagram.com/reel/ABC123/",
        content_type="video",
        media=[
            InstagramMediaItem(
                position=0,
                media_type="video",
                asset=video,
                thumbnail=thumbnail,
            )
        ],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {
            "video": (b"video-data", "video/mp4"),
            "thumbnail": (b"png-thumbnail", "image/png"),
        }
    )
    images = FakeImages()
    client = FakeTelegramClient()
    delivery, instagram = service(settings, cache, extraction, downloader, client, images)

    await delivery.deliver_instagram(request(url=True, caption="video"))

    method, fields, uploads = client.calls[0]
    assert method == "sendVideo"
    assert fields["supports_streaming"] is True
    assert fields["thumbnail"] == "attach://thumbnail_file"
    assert fields["cover"] == "attach://thumbnail_file"
    assert uploads == {
        "video_file": b"video-data",
        "thumbnail_file": b"\xff\xd8\xffthumbnail",
    }
    assert images.thumbnail_conversions == 1
    assert instagram.url_calls == [("https://www.instagram.com/p/ABC123/", False)]


@pytest.mark.asyncio
async def test_mixed_instagram_carousel_preserves_order_caption_and_thumbnail(settings) -> None:
    cache = CacheStore(600, 100)
    first = await descriptor(cache, "first", "image", 0)
    video = await descriptor(cache, "video", "video", 1, declared_content_type="video/mp4")
    thumbnail = await descriptor(cache, "thumbnail", "thumbnail", 1)
    last = await descriptor(cache, "last", "image", 2)
    extraction = InstagramExtractionResponse(
        extraction_id="instagram-extraction",
        source_url="https://www.instagram.com/p/ABC123/",
        content_type="carousel",
        media=[
            InstagramMediaItem(position=0, media_type="image", asset=first),
            InstagramMediaItem(
                position=1,
                media_type="video",
                asset=video,
                thumbnail=thumbnail,
            ),
            InstagramMediaItem(position=2, media_type="image", asset=last),
        ],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {
            "first": (b"\xff\xd8\xfffirst", "image/jpeg"),
            "video": (b"video-data", "video/mp4"),
            "thumbnail": (b"thumbnail-data", "image/webp"),
            "last": (b"BMlast", "image/bmp"),
        }
    )
    client = FakeTelegramClient()
    delivery, _instagram = service(settings, cache, extraction, downloader, client)

    await delivery.deliver_instagram(request(caption="album", parse_mode="HTML", has_spoiler=True))

    method, fields, uploads = client.calls[0]
    assert method == "sendMediaGroup"
    assert [item["type"] for item in fields["media"]] == ["photo", "video", "photo"]
    assert fields["media"][0]["caption"] == "album"
    assert fields["media"][0]["parse_mode"] == "HTML"
    assert all(item["has_spoiler"] is True for item in fields["media"])
    assert fields["media"][1]["supports_streaming"] is True
    assert fields["media"][1]["thumbnail"] == "attach://thumbnail_0_1"
    assert fields["media"][1]["cover"] == "attach://thumbnail_0_1"
    assert "caption" not in fields
    assert list(uploads.values()) == [
        b"\xff\xd8\xfffirst",
        b"video-data",
        b"\xff\xd8\xffthumbnail",
        b"\xff\xd8\xffconverted",
    ]


@pytest.mark.asyncio
async def test_instagram_document_carousel_preserves_bytes_and_options(settings) -> None:
    cache = CacheStore(600, 100)
    first = await descriptor(cache, "first", "image", 0)
    second = await descriptor(cache, "second", "video", 1)
    extraction = InstagramExtractionResponse(
        extraction_id="instagram-extraction",
        source_url="https://www.instagram.com/p/ABC123/",
        content_type="carousel",
        media=[
            InstagramMediaItem(position=0, media_type="image", asset=first),
            InstagramMediaItem(position=1, media_type="video", asset=second),
        ],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {"first": (b"BMoriginal", "image/bmp"), "second": (b"video", "video/mp4")}
    )
    images = FakeImages()
    client = FakeTelegramClient()
    delivery, _instagram = service(settings, cache, extraction, downloader, client, images)

    await delivery.deliver_instagram(
        request("document", caption="files", disable_content_type_detection=False)
    )

    method, fields, uploads = client.calls[0]
    assert method == "sendMediaGroup"
    assert [item["type"] for item in fields["media"]] == ["document", "document"]
    assert fields["media"][0]["caption"] == "files"
    assert all(item["disable_content_type_detection"] is False for item in fields["media"])
    assert list(uploads.values()) == [b"BMoriginal", b"video"]
    assert images.photo_conversions == 0
    assert images.thumbnail_conversions == 0


@pytest.mark.asyncio
async def test_instagram_carousel_batches_without_single_item_tail(settings) -> None:
    cache = CacheStore(600, 100)
    descriptors = [await descriptor(cache, f"image-{index}", "image", index) for index in range(11)]
    extraction = InstagramExtractionResponse(
        extraction_id="instagram-extraction",
        source_url="https://www.instagram.com/p/ABC123/",
        content_type="carousel",
        media=[
            InstagramMediaItem(position=index, media_type="image", asset=asset)
            for index, asset in enumerate(descriptors)
        ],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader(
        {f"image-{index}": (b"\xff\xd8\xffimage", "image/jpeg") for index in range(11)}
    )
    client = FakeTelegramClient()
    delivery, _instagram = service(settings, cache, extraction, downloader, client)

    outcome = await delivery.deliver_instagram(request(caption="first batch"))

    assert len(outcome.calls) == 2
    assert [len(call[1]["media"]) for call in client.calls] == [9, 2]
    assert client.calls[0][1]["media"][0]["caption"] == "first batch"
    assert all("caption" not in item for item in client.calls[1][1]["media"])
