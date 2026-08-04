from __future__ import annotations

from pathlib import Path

from tt_scrap.proxy import (
    ProxyManager,
    ProxySession,
    encode_proxy_auth,
    strip_proxy_auth,
)


def test_proxy_auth_is_encoded_and_redacted() -> None:
    proxy = encode_proxy_auth("http://user@example.com:p@ss@proxy.test:8080")
    assert proxy == "http://user%40example.com:p%40ss@proxy.test:8080"
    assert strip_proxy_auth(proxy) == "http://proxy.test:8080"


def test_proxy_session_is_sticky_then_rotates(tmp_path: Path) -> None:
    proxy_file = tmp_path / "proxies.txt"
    proxy_file.write_text(
        "# ignored\nhttp://one.test:8000\nhttp://two.test:8000\n",
        encoding="utf-8",
    )
    manager = ProxyManager(str(proxy_file), include_host=True)
    session = ProxySession(manager)

    first = session.get()
    assert session.get() == first
    second = session.rotate()
    third = session.rotate()

    assert first.url == "http://one.test:8000"
    assert second.url == "http://two.test:8000"
    assert third.url is None


def test_missing_proxy_configuration_uses_direct_connection() -> None:
    manager = ProxyManager()
    assert manager.next().url is None
