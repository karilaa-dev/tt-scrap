"""Compare photographic JPEG and PNG conversion latency and output size."""

from __future__ import annotations

import argparse
import io
import statistics
import time
from pathlib import Path

from PIL import Image, ImageOps
from pillow_heif import register_heif_opener


def _load_rgb(source: bytes) -> Image.Image:
    with Image.open(io.BytesIO(source)) as image:
        image.seek(0)
        oriented = ImageOps.exif_transpose(image)
        oriented.load()
        if oriented.mode in {"RGBA", "LA"} or "transparency" in oriented.info:
            rgba = oriented.convert("RGBA")
            background = Image.new("RGB", rgba.size, "white")
            background.paste(rgba, mask=rgba.getchannel("A"))
            return background
        return oriented.convert("RGB")


def _encode(source: bytes, output_format: str) -> bytes:
    image = _load_rgb(source)
    try:
        output = io.BytesIO()
        if output_format == "JPEG":
            image.save(
                output,
                format="JPEG",
                quality=85,
                subsampling=2,
                optimize=False,
                progressive=False,
            )
        else:
            image.save(output, format="PNG", optimize=False)
        return output.getvalue()
    finally:
        image.close()


def _measure(source: bytes, output_format: str, repetitions: int) -> tuple[float, int]:
    elapsed: list[float] = []
    output_size = 0
    for _ in range(repetitions):
        started = time.perf_counter()
        output_size = len(_encode(source, output_format))
        elapsed.append(time.perf_counter() - started)
    return statistics.median(elapsed), output_size


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Benchmark one HEIC/HEIF/AVIF or other photographic input"
    )
    parser.add_argument("input", type=Path)
    parser.add_argument("--repetitions", type=int, default=5)
    args = parser.parse_args()
    if args.repetitions < 1:
        parser.error("--repetitions must be at least 1")

    register_heif_opener(thumbnails=False, decode_threads=1)
    source = args.input.read_bytes()
    for output_format in ("JPEG", "PNG"):
        seconds, output_size = _measure(source, output_format, args.repetitions)
        print(f"{output_format}: median={seconds:.4f}s size={output_size:,} bytes")


if __name__ == "__main__":
    main()
