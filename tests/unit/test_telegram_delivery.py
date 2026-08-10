from __future__ import annotations

import asyncio
import io
import json
from collections.abc import AsyncIterable
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from tt_scrap.cache import CacheStore
from tt_scrap.errors import ImageConversionError, NetworkError
from tt_scrap.media import ConvertedImage, DownloadedAsset, StreamedAsset
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

    @asynccontextmanager
    async def stream(self, context: AssetFetchContext):
        yield None


class StreamingDownloader(FakeDownloader):
    def __init__(self, payloads: dict[str, tuple[bytes, str]]) -> None:
        super().__init__(payloads)
        self.stream_calls: list[str] = []

    @asynccontextmanager
    async def stream(self, context: AssetFetchContext):
        self.stream_calls.append(context.upstream_url)
        payload, content_type = self.payloads[context.upstream_url]

        async def chunks():
            midpoint = max(1, len(payload) // 2)
            yield payload[:midpoint]
            yield payload[midpoint:]

        yield StreamedAsset(chunks(), len(payload), content_type)


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

    async def normalize_photo(self, data: bytes, filename: str) -> ConvertedImage:
        self.photo_conversions += 1
        return ConvertedImage(b"\xff\xd8\xffnormalized", "normalized.jpg", "image/jpeg", 10, 10)

    async def native_photo_is_compliant(
        self, file, size, detected_content_type, declared_content_type
    ) -> bool:
        return True

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
            if isinstance(upload.file, AsyncIterable):
                uploaded[upload.field_name] = b"".join([chunk async for chunk in upload.file])
            else:
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
async def test_video_relays_upstream_bytes_into_telegram_upload(settings) -> None:
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
    downloader = StreamingDownloader(
        {"video": (b"streamed-video", "video/mp4"), "cover": (b"cover", "image/jpeg")}
    )
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(request())

    assert downloader.stream_calls == ["video"]
    assert downloader.calls == ["cover"]
    assert client.calls[0][2]["video_file"] == b"streamed-video"


@pytest.mark.asyncio
async def test_relay_prepares_cover_before_consuming_video_stream(settings) -> None:
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

    class OrderedDownloader(StreamingDownloader):
        cover_ready = False
        stream_opened = False

        async def download(self, context, *, compute_sha256=True):
            assert self.stream_opened
            result = await super().download(context, compute_sha256=compute_sha256)
            if context.upstream_url == "cover":
                self.cover_ready = True
            return result

        @asynccontextmanager
        async def stream(self, context):
            self.stream_opened = True

            async def chunks():
                assert self.cover_ready
                yield b"streamed-video"

            yield StreamedAsset(chunks(), len(b"streamed-video"), "video/mp4")

    downloader = OrderedDownloader(
        {"video": (b"streamed-video", "video/mp4"), "cover": (b"cover", "image/jpeg")}
    )
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(request())

    assert "thumbnail_file" in client.calls[0][2]


@pytest.mark.asyncio
async def test_interrupted_relay_retries_with_verified_download(settings) -> None:
    cache = CacheStore(600, 100)
    video = await descriptor(cache, "video", "video")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/video/123",
        resolved_url="https://www.tiktok.com/@a/video/123",
        content_type="video",
        media=[video],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )

    class InterruptedRelayDownloader(FakeDownloader):
        @asynccontextmanager
        async def stream(self, context):
            streamed: StreamedAsset

            async def chunks():
                yield b"partial-"
                failure = NetworkError("upstream interrupted")
                streamed.failure = failure
                raise failure

            streamed = StreamedAsset(chunks(), 14, "video/mp4")
            yield streamed

    downloader = InterruptedRelayDownloader({"video": (b"verified-video", "video/mp4")})
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(request())

    assert downloader.calls == ["video"]
    assert client.calls[0][2]["video_file"] == b"verified-video"


@pytest.mark.asyncio
async def test_video_relay_does_not_wait_for_a_slow_source_cover(settings) -> None:
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

    class SlowCoverDownloader(StreamingDownloader):
        async def download(self, context, *, compute_sha256=True):
            if context.upstream_url == "cover":
                await asyncio.sleep(1)
            return await super().download(context, compute_sha256=compute_sha256)

    downloader = SlowCoverDownloader(
        {"video": (b"streamed-video", "video/mp4"), "cover": (b"cover", "image/jpeg")}
    )
    client = FakeTelegramClient()
    settings.telegram_thumbnail_wait_seconds = 0.01

    await asyncio.wait_for(
        service(settings, cache, extraction, downloader, client).deliver(request()),
        timeout=0.5,
    )

    assert client.calls[0][2] == {"video_file": b"streamed-video"}
    assert "thumbnail" not in client.calls[0][1]


@pytest.mark.asyncio
async def test_downloads_continue_while_telegram_upload_slot_is_busy(settings) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image", "image")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=[image],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    both_downloaded = asyncio.Event()

    class SignalingDownloader(FakeDownloader):
        async def download(self, context, *, compute_sha256=True):
            result = await super().download(context, compute_sha256=compute_sha256)
            if len(self.calls) == 2:
                both_downloaded.set()
            return result

    class GatedTelegramClient(FakeTelegramClient):
        def __init__(self) -> None:
            super().__init__()
            self.started = asyncio.Event()
            self.release = asyncio.Event()
            self.active = 0
            self.peak = 0

        async def call(self, method, fields, uploads):
            self.active += 1
            self.peak = max(self.peak, self.active)
            self.started.set()
            await self.release.wait()
            try:
                return await super().call(method, fields, uploads)
            finally:
                self.active -= 1

    settings.telegram_pipeline_concurrency = 4
    settings.telegram_upload_concurrency = 1
    downloader = SignalingDownloader({"image": (b"\xff\xd8\xffimage", "image/jpeg")})
    client = GatedTelegramClient()
    delivery = service(settings, cache, extraction, downloader, client)
    tasks = [asyncio.create_task(delivery.deliver(request())) for _ in range(2)]

    await asyncio.wait_for(client.started.wait(), timeout=1)
    await asyncio.wait_for(both_downloaded.wait(), timeout=1)
    client.release.set()
    await asyncio.gather(*tasks)

    assert client.peak == 1
    assert downloader.calls == ["image", "image"]


@pytest.mark.asyncio
async def test_telegram_api_calls_respect_upload_concurrency(settings) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image", "image")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=[image],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )

    class SlowTelegramClient(FakeTelegramClient):
        def __init__(self) -> None:
            super().__init__()
            self.active = 0
            self.peak = 0

        async def call(self, method, fields, uploads):
            self.active += 1
            self.peak = max(self.peak, self.active)
            await asyncio.sleep(0.02)
            try:
                return await super().call(method, fields, uploads)
            finally:
                self.active -= 1

    settings.telegram_pipeline_concurrency = 8
    settings.telegram_upload_concurrency = 2
    downloader = FakeDownloader({"image": (b"\xff\xd8\xffimage", "image/jpeg")})
    client = SlowTelegramClient()
    delivery = service(settings, cache, extraction, downloader, client)

    await asyncio.gather(*(delivery.deliver(request()) for _ in range(8)))

    assert client.peak == 2


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
async def test_single_image_slideshow_attaches_caption_to_photo(settings) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image-0", "image")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=[image],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    downloader = FakeDownloader({"image-0": (b"\xff\xd8\xffimage", "image/jpeg")})
    client = FakeTelegramClient()

    await service(settings, cache, extraction, downloader, client).deliver(
        request(caption="source", parse_mode="HTML")
    )

    method, fields, uploads = client.calls[0]
    assert method == "sendPhoto"
    assert fields["caption"] == "source"
    assert fields["parse_mode"] == "HTML"
    assert uploads["media_0"] == b"\xff\xd8\xffimage"


@pytest.mark.parametrize(
    ("payload", "content_type"),
    [
        (b"\xff\xd8\xfforiginal-jpeg", "image/jpeg"),
        (b"\x89PNG\r\n\x1a\noriginal-png", "image/png"),
        (b"RIFFxxxxWEBPoriginal-webp", "image/webp"),
    ],
)
@pytest.mark.asyncio
async def test_native_slideshow_images_are_uploaded_byte_for_byte(
    settings, payload: bytes, content_type: str
) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image-0", "image")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=[image],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )
    images = FakeImages()
    client = FakeTelegramClient()

    await service(
        settings,
        cache,
        extraction,
        FakeDownloader({"image-0": (payload, content_type)}),
        client,
        images,
    ).deliver(request())

    assert client.calls[0][2]["media_0"] == payload
    assert images.photo_conversions == 0


@pytest.mark.asyncio
async def test_noncompliant_native_slide_is_normalized_before_upload(settings) -> None:
    cache = CacheStore(600, 100)
    image = await descriptor(cache, "image-0", "image")
    extraction = TikTokExtractionResponse(
        extraction_id="extraction-1",
        source_id="123",
        source_url="https://www.tiktok.com/@a/photo/123",
        resolved_url="https://www.tiktok.com/@a/photo/123",
        content_type="slideshow",
        media=[image],
        expires_at=datetime.now(UTC) + timedelta(minutes=10),
    )

    class NoncompliantImages(FakeImages):
        async def native_photo_is_compliant(
            self, file, size, detected_content_type, declared_content_type
        ) -> bool:
            return False

    images = NoncompliantImages()
    client = FakeTelegramClient()
    await service(
        settings,
        cache,
        extraction,
        FakeDownloader({"image-0": (b"\xff\xd8\xffoversized", "image/jpeg")}),
        client,
        images,
    ).deliver(request())

    assert client.calls[0][2]["media_0"] == b"\xff\xd8\xffnormalized"
    assert images.photo_conversions == 1


@pytest.mark.asyncio
async def test_thumbnail_preparation_overlaps_video_download(settings) -> None:
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
    thumbnail_ready = asyncio.Event()

    class GatedDownloader(FakeDownloader):
        async def download(self, context, *, compute_sha256=True):
            if context.upstream_url == "video":
                await asyncio.wait_for(thumbnail_ready.wait(), timeout=1)
            return await super().download(context, compute_sha256=compute_sha256)

    class SignalingImages(FakeImages):
        async def prepare_thumbnail(self, data: bytes, filename: str) -> ConvertedImage:
            result = await super().prepare_thumbnail(data, filename)
            thumbnail_ready.set()
            return result

    client = FakeTelegramClient()
    await asyncio.wait_for(
        service(
            settings,
            cache,
            extraction,
            GatedDownloader(
                {"video": (b"video-data", "video/mp4"), "cover": (b"cover", "image/jpeg")}
            ),
            client,
            SignalingImages(),
        ).deliver(request()),
        timeout=1,
    )

    assert client.calls[0][0] == "sendVideo"


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

    images = FailingImages(photo=True)
    with pytest.raises(ImageConversionError, match="only HEIC/HEIF is converted"):
        await service(
            settings,
            cache,
            extraction,
            downloader,
            client,
            images,
        ).deliver(request())

    assert client.calls == []
    assert images.photo_conversions == 0


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
