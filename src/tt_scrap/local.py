"""Local launcher with an optional process-owned ephemeral Redis server."""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import tempfile
import time
from collections.abc import Sequence
from pathlib import Path
from urllib.parse import unquote, urlparse

import uvicorn
from redis import Redis
from redis.exceptions import RedisError

from .config import get_settings


def redis_is_ready(url: str) -> bool:
    client = Redis.from_url(
        url,
        socket_connect_timeout=0.25,
        socket_timeout=0.25,
        decode_responses=False,
    )
    try:
        return bool(client.ping())
    except RedisError:
        return False
    finally:
        client.close()


def local_redis_command(url: str, working_directory: Path) -> list[str]:
    parsed = urlparse(url)
    if parsed.scheme not in {"redis", "rediss"}:
        raise ValueError("REDIS_URL must use redis:// or rediss://")
    if parsed.scheme == "rediss":
        raise ValueError("tt-scrap-local cannot create a TLS Redis server")
    if parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise ValueError("tt-scrap-local only starts Redis on a loopback address")
    password = unquote(parsed.password or "")
    if not password:
        raise ValueError("Local REDIS_URL must include a password")
    if any(ord(character) < 32 for character in password):
        raise ValueError("Local Redis password cannot contain control characters")
    binary = shutil.which("redis-server")
    if not binary:
        raise FileNotFoundError(
            "redis-server is not installed; install Redis or point REDIS_URL at an existing server"
        )
    escaped_password = password.replace("\\", "\\\\").replace('"', '\\"')
    escaped_directory = str(working_directory).replace("\\", "\\\\").replace('"', '\\"')
    config = "\n".join(
        (
            f"bind {parsed.hostname}",
            f"port {parsed.port or 6379}",
            "protected-mode yes",
            'save ""',
            "appendonly no",
            f'requirepass "{escaped_password}"',
            f'dir "{escaped_directory}"',
            "daemonize no",
            "",
        )
    )
    config_path = working_directory / "redis.conf"
    descriptor = os.open(config_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as config_file:
        config_file.write(config)
    return [binary, str(config_path)]


def wait_for_redis(url: str, process: subprocess.Popen[bytes], timeout: float = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if redis_is_ready(url):
            return
        if process.poll() is not None:
            raise RuntimeError(f"redis-server exited with status {process.returncode}")
        time.sleep(0.05)
    raise TimeoutError("redis-server did not become ready within 5 seconds")


def stop_redis(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=2)


def _parse_args(arguments: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run tt-scrap and a persistence-disabled local Redis when needed"
    )
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument(
        "--no-start-redis",
        action="store_true",
        help="Require REDIS_URL to already be reachable",
    )
    return parser.parse_args(arguments)


def run(arguments: Sequence[str] | None = None) -> None:
    args = _parse_args(arguments)
    settings = get_settings()
    redis_url = settings.redis_url.get_secret_value()
    redis_process: subprocess.Popen[bytes] | None = None
    temporary_directory: tempfile.TemporaryDirectory[str] | None = None
    try:
        if not redis_is_ready(redis_url):
            if args.no_start_redis:
                raise RuntimeError("REDIS_URL is not reachable")
            temporary_directory = tempfile.TemporaryDirectory(prefix="tt-scrap-redis-")
            command = local_redis_command(redis_url, Path(temporary_directory.name))
            redis_process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
            )
            wait_for_redis(redis_url, redis_process)
        uvicorn.run("tt_scrap.main:app", host=args.host, port=args.port, workers=1)
    finally:
        if redis_process is not None:
            stop_redis(redis_process)
        if temporary_directory is not None:
            temporary_directory.cleanup()


if __name__ == "__main__":
    run()
