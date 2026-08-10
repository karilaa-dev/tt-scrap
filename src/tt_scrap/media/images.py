"""Bounded image conversion for Telegram photo and thumbnail uploads."""

from __future__ import annotations

import asyncio
import io
import logging
import math
import multiprocessing
import os
import signal
from concurrent.futures import ProcessPoolExecutor
from dataclasses import dataclass
from functools import partial
from time import perf_counter
from typing import BinaryIO, Literal

from PIL import Image, ImageOps
from pillow_heif import register_heif_opener

from ..config import Settings
from ..errors import ImageConversionError
from ..logging import elapsed_ms, log_event

logger = logging.getLogger(__name__)

ImageFormat = Literal[
    "jpeg",
    "png",
    "webp",
    "heic",
    "heif",
    "avif",
    "tiff",
    "bmp",
    "gif",
    "unknown",
]

_NATIVE_PHOTO_FORMATS = {"jpeg", "png", "webp"}
_PHOTO_MAX_BYTES = 10 * 1024 * 1024
_THUMBNAIL_MAX_BYTES = 200_000
_HEIC_PHOTO_FORMATS = {"heic", "heif"}
_FALLBACK_PHOTO_FORMATS = {"avif", "tiff", "bmp", "gif"}
_NORMALIZABLE_PHOTO_FORMATS = _NATIVE_PHOTO_FORMATS | _HEIC_PHOTO_FORMATS | _FALLBACK_PHOTO_FORMATS
_THUMBNAIL_INPUT_FORMATS = _NATIVE_PHOTO_FORMATS | _HEIC_PHOTO_FORMATS


@dataclass(frozen=True, slots=True)
class ConvertedImage:
    data: bytes
    filename: str
    content_type: str
    width: int
    height: int


def detect_image_format(prefix: bytes) -> ImageFormat:
    if prefix.startswith(b"\xff\xd8\xff"):
        return "jpeg"
    if prefix.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png"
    if prefix.startswith(b"RIFF") and len(prefix) >= 12 and prefix[8:12] == b"WEBP":
        return "webp"
    if prefix.startswith((b"II*\x00", b"MM\x00*")):
        return "tiff"
    if prefix.startswith(b"BM"):
        return "bmp"
    if prefix.startswith((b"GIF87a", b"GIF89a")):
        return "gif"
    if len(prefix) >= 12 and prefix[4:8] == b"ftyp":
        brand = prefix[8:12]
        if brand in {b"avif", b"avis"}:
            return "avif"
        if brand in {b"mif1", b"msf1"}:
            return "heif"
        if brand in {
            b"heic",
            b"heix",
            b"hevc",
            b"hevx",
            b"heim",
            b"heis",
        }:
            return "heic"
    return "unknown"


def is_native_telegram_photo(prefix: bytes) -> bool:
    detected = detect_image_format(prefix)
    if detected == "webp" and len(prefix) > 20 and prefix[12:16] == b"VP8X" and prefix[20] & 0x02:
        return False
    return detected in _NATIVE_PHOTO_FORMATS


def _init_image_worker() -> None:
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    register_heif_opener(thumbnails=False, decode_threads=1)
    Image.MAX_IMAGE_PIXELS = 100_000_000


def _warm_image_worker() -> None:
    """Force ProcessPoolExecutor workers to start during application startup."""


def _rgb_image(image: Image.Image) -> Image.Image:
    oriented = ImageOps.exif_transpose(image)
    if oriented.mode in {"RGBA", "LA"} or (
        oriented.mode == "P" and "transparency" in oriented.info
    ):
        rgba = oriented.convert("RGBA")
        background = Image.new("RGB", rgba.size, "white")
        background.paste(rgba, mask=rgba.getchannel("A"))
        return background
    if oriented.mode != "RGB":
        return oriented.convert("RGB")
    return oriented


def _fit_photo_dimensions(image: Image.Image) -> Image.Image:
    width, height = image.size
    if width + height > 10_000:
        scale = 10_000 / (width + height)
        target = (max(1, int(width * scale)), max(1, int(height * scale)))
        image = image.resize(target, Image.Resampling.BILINEAR)
        width, height = image.size
    ratio = max(width / height, height / width)
    if ratio <= 20:
        return image
    if width > height:
        target_size = (width, math.ceil(width / 20))
    else:
        target_size = (math.ceil(height / 20), height)
    background = Image.new("RGB", target_size, "white")
    offset = ((target_size[0] - width) // 2, (target_size[1] - height) // 2)
    background.paste(image, offset)
    return background


def _encode_jpeg(image: Image.Image, *, quality: int, subsampling: int) -> bytes:
    output = io.BytesIO()
    image.save(
        output,
        format="JPEG",
        quality=quality,
        subsampling=subsampling,
        optimize=False,
        progressive=False,
        exif=b"",
    )
    return output.getvalue()


def _normalize_photo_sync(data: bytes, filename: str) -> ConvertedImage:
    if detect_image_format(data[:32]) not in _NORMALIZABLE_PHOTO_FORMATS:
        raise ValueError("Photo format cannot be normalized")
    try:
        with Image.open(io.BytesIO(data)) as opened:
            opened.seek(0)
            image = _fit_photo_dimensions(_rgb_image(opened))
            encoded = _encode_jpeg(image, quality=85, subsampling=2)
            if len(encoded) > _PHOTO_MAX_BYTES:
                encoded = _encode_jpeg(image, quality=75, subsampling=2)
            if len(encoded) > _PHOTO_MAX_BYTES:
                scale = math.sqrt(_PHOTO_MAX_BYTES / len(encoded)) * 0.95
                target = (
                    max(1, int(image.width * scale)),
                    max(1, int(image.height * scale)),
                )
                image = image.resize(target, Image.Resampling.BILINEAR)
                encoded = _encode_jpeg(image, quality=75, subsampling=2)
            if len(encoded) > _PHOTO_MAX_BYTES:
                raise ValueError("Converted photo exceeds Telegram's size limit")
            stem = filename.rsplit(".", 1)[0] if "." in filename else filename
            return ConvertedImage(encoded, f"{stem}.jpg", "image/jpeg", *image.size)
    except Exception as exc:
        raise ValueError("Unsupported or corrupt image") from exc


def _convert_photo_sync(data: bytes, filename: str) -> ConvertedImage:
    if detect_image_format(data[:32]) not in _HEIC_PHOTO_FORMATS:
        raise ValueError("Only HEIC/HEIF photos may be converted")
    return _normalize_photo_sync(data, filename)


def _thumbnail_sync(data: bytes, filename: str) -> ConvertedImage:
    try:
        with Image.open(io.BytesIO(data)) as opened:
            opened.seek(0)
            if (
                opened.format == "JPEG"
                and opened.width <= 320
                and opened.height <= 320
                and len(data) < _THUMBNAIL_MAX_BYTES
            ):
                stem = filename.rsplit(".", 1)[0] if "." in filename else filename
                return ConvertedImage(
                    data, f"{stem}.jpg", "image/jpeg", opened.width, opened.height
                )
            image = _rgb_image(opened)
            image.thumbnail((320, 320), Image.Resampling.BILINEAR)
            encoded = b""
            for quality in (80, 65, 50):
                encoded = _encode_jpeg(image, quality=quality, subsampling=2)
                if len(encoded) < _THUMBNAIL_MAX_BYTES:
                    break
            if len(encoded) >= _THUMBNAIL_MAX_BYTES:
                image.thumbnail((256, 256), Image.Resampling.BILINEAR)
                encoded = _encode_jpeg(image, quality=50, subsampling=2)
            if len(encoded) >= _THUMBNAIL_MAX_BYTES:
                raise ValueError("Converted thumbnail exceeds Telegram's size limit")
            stem = filename.rsplit(".", 1)[0] if "." in filename else filename
            return ConvertedImage(encoded, f"{stem}.jpg", "image/jpeg", *image.size)
    except Exception as exc:
        raise ValueError("Unsupported or corrupt thumbnail") from exc


def _verified_jpeg_thumbnail(data: bytes, filename: str) -> ConvertedImage | None:
    if not data.startswith(b"\xff\xd8\xff") or len(data) >= _THUMBNAIL_MAX_BYTES:
        return None
    try:
        with Image.open(io.BytesIO(data)) as opened:
            if opened.format != "JPEG" or opened.width > 320 or opened.height > 320:
                return None
            opened.verify()
            stem = filename.rsplit(".", 1)[0] if "." in filename else filename
            return ConvertedImage(data, f"{stem}.jpg", "image/jpeg", opened.width, opened.height)
    except (OSError, ValueError):
        return None


def _read_file_sync(file: BinaryIO) -> bytes:
    file.seek(0)
    data = file.read()
    file.seek(0)
    return data


def _native_photo_is_compliant_sync(
    file: BinaryIO,
    size: int,
    detected_content_type: str,
    declared_content_type: str | None,
) -> bool:
    del detected_content_type, declared_content_type
    if size > _PHOTO_MAX_BYTES:
        return False
    try:
        file.seek(0)
        with Image.open(file) as image:
            width, height = image.size
            if getattr(image, "is_animated", False):
                return False
            if width < 1 or height < 1 or width + height > 10_000:
                return False
            return max(width / height, height / width) <= 20
    except (OSError, ValueError, ZeroDivisionError):
        return False
    finally:
        file.seek(0)


def image_worker_count(configured_workers: int, available_cpus: int | None = None) -> int:
    """Reserve one available CPU while allowing an explicit lower worker cap."""
    cpu_count = available_cpus if available_cpus is not None else os.process_cpu_count()
    cpu_limit = max(1, (cpu_count or 1) - 1)
    return cpu_limit if configured_workers == 0 else min(configured_workers, cpu_limit)


class ImagePreparationService:
    def __init__(self, settings: Settings) -> None:
        workers = image_worker_count(settings.image_conversion_workers)
        self._workers = workers
        self._semaphore = asyncio.Semaphore(workers)
        self._executor = ProcessPoolExecutor(
            max_workers=workers,
            mp_context=multiprocessing.get_context("spawn"),
            initializer=_init_image_worker,
        )
        log_event(
            logger,
            "image.worker_pool.started",
            message="Image conversion worker pool started",
            worker_count=workers,
        )

    async def warm(self) -> None:
        """Start HEIC workers before the first request pays their spawn cost."""
        started_at = perf_counter()
        loop = asyncio.get_running_loop()
        await asyncio.gather(
            *(
                loop.run_in_executor(self._executor, _warm_image_worker)
                for _ in range(self._workers)
            )
        )
        log_event(
            logger,
            "image.worker_pool.warmed",
            message="Image conversion worker pool warmed",
            worker_count=self._workers,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )

    async def read_file(self, file: BinaryIO) -> bytes:
        started_at = perf_counter()
        data = await asyncio.to_thread(_read_file_sync, file)
        log_event(
            logger,
            "image.file_read.completed",
            message="Image bytes read for preparation",
            output_bytes=len(data),
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return data

    async def native_photo_is_compliant(
        self,
        file: BinaryIO,
        size: int,
        detected_content_type: str,
        declared_content_type: str | None,
    ) -> bool:
        started_at = perf_counter()
        compliant = await asyncio.to_thread(
            _native_photo_is_compliant_sync,
            file,
            size,
            detected_content_type,
            declared_content_type,
        )
        log_event(
            logger,
            "image.native_validation.completed",
            message="Native Telegram photo validation completed",
            content_type=detected_content_type,
            request_bytes=size,
            compliant=compliant,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return compliant

    async def convert_photo(self, data: bytes, filename: str) -> ConvertedImage:
        started_at = perf_counter()
        detected = detect_image_format(data[:32])
        if detected not in _HEIC_PHOTO_FORMATS:
            raise ImageConversionError(f"Only HEIC/HEIF photos are converted; detected {detected}")
        async with self._semaphore:
            queue_wait = elapsed_ms(started_at)
            loop = asyncio.get_running_loop()
            try:
                result = await loop.run_in_executor(
                    self._executor, partial(_convert_photo_sync, data, filename)
                )
            except (OSError, RuntimeError, ValueError) as exc:
                log_event(
                    logger,
                    "image.photo_conversion.failed",
                    level=logging.WARNING,
                    message="Telegram photo conversion failed",
                    request_bytes=len(data),
                    queue_wait_ms=queue_wait,
                    elapsed_ms=elapsed_ms(started_at),
                    error_type=type(exc).__name__,
                    success=False,
                )
                raise ImageConversionError("Telegram photo conversion failed") from exc
        log_event(
            logger,
            "image.photo_conversion.completed",
            message="Telegram photo conversion completed",
            request_bytes=len(data),
            output_bytes=len(result.data),
            width=result.width,
            height=result.height,
            content_type=result.content_type,
            fast_path=False,
            queue_wait_ms=queue_wait,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return result

    async def normalize_photo(self, data: bytes, filename: str) -> ConvertedImage:
        """Normalize a decodable source photo into a Telegram-compatible JPEG."""
        started_at = perf_counter()
        detected = detect_image_format(data[:32])
        if detected not in _NATIVE_PHOTO_FORMATS | _FALLBACK_PHOTO_FORMATS:
            raise ImageConversionError(f"Photo format cannot be normalized: {detected}")
        async with self._semaphore:
            queue_wait = elapsed_ms(started_at)
            loop = asyncio.get_running_loop()
            try:
                result = await loop.run_in_executor(
                    self._executor, partial(_normalize_photo_sync, data, filename)
                )
            except (OSError, RuntimeError, ValueError) as exc:
                log_event(
                    logger,
                    "image.photo_normalization.failed",
                    level=logging.WARNING,
                    message="Telegram photo normalization failed",
                    request_bytes=len(data),
                    queue_wait_ms=queue_wait,
                    elapsed_ms=elapsed_ms(started_at),
                    error_type=type(exc).__name__,
                    success=False,
                )
                raise ImageConversionError("Telegram photo normalization failed") from exc
        log_event(
            logger,
            "image.photo_normalization.completed",
            message="Telegram photo normalization completed",
            request_bytes=len(data),
            output_bytes=len(result.data),
            width=result.width,
            height=result.height,
            content_type=result.content_type,
            fast_path=False,
            queue_wait_ms=queue_wait,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return result

    async def prepare_thumbnail(self, data: bytes, filename: str) -> ConvertedImage:
        started_at = perf_counter()
        detected = detect_image_format(data[:32])
        if detected not in _THUMBNAIL_INPUT_FORMATS:
            raise ImageConversionError(f"Unsupported Telegram thumbnail format: {detected}")
        verified = await asyncio.to_thread(_verified_jpeg_thumbnail, data, filename)
        if verified is not None:
            log_event(
                logger,
                "image.thumbnail_preparation.completed",
                message="Telegram thumbnail already compliant",
                request_bytes=len(data),
                output_bytes=len(verified.data),
                width=verified.width,
                height=verified.height,
                content_type=verified.content_type,
                fast_path=True,
                elapsed_ms=elapsed_ms(started_at),
                success=True,
            )
            return verified
        queue_started_at = perf_counter()
        async with self._semaphore:
            queue_wait = elapsed_ms(queue_started_at)
            try:
                if detected in _NATIVE_PHOTO_FORMATS:
                    result = await asyncio.to_thread(_thumbnail_sync, data, filename)
                    execution = "thread"
                else:
                    loop = asyncio.get_running_loop()
                    result = await loop.run_in_executor(
                        self._executor, partial(_thumbnail_sync, data, filename)
                    )
                    execution = "process"
            except (OSError, RuntimeError, ValueError) as exc:
                log_event(
                    logger,
                    "image.thumbnail_preparation.failed",
                    level=logging.WARNING,
                    message="Telegram thumbnail conversion failed",
                    request_bytes=len(data),
                    queue_wait_ms=queue_wait,
                    elapsed_ms=elapsed_ms(started_at),
                    error_type=type(exc).__name__,
                    success=False,
                )
                raise ImageConversionError("Telegram thumbnail conversion failed") from exc
        log_event(
            logger,
            "image.thumbnail_preparation.completed",
            message="Telegram thumbnail conversion completed",
            request_bytes=len(data),
            output_bytes=len(result.data),
            width=result.width,
            height=result.height,
            content_type=result.content_type,
            fast_path=False,
            execution=execution,
            queue_wait_ms=queue_wait,
            elapsed_ms=elapsed_ms(started_at),
            success=True,
        )
        return result

    async def close(self) -> None:
        await asyncio.to_thread(self._executor.shutdown, wait=True, cancel_futures=True)
