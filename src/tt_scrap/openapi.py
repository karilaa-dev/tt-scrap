"""Generate a deterministic OpenAPI document from the application routes."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from .app import create_app
from .config import Settings

_SCHEMA_GENERATION_KEY = "openapi-schema-generation-key"


def build_openapi_schema(settings: Settings | None = None) -> dict[str, Any]:
    """Build the same schema served by the application's /openapi.json route."""
    configured = settings or Settings.model_validate({"tt_scrap_api_key": _SCHEMA_GENERATION_KEY})
    return create_app(configured).openapi()


def export_openapi_schema(output: Path, settings: Settings | None = None) -> None:
    """Write the current application schema as formatted JSON."""
    schema = build_openapi_schema(settings)
    output.write_text(
        json.dumps(schema, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def run() -> None:
    parser = argparse.ArgumentParser(
        description="Export tt-scrap's current FastAPI OpenAPI schema",
    )
    parser.add_argument(
        "output",
        nargs="?",
        type=Path,
        default=Path("openapi.json"),
        help="output path (default: openapi.json)",
    )
    args = parser.parse_args()
    export_openapi_schema(args.output)
    print(args.output.resolve())


if __name__ == "__main__":
    run()
