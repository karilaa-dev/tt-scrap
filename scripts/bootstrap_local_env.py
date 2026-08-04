"""Provision ignored local tt-scrap secrets from an existing tt-bot checkout."""

from __future__ import annotations

import argparse
import secrets
import shutil
from pathlib import Path

from cryptography.fernet import Fernet
from dotenv import dotenv_values


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("source", type=Path, help="Path to the tt-bot checkout")
    parser.add_argument("--target", type=Path, default=Path.cwd())
    args = parser.parse_args()

    source = args.source.resolve()
    target = args.target.resolve()
    existing = dotenv_values(source / ".env")
    redis_password = secrets.token_urlsafe(32)
    values = {
        "TT_SCRAP_API_KEY": secrets.token_urlsafe(32),
        "CACHE_ENCRYPTION_KEY": Fernet.generate_key().decode(),
        "REDIS_PASSWORD": redis_password,
        "REDIS_URL": f"redis://:{redis_password}@127.0.0.1:6379/0",
        "RAPIDAPI_KEY": existing.get("RAPIDAPI_KEY", ""),
        "YTDLP_COOKIES": "cookies.txt",
        "PROXY_FILE": "proxies.txt",
        "PROXY_DATA_ONLY": existing.get("PROXY_DATA_ONLY", "false"),
        "PROXY_INCLUDE_HOST": existing.get("PROXY_INCLUDE_HOST", "false"),
        "URL_RESOLVE_MAX_RETRIES": existing.get("URL_RESOLVE_MAX_RETRIES", "3"),
        "VIDEO_INFO_MAX_RETRIES": existing.get("VIDEO_INFO_MAX_RETRIES", "3"),
        "DOWNLOAD_MAX_RETRIES": existing.get("DOWNLOAD_MAX_RETRIES", "3"),
        "MAX_VIDEO_DURATION": existing.get("MAX_VIDEO_DURATION", "0"),
        "CACHE_TTL_SECONDS": "600",
        "LOG_LEVEL": existing.get("LOG_LEVEL", "INFO"),
    }
    target.mkdir(parents=True, exist_ok=True)
    rendered = "\n".join(f"{key}={value or ''}" for key, value in values.items()) + "\n"
    (target / ".env").write_text(rendered, encoding="utf-8")
    for filename in ("cookies.txt", "proxies.txt"):
        source_file = source / filename
        if source_file.is_file():
            shutil.copyfile(source_file, target / filename)
    print(f"Provisioned ignored local configuration in {target}")


if __name__ == "__main__":
    main()
