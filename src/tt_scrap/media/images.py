"""Bounded image conversion for Telegram photo and thumbnail uploads."""

from __future__ import annotations

import asyncio
import io
import math
import multiprocessing
import os
import signal
from concurrent.futures import ProcessPoolExecutor
from dataclasses import dataclass
from functools import partial
from typing import BinaryIO, Literal

from PIL import Image, ImageOps
from pillow_heif import register_heif_opener

from ..config import Settings
from ..errors import ImageConversionError

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
_CONTENT_TYPE_ALIASES = {
    "image/jpg": "image/jpeg",
    "image/pjpeg": "image/jpeg",
    "image/x-png": "image/png",
}


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


def _convert_photo_sync(data: bytes, filename: str) -> ConvertedImage:
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


def _normalized_content_type(value: str | None) -> str | None:
    if value is None:
        return None
    normalized = value.split(";", 1)[0].strip().lower()
    return _CONTENT_TYPE_ALIASES.get(normalized, normalized)


def _native_photo_is_compliant_sync(
    file: BinaryIO,
    size: int,
    detected_content_type: str,
    declared_content_type: str | None,
) -> bool:
    if size > _PHOTO_MAX_BYTES:
        return False
    if declared_content_type is not None and _normalized_content_type(
        declared_content_type
    ) != _normalized_content_type(detected_content_type):
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


class ImagePreparationService:
    def __init__(self, settings: Settings) -> None:
        workers = min(settings.image_conversion_workers, os.cpu_count() or 1)
        self._semaphore = asyncio.Semaphore(workers)
        self._executor = ProcessPoolExecutor(
            max_workers=workers,
            mp_context=multiprocessing.get_context("spawn"),
            initializer=_init_image_worker,
        )

    async def read_file(self, file: BinaryIO) -> bytes:
        return await asyncio.to_thread(_read_file_sync, file)

    async def native_photo_is_compliant(
        self,
        file: BinaryIO,
        size: int,
        detected_content_type: str,
        declared_content_type: str | None,
    ) -> bool:
        return await asyncio.to_thread(
            _native_photo_is_compliant_sync,
            file,
            size,
            detected_content_type,
            declared_content_type,
        )

    async def convert_photo(self, data: bytes, filename: str) -> ConvertedImage:
        async with self._semaphore:
            loop = asyncio.get_running_loop()
            try:
                return await loop.run_in_executor(
                    self._executor, partial(_convert_photo_sync, data, filename)
                )
            except (OSError, RuntimeError, ValueError) as exc:
                raise ImageConversionError("Telegram photo conversion failed") from exc

    async def prepare_thumbnail(self, data: bytes, filename: str) -> ConvertedImage:
        verified = await asyncio.to_thread(_verified_jpeg_thumbnail, data, filename)
        if verified is not None:
            return verified
        async with self._semaphore:
            loop = asyncio.get_running_loop()
            try:
                return await loop.run_in_executor(
                    self._executor, partial(_thumbnail_sync, data, filename)
                )
            except (OSError, RuntimeError, ValueError) as exc:
                raise ImageConversionError("Telegram thumbnail conversion failed") from exc

    async def close(self) -> None:
        await asyncio.to_thread(self._executor.shutdown, wait=True, cancel_futures=True)
