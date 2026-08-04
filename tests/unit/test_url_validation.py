from __future__ import annotations

import pytest

from tt_scrap.errors import InvalidLinkError
from tt_scrap.platforms.instagram import validate_instagram_url
from tt_scrap.platforms.tiktok.adapter import validate_tiktok_url


@pytest.mark.parametrize(
    "url",
    [
        "https://www.tiktok.com/@user/video/123",
        "https://vm.tiktok.com/example/",
        "https://www.tiktok.com/@user/photo/123",
    ],
)
def test_tiktok_urls_are_accepted(url: str) -> None:
    validate_tiktok_url(url)


@pytest.mark.parametrize(
    "url",
    [
        "http://www.tiktok.com/@user/video/123",
        "https://tiktok.com.evil.test/@user/video/123",
        "file:///etc/passwd",
    ],
)
def test_tiktok_ssrf_urls_are_rejected(url: str) -> None:
    with pytest.raises(InvalidLinkError):
        validate_tiktok_url(url)


@pytest.mark.parametrize(
    "url",
    [
        "https://www.instagram.com/reel/ABC-123/",
        "https://instagram.com/p/ABC_123/",
        "https://www.instagram.com/stories/user/",
    ],
)
def test_instagram_urls_are_accepted(url: str) -> None:
    validate_instagram_url(url)


@pytest.mark.parametrize(
    "url",
    [
        "http://www.instagram.com/p/ABC/",
        "https://instagram.com.evil.test/p/ABC/",
        "https://www.instagram.com/accounts/login/",
    ],
)
def test_instagram_ssrf_urls_are_rejected(url: str) -> None:
    with pytest.raises(InvalidLinkError):
        validate_instagram_url(url)
