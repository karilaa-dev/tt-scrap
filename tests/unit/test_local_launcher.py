from __future__ import annotations

from pathlib import Path

import pytest

from tt_scrap.local import local_redis_command


def test_local_redis_command_disables_persistence(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setattr("tt_scrap.local.shutil.which", lambda _: "/usr/bin/redis-server")
    command = local_redis_command("redis://:secret@127.0.0.1:6380/0", tmp_path)
    config = (tmp_path / "redis.conf").read_text(encoding="utf-8")

    assert command[0] == "/usr/bin/redis-server"
    assert command == ["/usr/bin/redis-server", str(tmp_path / "redis.conf")]
    assert "port 6380" in config
    assert 'save ""' in config
    assert "appendonly no" in config
    assert 'requirepass "secret"' in config
    assert f'dir "{tmp_path}"' in config
    assert "secret" not in " ".join(command)
    assert (tmp_path / "redis.conf").stat().st_mode & 0o777 == 0o600


@pytest.mark.parametrize(
    "url",
    [
        "redis://:secret@redis.internal:6379/0",
        "rediss://:secret@127.0.0.1:6380/0",
        "redis://127.0.0.1:6380/0",
    ],
)
def test_local_launcher_rejects_unsafe_embedded_redis_configuration(
    monkeypatch, tmp_path: Path, url: str
) -> None:
    monkeypatch.setattr("tt_scrap.local.shutil.which", lambda _: "/usr/bin/redis-server")
    with pytest.raises(ValueError):
        local_redis_command(url, tmp_path)
