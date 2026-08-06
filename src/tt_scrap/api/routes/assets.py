from __future__ import annotations

from collections.abc import Iterator

from fastapi import APIRouter, Depends, Request
from fastapi.responses import StreamingResponse

from ...media.downloader import filename_for_type
from ..dependencies import require_api_key
from ..responses import AUTHENTICATED_RESPONSES

_BINARY_MEDIA_TYPES = (
    "application/octet-stream",
    "video/mp4",
    "audio/mpeg",
    "audio/mp4",
    "image/jpeg",
    "image/png",
    "image/webp",
    "image/heic",
    "image/heif",
    "image/avif",
    "image/tiff",
    "image/bmp",
    "image/gif",
)
_BINARY_CONTENT = {
    media_type: {"schema": {"type": "string", "format": "binary"}}
    for media_type in _BINARY_MEDIA_TYPES
}

router = APIRouter(
    prefix="/v1/assets",
    tags=["assets"],
    dependencies=[Depends(require_api_key)],
    responses=AUTHENTICATED_RESPONSES,
)


@router.get(
    "/{token}",
    operation_id="downloadAsset",
    response_class=StreamingResponse,
    responses={
        200: {
            "description": "Resolved media bytes; Content-Type reflects the detected format",
            "content": _BINARY_CONTENT,
            "headers": {
                "Content-Length": {
                    "description": "Exact response size in bytes",
                    "schema": {"type": "integer"},
                },
                "Content-Disposition": {
                    "description": "Attachment filename with a detected-format extension",
                    "schema": {"type": "string"},
                },
                "ETag": {
                    "description": "Quoted SHA-256 digest",
                    "schema": {"type": "string"},
                },
                "X-Content-SHA256": {
                    "description": "Unquoted SHA-256 digest",
                    "schema": {"type": "string"},
                },
                "Cache-Control": {
                    "description": "Always private, no-store",
                    "schema": {"type": "string", "example": "private, no-store"},
                },
                "X-Request-ID": {
                    "description": "Request correlation identifier",
                    "schema": {"type": "string"},
                },
            },
        }
    },
)
async def get_asset(token: str, request: Request) -> StreamingResponse:
    """Download an opaque asset URL returned by an extraction response.

    Send the same tt-scrap bearer token used for extraction. The token is temporary;
    download before the descriptor's `expires_at`. For TikTok adaptive video, this
    request also downloads the separate audio and performs a stream-copy MP4 mux
    before responding, so the downloaded video is playable and contains audio.
    """
    context = await request.app.state.cache.get_asset(token)
    downloaded = await request.app.state.asset_downloader.download(context)
    filename = filename_for_type(context.filename, downloaded.content_type)
    if downloaded.sha256 is None:
        raise RuntimeError("Asset download did not produce a checksum")

    def chunks() -> Iterator[bytes]:
        try:
            while chunk := downloaded.file.read(64 * 1024):
                yield chunk
        finally:
            downloaded.file.close()

    return StreamingResponse(
        chunks(),
        media_type=downloaded.content_type,
        headers={
            "Content-Length": str(downloaded.size),
            "Content-Disposition": f'attachment; filename="{filename}"',
            "ETag": f'"{downloaded.sha256}"',
            "X-Content-SHA256": downloaded.sha256,
            "Cache-Control": "private, no-store",
        },
    )
