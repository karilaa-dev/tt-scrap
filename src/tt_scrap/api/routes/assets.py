from __future__ import annotations

from collections.abc import Iterator

from fastapi import APIRouter, Depends, Request
from fastapi.responses import StreamingResponse

from ...media.downloader import filename_for_type
from ..dependencies import require_api_key

router = APIRouter(
    prefix="/v1/assets",
    tags=["assets"],
    dependencies=[Depends(require_api_key)],
)


@router.get("/{token}")
async def get_asset(token: str, request: Request) -> StreamingResponse:
    context = await request.app.state.cache.get_asset(token)
    downloaded = await request.app.state.asset_downloader.download(context)
    filename = filename_for_type(context.filename, downloaded.content_type)

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
