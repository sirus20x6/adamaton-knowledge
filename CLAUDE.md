# Agent instructions — adamaton-knowledge

> **🚫 BLOCKING — never commit to `main` directly.** Adamaton's pre-commit hook hard-rejects commits to `main`/`master` from the canonical checkout. Always start with:
> ```bash
> cd /thearray/git/Adamaton && bin/adam claim 'knowledge'/<task>
> ```
> This creates a worktree on a feature branch where you can work freely. If you accidentally edited main, run `bin/adam rescue` from the umbrella to recover. See [umbrella CLAUDE.md](../CLAUDE.md) for the full agent workflow.

Four Go modules (skills, skills-rae, reindex, r2g) + two Zig sidecars (latex2text, arxiv-skip). Owns the RAG / graphrag / memory data plane.

## Before you edit

From the umbrella:

```bash
bin/adam claim knowledge/<task>
```

## Build / test

```bash
# from any module dir
go build ./... && go test ./...

# zig sidecars
cd tools/latex2text && zig build && zig build test
cd tools/arxiv-skip  && zig build && zig build test
```

From the umbrella, `go build ./...` covers all four Go modules via `go.work`.

### libztok (cgo) is required to build r2g + reindex

`r2g/chunking` tokenizes via the `ztok` cgo binding (the umbrella's `ztok`
submodule, `github.com/sirus20x6/ztok-go`), so building r2g or reindex now
needs `libztok` + `ztok.h` on the cgo paths. Build it once from the submodule
and point pkg-config at it:

```bash
# from the umbrella root
cd ztok && zig build -Doptimize=ReleaseFast -p "$PWD/zig-out" && cd -
export PKG_CONFIG_PATH="$ADAM_ROOT/ztok/zig-out/lib/pkgconfig"
export LD_LIBRARY_PATH="$ADAM_ROOT/ztok/zig-out/lib"   # for the .so at run/test time
```

The Docker images don't need this — their `ztoklib` build stage compiles
`libztok.a` with Zig and static-links it (`-tags nopkgconfig` + `CGO_*FLAGS`).
ztok's cl100k output is byte-identical to tiktoken's, so the swap moved no
chunk boundaries and required no reindex (see `r2g/chunking/chunker_parity_test.go`).

**Both build modes are exercised by `r2g/internal/ingestpipeline/pipeline_test.go`**
(the parse→chunk→embed→persist correctness path). It builds a real
ztok-backed `chunking.Chunker`, so it fails loudly if the cgo link is
broken under either mode:

```bash
# local pkg-config / shared .so (the dev path above)
GOWORK=off CGO_ENABLED=1 GOFLAGS=-mod=mod \
  PKG_CONFIG_PATH="$ADAM_ROOT/ztok/zig-out/lib/pkgconfig" \
  LD_LIBRARY_PATH="$ADAM_ROOT/ztok/zig-out/lib" \
  go test ./internal/ingestpipeline/...

# Docker static-link path (libztok.a, no .so) — mirrors the image
zig build -Doptimize=ReleaseFast -p /tmp/ztok-static   # then rm libztok.so*
GOWORK=off CGO_ENABLED=1 GOFLAGS=-mod=mod \
  CGO_CFLAGS="-I/tmp/ztok-static/include" CGO_LDFLAGS="-L/tmp/ztok-static/lib" \
  go build -tags nopkgconfig ./cmd/r2g
```

The DB-backed cases (happy path, embedder-error, already-ingested) stand
up an ephemeral paradedb via `core/pgutil.NewTestDB` and a fake
OpenAI-shaped embedding sidecar; they `t.Skip` when Docker is absent, so
the parse/chunk/validation branches still run on a daemon-less CI box.

## Gotchas

- **Each subdir is its own Go module.** `cd skills && go test ./...` and `cd r2g && go test ./...` are separate test runs. CI per-module pins use the tagged version of `adamaton-core`; local dev resolves via the umbrella `go.work`.
- **`octen` returns 1024-dim vectors** (memory: `octen_embedding_dim`). Any `vector(N)` migration in `r2g/migrations/` or `reindex/sql/` must match.
- **skills-rae listens on `:7376`**. Don't pick another port without updating `platform.dashboard` + `deepresearch` callers + the umbrella deploy compose.
- **r2g replaces R2R `/v3/*` only** — the platform wrapper at `/platform/*` is still Python (see `r2g/SURFACE.md`). Don't bake `/platform/*` semantics into r2g.
- **Migrations are `golang-migrate`-style** (`NNN_name.up.sql` / `.down.sql`). The runner is in `core/pgutil`. Apply via the binary's `--migrate` flag, not by hand.
- **Embedding model swaps cascade.** If you change the model in `core/octen`, you must reindex everything by re-running the `reindex` worker. Bump the model version field rather than swapping in place.

## Universal rules

See `../CLAUDE.md` in the umbrella.
