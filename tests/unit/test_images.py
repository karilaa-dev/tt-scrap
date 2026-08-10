from __future__ import annotations

import asyncio
import io

import pytest
from PIL import Image, JpegImagePlugin
from pillow_heif import from_pillow

from tt_scrap.errors import ImageConversionError
from tt_scrap.media.images import (
    ImagePreparationService,
    detect_image_format,
    image_worker_count,
    is_native_telegram_photo,
)


def image_bytes(format_name: str, mode: str = "RGB", size: tuple[int, int] = (640, 480)) -> bytes:
    image = Image.new(mode, size, (20, 40, 60, 128) if mode == "RGBA" else "navy")
    output = io.BytesIO()
    image.save(output, format=format_name)
    return output.getvalue()


def test_detects_supported_and_conversion_formats() -> None:
    assert detect_image_format(b"\xff\xd8\xffpayload") == "jpeg"
    assert detect_image_format(b"\x89PNG\r\n\x1a\npayload") == "png"
    assert detect_image_format(b"RIFFxxxxWEBPpayload") == "webp"
    assert detect_image_format(b"\x00\x00\x00\x18ftypheicpayload") == "heic"
    assert detect_image_format(b"\x00\x00\x00\x18ftypmif1payload") == "heif"
    assert detect_image_format(b"\x00\x00\x00\x18ftypavifpayload") == "avif"
    assert detect_image_format(b"II*\x00payload") == "tiff"
    assert detect_image_format(b"BMpayload") == "bmp"
    assert detect_image_format(b"GIF89apayload") == "gif"
    assert detect_image_format(b"invalid") == "unknown"
    assert is_native_telegram_photo(b"\xff\xd8\xffpayload")
    assert not is_native_telegram_photo(b"\x00\x00\x00\x18ftypheicpayload")
    assert not is_native_telegram_photo(b"RIFFxxxxWEBPVP8X\x00\x00\x00\x00\x02animated")


@pytest.mark.parametrize(
    ("configured_workers", "available_cpus", "expected"),
    [
        (0, 8, 7),
        (0, 1, 1),
        (4, 8, 4),
        (16, 8, 7),
    ],
)
def test_image_worker_count_reserves_one_cpu(
    configured_workers: int, available_cpus: int, expected: int
) -> None:
    assert image_worker_count(configured_workers, available_cpus) == expected


@pytest.mark.asyncio
async def test_heic_photo_is_converted_to_baseline_jpeg(settings) -> None:
    image = Image.new("RGB", (640, 480), "navy")
    source = io.BytesIO()
    from_pillow(image).save(source)
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.convert_photo(source.getvalue(), "slide.heic")
    finally:
        await service.close()

    assert result.data.startswith(b"\xff\xd8\xff")
    assert result.filename == "slide.jpg"
    assert result.content_type == "image/jpeg"
    assert (result.width, result.height) == (640, 480)
    with Image.open(io.BytesIO(result.data)) as converted:
        assert not converted.info.get("progressive")
        assert JpegImagePlugin.get_sampling(converted) == 2


@pytest.mark.asyncio
async def test_only_heic_and_heif_are_accepted_for_photo_conversion(settings) -> None:
    image = Image.new("RGB", (64, 48), "navy")
    heif = io.BytesIO()
    from_pillow(image).save(heif)
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        heif_result = await service.convert_photo(heif.getvalue(), "slide.heic")
        with pytest.raises(ImageConversionError, match="Only HEIC/HEIF"):
            await service.convert_photo(image_bytes("BMP"), "slide.bmp")
        with pytest.raises(ImageConversionError, match="Only HEIC/HEIF"):
            await service.convert_photo(image_bytes("AVIF"), "slide.avif")
    finally:
        await service.close()

    assert heif_result.data.startswith(b"\xff\xd8\xff")


@pytest.mark.asyncio
async def test_thumbnail_is_jpeg_bounded_and_alpha_is_composited(settings) -> None:
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.prepare_thumbnail(image_bytes("PNG", mode="RGBA"), "cover.png")
    finally:
        await service.close()

    assert result.data.startswith(b"\xff\xd8\xff")
    assert result.filename == "cover.jpg"
    assert max(result.width, result.height) <= 320
    assert len(result.data) < 200_000
    with Image.open(io.BytesIO(result.data)) as converted:
        assert converted.mode == "RGB"


@pytest.mark.asyncio
async def test_compliant_thumbnail_is_returned_without_reencoding(settings) -> None:
    source = image_bytes("JPEG", size=(120, 90))
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.prepare_thumbnail(source, "cover.jpeg")
    finally:
        await service.close()

    assert result.data == source
    assert (result.width, result.height) == (120, 90)


@pytest.mark.asyncio
async def test_native_photo_validation_ignores_declared_mime_but_checks_limits(settings) -> None:
    source = image_bytes("PNG", size=(100, 100))
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        assert await service.native_photo_is_compliant(
            io.BytesIO(source), len(source), "image/png", "image/png; charset=binary"
        )
        assert await service.native_photo_is_compliant(
            io.BytesIO(source), len(source), "image/png", "image/jpeg"
        )
        panoramic = image_bytes("PNG", size=(2100, 100))
        assert not await service.native_photo_is_compliant(
            io.BytesIO(panoramic), len(panoramic), "image/png", "image/png"
        )
        assert not await service.native_photo_is_compliant(
            io.BytesIO(b"\xff\xd8\xffcorrupt"), 10, "image/jpeg", "image/jpeg"
        )
    finally:
        await service.close()


@pytest.mark.asyncio
async def test_noncompliant_native_photo_is_normalized_to_fit_telegram(settings) -> None:
    panoramic = image_bytes("PNG", size=(2100, 100))
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.normalize_photo(panoramic, "panorama.png")
    finally:
        await service.close()

    assert result.filename == "panorama.jpg"
    assert result.content_type == "image/jpeg"
    assert max(result.width / result.height, result.height / result.width) <= 20
    assert len(result.data) < 10_000_000


@pytest.mark.asyncio
async def test_animated_native_photo_is_flattened_during_normalization(settings) -> None:
    frames = [Image.new("RGB", (80, 40), color) for color in ("navy", "orange")]
    source = io.BytesIO()
    frames[0].save(source, format="WEBP", save_all=True, append_images=frames[1:], duration=50)
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.normalize_photo(source.getvalue(), "animated.webp")
    finally:
        await service.close()

    with Image.open(io.BytesIO(result.data)) as normalized:
        assert normalized.format == "JPEG"
        assert not getattr(normalized, "is_animated", False)


@pytest.mark.asyncio
async def test_exif_orientation_is_applied_during_heic_conversion(settings) -> None:
    image = Image.new("RGB", (80, 40), "navy")
    exif = Image.Exif()
    exif[274] = 6
    source = io.BytesIO()
    from_pillow(image).save(source, exif=exif.tobytes())
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    try:
        result = await service.convert_photo(source.getvalue(), "oriented.heic")
    finally:
        await service.close()

    assert (result.width, result.height) == (40, 80)


@pytest.mark.asyncio
async def test_process_conversion_keeps_event_loop_responsive(settings) -> None:
    image = Image.new("RGB", (1200, 900), "navy")
    encoded = io.BytesIO()
    from_pillow(image).save(encoded)
    source = encoded.getvalue()
    service = ImagePreparationService(settings.model_copy(update={"image_conversion_workers": 1}))
    tasks = [
        asyncio.create_task(service.convert_photo(source, f"slide-{index}.heic"))
        for index in range(2)
    ]
    heartbeat_ticks = 0
    try:
        while not all(task.done() for task in tasks):
            heartbeat_ticks += 1
            await asyncio.sleep(0.001)
        await asyncio.gather(*tasks)
    finally:
        await service.close()

    assert heartbeat_ticks > 1
