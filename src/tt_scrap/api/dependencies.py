"""Route dependencies."""

from __future__ import annotations

import secrets
from typing import Annotated

from fastapi import Depends, Request
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from ..errors import AuthenticationError

_bearer = HTTPBearer(auto_error=False)


async def require_api_key(
    request: Request,
    credentials: Annotated[HTTPAuthorizationCredentials | None, Depends(_bearer)],
) -> None:
    expected = request.app.state.settings.tt_scrap_api_key.get_secret_value()
    provided = (
        credentials.credentials if credentials and credentials.scheme.lower() == "bearer" else ""
    )
    if not provided or not secrets.compare_digest(provided, expected):
        raise AuthenticationError("A valid bearer token is required")
