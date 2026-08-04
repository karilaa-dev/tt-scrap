"""Proxy loading, sanitization, stickiness, and rotation."""

from __future__ import annotations

import logging
import threading
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import quote, unquote, urlsplit

logger = logging.getLogger(__name__)


def strip_proxy_auth(proxy_url: str | None) -> str:
    if proxy_url is None:
        return "direct"
    parsed = urlsplit(proxy_url)
    if parsed.scheme not in {"http", "https", "socks5"} or not parsed.hostname:
        return "configured-proxy"
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    port = f":{parsed.port}" if parsed.port else ""
    return f"{parsed.scheme}://{host}{port}"


def encode_proxy_auth(proxy_url: str) -> str:
    parsed = urlsplit(proxy_url)
    if (
        parsed.scheme not in {"http", "https", "socks5"}
        or not parsed.hostname
        or parsed.username is None
    ):
        return proxy_url
    username = quote(unquote(parsed.username), safe="")
    password = quote(unquote(parsed.password or ""), safe="")
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    port = f":{parsed.port}" if parsed.port else ""
    return f"{parsed.scheme}://{username}:{password}@{host}{port}"


@dataclass(frozen=True, slots=True)
class ProxyChoice:
    slot: int | None
    url: str | None = field(repr=False)


class ProxyManager:
    def __init__(self, proxy_file: str = "", *, include_host: bool = False) -> None:
        proxies: list[str | None] = []
        if proxy_file:
            path = Path(proxy_file).expanduser().resolve()
            if not path.is_file():
                raise FileNotFoundError(f"Proxy file not found: {path}")
            for raw_line in path.read_text(encoding="utf-8").splitlines():
                line = raw_line.strip()
                if line and not line.startswith("#"):
                    proxies.append(encode_proxy_auth(line))
        if include_host:
            proxies.append(None)
        if not proxies:
            proxies = [None]
        self._proxies = tuple(proxies)
        self._next_slot = 0
        self._lock = threading.Lock()
        logger.info("Loaded %d proxy choices", len(self._proxies))

    @property
    def count(self) -> int:
        return len(self._proxies)

    def next(self) -> ProxyChoice:
        with self._lock:
            slot = self._next_slot
            self._next_slot = (slot + 1) % len(self._proxies)
        return ProxyChoice(slot=slot, url=self._proxies[slot])

    def from_slot(self, slot: int | None) -> ProxyChoice:
        if slot is None or slot < 0 or slot >= len(self._proxies):
            return self.next()
        return ProxyChoice(slot=slot, url=self._proxies[slot])

    def rotate(self, current: ProxyChoice) -> ProxyChoice:
        if current.slot is None:
            return self.next()
        slot = (current.slot + 1) % len(self._proxies)
        return ProxyChoice(slot=slot, url=self._proxies[slot])


@dataclass(slots=True)
class ProxySession:
    manager: ProxyManager
    choice: ProxyChoice | None = None

    def get(self) -> ProxyChoice:
        if self.choice is None:
            self.choice = self.manager.next()
        return self.choice

    def rotate(self) -> ProxyChoice:
        self.choice = self.manager.rotate(self.get())
        return self.choice
