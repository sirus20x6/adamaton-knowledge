# adamomaton-knowledge

RAG, graphrag, and skill-memory backends. Four Go modules + two Zig sidecars. Owns the schema for `evo.skills`, `evo.skill_communities`, `evo.insights`, plus corpora/documents/chunks/figures.

## Layout

| Dir | Module | Purpose |
|---|---|---|
| `skills/` | `adamomaton-knowledge/skills` | Skill-bank Temporal activities + workflows (skill ingestion, dedup, embedding) |
| `skills-rae/` | `adamomaton-knowledge/skills-rae` | Retrieval-Augmented-Editor HTTP service. Listens on **:7376**. Endpoints: `/v1/rae/compile`, `/v1/rae/search`, `/v1/rae/communities`, `/v1/rae/skills/*` |
| `reindex/` | `adamomaton-knowledge/reindex` | Periodic re-embed/community-detection worker. SQL in `reindex/sql/` |
| `r2g/` | `adamomaton-knowledge/r2g` | Go port of R2R. HTTP `/v3/*` (replacing the Python R2R container). See `r2g/SURFACE.md` for the audited endpoint surface |
| `tools/latex2text/` | Zig | LaTeX → plain text sidecar used by reindex (avoids Python texlive deps) |
| `tools/arxiv-skip/` | Zig | Arxiv paper-id deduplication helper |

## Build

Each module is independent:

```bash
cd skills && go build ./...
cd skills-rae && go build ./...
cd reindex && go build ./...
cd r2g && go build ./...

# Zig sidecars
cd tools/latex2text && zig build
cd tools/arxiv-skip  && zig build
```

From the umbrella, `go.work` covers all four Go modules so `go build ./...` at the umbrella root works.

## Test

```bash
cd <module> && go test ./...
```

`r2g` integration tests need a live Postgres + pgvector; see `r2g/testdata/`.

## Dev DSN / endpoints

- Postgres: `postgres://evo:evo@localhost:5432/evo` (workstation dev) — provisioned by the umbrella `bin/adam deploy workstation`
- skills-rae HTTP: `:7376`
- r2g HTTP: replaces the Python R2R container; default `:7272` when run standalone

Migrations:

- `r2g/migrations/` — corpora / documents / chunks
- `reindex/sql/` — community detection materialised views

## Where this fits

`deepresearch` calls `skills-rae` for per-stage skill context. `platform.dashboard` calls `skills-rae` for the `/skills` and `/memory` pages, and `r2g` for the `/library` page. `evolve` and `deepresearch` both write to `evo.insights` directly via SQL. Depends only on `adamomaton-core`. See `docs/ARCHITECTURE.md` in the umbrella.
