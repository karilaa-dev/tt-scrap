# Restored Python comparison baseline

The historical comparison used the latest Python service from before the rewrite, with both TikTok backends: Python plus a persistent Node protocol worker for app requests, and Python with yt-dlp/curl-cffi for web requests.

All 59 Python and compiled JavaScript source files match the saved baseline. The aggregate SHA256 is `e77026374e775af43988ca810ed1dae88c5f303c01b8b27457d1781a03d3eb45`. Sort paths within `src/**/*.py` and `tiktok-app/dist/**/*.js`, hash each file, join `SHA256  relative/path` records with newlines without a trailing newline, then hash the UTF-8 result. The original locked dependencies remain unchanged.

Measured runtimes are Go 1.27.2, Python 3.13.14 and Node 24.18.0. Python uses FastAPI 0.141.1, Uvicorn 0.52.1, curl-cffi 0.15.0 and yt-dlp 2026.8.19. The app comparison includes its Node child process in whole-service CPU and memory measurements.

The Go and Python services use separate disposable databases cloned from one configured proxy and its ten registered identities. Seeds are renewed before measurement. The benchmark does not run the historical identity manager against the active service database. All sides use the same proxy, public videos, explicit concurrency settings and fresh metadata requests.

The public endpoint benchmark includes normal signing, quality probes and persistence. The isolated API benchmark excludes registration, seed renewal, signing and database work: a preparation process hands signed requests to a separate Go or Node HTTP process. Python web remains a separate extractor process. Every timed response is fully read and validated; errors remain visible. Startup and warmup are outside the timed windows.

The Python service, Node worker, cross-runtime harnesses, earlier reports, and measured Go snapshot were preserved in a local reference archive outside this Go-only repository. They are not required for the current build or tests. The latest Python app baseline was never committed, so Git history alone cannot reproduce that comparison. The raw evidence and source hash above remain available in the [Go comparison](go-python-comparison.md).

Current Go benchmarks run with `make bench`. `cmd/bench-api` remains a Go-only executor for prepared upstream requests; see [Contributing](../CONTRIBUTING.md). Cross-runtime reruns require the archived reference implementations and harnesses.
