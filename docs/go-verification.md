# Go project verification

Verified on 2026-10-13 with Go 1.27.2, CGO, libheif, and FFmpeg. The final repository checks ran from an export of the Git index into a clean directory, without local Python or Node tooling.

| Check | Result |
| --- | --- |
| `make check` | Formatting, module consistency, vet, race tests, and release build passed |
| Identity database tests | Passed against a disposable PostgreSQL 18 container |
| Production Docker build | Passed from the clean source export |
| Container startup | App and web modes passed readiness, authentication, validation, and built-in health checks |
| Public OpenAPI | Byte-identical to the original contract and the served container response |
| App startup | Zero identities registered before a request |
| Container user and shutdown | UID 10001; both modes exited successfully on SIGTERM |
| Compose configuration | Base file and optional proxy override validated |
| GitHub Actions workflows | Both passed actionlint |
| Source and credentials | All executable project source is Go; no configured private credentials found in the staged files or compressed reports |

The clean export caught and fixed a root-level credential ignore rule that previously hid the Go cookie source files. Both files are now part of the repository. The Docker build context allows only source and build inputs.

The race suite covers HTTP contracts, media conversion, streamed downloads, protocol vectors, and identity ownership, persistence, balancing, lazy renewal, recovery, cancellation, and checkpoint failures. Live upstream tests remain opt-in. No live Instagram request or Telegram upload is claimed; those integrations use controlled HTTP fixtures.

## Earlier live checks

Before the final repository cleanup, native app and web tests extracted and downloaded the same 1080×1920 public video. Both produced **5,597,812 bytes** with SHA-256 `708f4144d1c517d0e9e81c42fa0cd39f9f1d8403b13913c3a804a057f9c3a9bf`. A separate native app feed test returned six playable videos. The final cleanup changed Go module imports and project files, not extraction behavior.

The [Go/Python comparison](go-python-comparison.md) and [app optimization report](go-app-optimization.md) retain measured CPU, memory, latency, concurrency, failures, and limitations. Their binary hashes refer to the measured snapshots before the module rename. The reference implementations and old cross-runtime harnesses are archived outside this Go-only repository; see [baseline provenance](benchmark-baselines.md).
