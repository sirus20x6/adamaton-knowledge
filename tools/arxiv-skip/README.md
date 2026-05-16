# arxiv-skip

A tiny Zig HTTP sidecar that pre-flights arxiv papers to detect whether
they have LaTeX source available. The reindex worker uses this to
short-circuit the ~86% of papers in the reindex backlog that have no
usable LaTeX upstream, dropping per-skip cost from ~30s of full
deepresearch round-trip to ~200ms of HEAD against arxiv.

## API

```
POST /check
Content-Type: application/json
{"arxiv_id": "2411.06989"}

→ 200 OK
{"arxiv_id": "2411.06989", "has_latex": true,  "reason": "...", "elapsed_ms": 142}
{"arxiv_id": "9999.99999",  "has_latex": false, "reason": "withdrawn or not found", "elapsed_ms": 110}

→ 502 (transient upstream)
{"arxiv_id": "...", "error": "arxiv timeout"}

GET /health  → 200 "ok"
GET /metrics → 200 placeholder
```

`has_latex=true` means we observed an `application/gzip` (or
`application/x-eprint*`) response, or a `content-disposition` filename
ending in `.tar.gz` / `.gz`. `has_latex=false` covers PDF-only
submissions, 404 (withdrawn), 410 (gone), and unknown content-types.
5xx / network failures surface as 502 so the Go caller can retry.

## Build

Local:

```
cd tools/arxiv-skip
zig build           # → zig-out/bin/arxiv-skip
zig build test      # unit tests (classify, isValidArxivId, fake fetcher)
```

Cross-compile for the Pi:

```
make pi-arxiv-skip
file tools/arxiv-skip/zig-out/bin/arxiv-skip
# ELF 64-bit LSB executable, ARM aarch64, ... statically linked, stripped
```

The Makefile target uses `-Dtarget=aarch64-linux-musl -Doptimize=ReleaseSmall`
so the resulting binary drops into the alpine docker image without any
glibc/musl version-skew worries.

## Configuration

Environment:

- `ARXIV_SKIP_PORT` — listener port. Defaults to `9871`. Listens on
  `0.0.0.0` (so docker-compose siblings can reach it).

There is no persistent state — the Go side has its own cache in
`evo.reindex_skipped`. Re-deploys are free.

## Deploy on the Pi

1. Cross-compile: `make pi-arxiv-skip`.
2. Copy `tools/arxiv-skip/zig-out/bin/arxiv-skip` next to the
   `tools/arxiv-skip/Dockerfile` on the Pi and `docker build -t
   evo-arxiv-skip:dev .`.
3. Add to your Pi `docker-compose.yml`:

```yaml
arxiv-skip:
  image: evo-arxiv-skip:dev
  restart: unless-stopped
  environment:
    ARXIV_SKIP_PORT: "9871"
  expose: ["9871"]
  networks: [default]

reindex-worker:
  environment:
    ARXIV_SKIP_URL: "http://arxiv-skip:9871"
```

4. `docker compose up -d arxiv-skip reindex-worker`. With
   `ARXIV_SKIP_URL` set, the worker calls the sidecar before every
   ingest attempt and skips the heavy pipeline whenever the verdict is
   `has_latex=false`.

## Failure mode: fail-open

If `ARXIV_SKIP_URL` is unset, the worker calls
`SkipClient == nil` → no preflight, full slow path. If the sidecar is
configured but returns an error, the activity logs and falls through —
the slow path still runs. The sidecar's only job is to *save* work,
never to block it.

## Memory & concurrency

- Workers are bounded to 32 concurrent via `Io.Semaphore`. Beyond that
  the kernel listen backlog absorbs the queue.
- Each worker uses an HTTP client per request (no shared connection
  pool yet — arxiv rate-limits long before a pool would help, and
  thread-local state is simpler).
- Resident memory should sit well under 50 MB on the Pi 5.
