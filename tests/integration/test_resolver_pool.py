from __future__ import annotations

import subprocess
import sys
from pathlib import Path


def test_real_connect_tls_failures_do_not_poison_subsequent_resolution() -> None:
    root = Path(__file__).resolve().parents[2]
    result = subprocess.run(
        [sys.executable, str(root / "scripts/diagnostics/repro_tiktok_proxy_pool.py")],
        cwd=root,
        capture_output=True,
        text=True,
        timeout=15,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert "request=3 resolved=True" in result.stdout
