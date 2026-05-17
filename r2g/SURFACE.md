# r2g Surface Audit

Phase 0 deliverable for the R2R → Go port. Source-of-truth list of every
endpoint our code calls on the R2R service (`/v3/*`) and on our own
Python platform wrapper (`/platform/*`).

The audit was conducted by grepping the evo monorepo (`reindex/`,
`skills/`, `dispatch/`, `evolve/`, `dashboard/`, `workflow-builder/`,
`r2g/`), the Python platform backend (`deepresearch/platform/backend/`),
and the React SPA (`/thearray/scratch/dr-frontend/src/`).

## Key distinction: R2R vs platform wrapper

The deepresearch container we run on the Pi (`deepresearch-backend-1`)
holds **two things**:

1. **R2R proper** — vendored from SciPhi-AI at `/thearray/git/deepresearch/R2R/py/`,
   exposes `/v3/*` routes. This is what r2g replaces.
2. **Our platform wrapper** — `/thearray/git/deepresearch/platform/backend/app/`,
   exposes `/platform/*` routes. We own this code. It imports R2R as a
   Python library AND has its own FastAPI surface.

For the Phase 1–7 port, **r2g replaces only #1**. The platform wrapper
in #2 keeps serving `/platform/*`, but switches from importing R2R
in-process to calling r2g over HTTP. That alone removes the R2R-loaded
embedding models, chunking buffers, and Hatchet machinery from the
backend container — most of the Pi memory win.

A future phase (not Phase 1–7) can decide whether to also port the
platform wrapper to Go; until then it stays Python.

## R2R `/v3/*` endpoints we depend on (port target for r2g)

| Method | Path | Caller | Hot? | Notes |
|--------|------|--------|------|-------|
| POST   | `/v3/documents`               | `platform/backend/app/research/swarm.py`, `platform/backend/app/api/sources.py`, `platform/backend/app/workers/hi_worker.py`, `evo/reindex/activities/ingest.go` (indirectly via `/platform/sources/ingest`) | Yes | Multipart or JSON. Returns immediately; chunks/embeddings written async in current R2R. r2g should make this synchronous. |
| POST   | `/v3/documents/{id}/extract`  | `evo/reindex/activities/ingest.go:259`, `platform/backend/app/workers/hi_worker.py`, `platform/backend/app/kg/live.py` | Yes | LLM-driven entity + relationship extraction. Today fires off Hatchet workflow under R2R; r2g should bound concurrency and gate on chunks-actually-present. |
| DELETE | `/v3/documents/{id}`          | `evo/reindex/activities/ingest.go:303` | Yes | 404 → treat as success. Used during reindex rewire to clean up old doc. |
| POST   | `/v3/retrieval/search`        | `evo/dashboard/apiserver/skills_search_graph.go:119`, `platform/backend/app/research/swarm.py` | Yes | Hybrid search (BM25 + embedding). Pgvector ANN + tsvector FTS. Optional `search_settings.graph_vote: bool` overrides `R2G_GRAPH_VOTE_ENABLED` for the request (TGS-RAG Graph-to-Text). |
| GET    | `/v3/documents`               | SPA `src/api/library.ts:284` | Yes | List with pagination, filter by collection_id and ingestion_status. |
| GET    | `/v3/chunks`                  | SPA `src/features/library/DocumentReader.tsx:95` | Yes | Filter by document_id, paginated. |

**Deferred R2R endpoint:**
- GET `/v3/graphs/{corpus}/relationships` — graph visualization (best-effort; failures don't block). Used only by the dashboard skills graph view.

## Platform `/platform/*` endpoints — KEEP (stay in Python for now)

These are not R2R. They live in the platform wrapper and either call
R2R internally or talk to our own postgres schema. Tagged **keep**
because the Pi/dashboard hot paths need them; they'll move to Go later
or never.

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/platform/sources/ingest` | Wraps `POST /v3/documents` with LaTeX detection + figure extraction + corpus attachment. Reindex pipeline hits this. |
| POST | `/platform/sources/upload` | File upload to temp storage. |
| POST | `/platform/skills/ingest` | Skill uploads (`evo/skills/activities/r2r.go:91`). |
| DELETE | `/platform/skills/{id}` | Skill cleanup (`evo/skills/activities/r2r.go:122`). |
| GET, POST | `/platform/corpora/` | List + create corpora. |
| GET, PATCH, DELETE | `/platform/corpora/{id}` | CRUD per corpus. |
| GET, POST | `/platform/corpora/{id}/documents` | List/attach docs in a corpus. |
| POST | `/platform/research/` | Submit a deep-research task. |
| GET | `/platform/research/{task_id}` | Poll research task status. |
| POST | `/platform/research/{task_id}/cancel` | Cancel research task. |
| POST | `/platform/search` | Live multi-adapter search (arxiv, openalex, searxng). |

## DEFER — features we might want later

Graph exploration, books, zotero, plugins, experiments, GraphRAG,
trust scoring, research execution log, sources catalog. All currently
in the platform wrapper. None are on the hot path of reindex or the
core dashboard SPA. They can stay in Python indefinitely.

Conversations / agent / RAG tools (per the user's note) fall here too
— defer until we decide we need them.

## DROP — features we've turned off or don't use

- `/platform/health`, `/platform/health/live`, `/platform/health/ready` — replaced by direct DB/service pings via `evo-api/api/v1/system/status`.
- `/platform/settings/` — runtime admin config, not used by us.
- R2R `/v3/conversations/*`, `/v3/agent/*`, `/v3/users/*`, `/v3/prompts/*` — never called.
- R2R auth — `require_authentication=false` in `platform.toml`.

## What this means for r2g

- **Phase 1 scope is tiny**: 6 endpoints, all read-or-CRUD against
  the shared postgres + pgvector.
- **Phase 2 scope is the meat**: `POST /v3/documents` must run the
  full parse → chunk → embed pipeline. `POST /v3/documents/{id}/extract`
  must run the LLM graph extraction. These are the ones that matter
  for memory + correctness.
- **Phase 7 retires R2R, not the whole backend container**: the
  platform wrapper keeps running. Memory savings come from R2R's
  models + buffers no longer being loaded in-process; the FastAPI
  wrapper itself is light.
- **Future option**: port the platform wrapper to Go later. The
  /platform/* surface above is the audit you'd start with.

## Deployment record (2026-05-14)

r2g shipped to the Pi at commit da95a9f, running alongside R2R as
`deepresearch-r2g-1`. Image: `evo-r2g:dev`. Listens on :7373 (public)
and :7374 (pprof + /metrics).

Service block injected into `~/deepresearch/docker-compose.yml` on
the Pi (the compose file is Pi-only, not tracked in any repo):

```yaml
  r2g:
    image: evo-r2g:dev
    env_file: [./platform/.env]
    environment:
      R2G_DATABASE_URL: "postgresql://postgres:postgres@postgres:5432/postgres?sslmode=disable"
      R2G_PORT: "7373"
      R2G_PPROF_PORT: "7374"
      R2G_LOG_FORMAT: "json"
      BGE_SIDECAR_URL: "http://10.0.4.37:8089"
      LATEX2TEXT_BIN: "/app/bin/latex2text"
      OPENAI_API_BASE: "http://10.0.4.37:9080/v1"
      R2G_LLM_MODEL: "qwen3.6-27b-nvfp8"
    extra_hosts:
      - "workstation:10.0.4.37"
    depends_on:
      postgres:
        condition: service_healthy
    expose: ["7373", "7374"]
    volumes:
      - ./platform/backend:/app:ro
    restart: unless-stopped
```

Verification:
- /health -> 200 ok
- /ready -> all three checks green (postgres + bge_sidecar + llm)
- GET /v3/documents/{id} byte-identical to R2R for the same doc
- GET /v3/chunks?document_id=... filters correctly (R2R bug fixed)

Memory:
- r2g VmRSS: 34 MB
- R2R Python: 730 MB

Not yet:
- Phase 3 extract live verification (requires vLLM idle window)
- Retire R2R container (Phase 7) -- waiting for soak

## Full cutover (2026-05-14)

Caddy now routes the 6 r2g endpoints to `r2g:7373`:

```caddyfile
@r2g {
    path /v3/documents /v3/documents/* /v3/chunks /v3/chunks/* /v3/retrieval/search
}
handle @r2g {
    reverse_proxy r2g:7373 {
        transport http { read_timeout 600s }
    }
}
```

Anything else under `/v3/*` (graphs, conversations, deferred) still
falls through to the existing `@api` block which targets R2R.

The platform Python wrapper also switched: `R2R_INTERNAL_URL` env on
both `backend` and `worker-lo` services now points at `http://r2g:7373`
(was unset; defaulted to `http://backend:7272`). The Python code at
`platform/backend/app/api/sources.py` and `app/workers/hi_worker.py`
that wraps `/platform/sources/ingest`, /sync, /skills, etc. now
internally talks to r2g for chunking + storage. Restart of `backend`
+ `worker-lo` containers picked it up.

All 6 endpoints verified through `https://deepresearch.local/v3/*`:

| Endpoint              | Status | Latency |
|-----------------------|--------|---------|
| GET  /v3/documents     | 200    | 225 ms |
| GET  /v3/documents/{id} | 200   |  18 ms |
| GET  /v3/chunks?...     | 200   |  45 ms |
| POST /v3/retrieval/search | 200 | 3.6 s  |
| POST /v3/documents      | 200   | 2.1 s  |
| DELETE /v3/documents/{id} | 200 |  40 ms |

Rollback: delete the `@r2g` block from `~/deepresearch/infra/Caddyfile`
(one paste from `Caddyfile.bak.*`) + `docker compose exec frontend caddy
reload --config /etc/caddy/Caddyfile`, then unset
`R2R_INTERNAL_URL` in `docker-compose.yml` + restart backend +
worker-lo. ~30 seconds total. No data migration ever -- same postgres
+ same schema.

## Post-cutover hardening (2026-05-14, commit 560fe16)

First soak found two compat issues:

1. r2g was writing `collection_ids=NULL, owner_id=NULL` when the
   caller didn't supply them. R2R's pydantic `DocumentResponse`
   model requires non-NULL on both, so its
   `/v3/documents/{id}/extract` endpoint (still reached by the
   reindex worker via PLATFORM_URL=backend:7272, bypassing Caddy)
   crashed with `ValidationError` whenever it tried to extract a
   doc r2g had written. Fixed in r2g/routes/ingest.go: default
   collection_ids to `[]` and owner_id to
   `R2G_DEFAULT_OWNER_ID` (set to the deepresearch superuser
   `2acb499e-8428-543b-bd85-0d9098718220` in the compose env).

2. latex2text's `\input{Sections/0_abstract}` resolver only tried
   exact + suffix matches. About 50% of recent arxiv papers split
   sections into a `Sections/` subdir and use `\input{}` without
   `.tex` extension, so r2g 422'd them while pandoc would have
   parsed fine. Fixed in tools/latex2text/src/main.zig:
   resolveInclude now tries `<name>.tex` first per the TeX
   convention. tar.zig got a new `findByPath` helper for path-
   prefixed lookups. Sample retest: 6/6 papers now parse cleanly
   (was 3/6 before).

After both fixes shipped (binary at /app/bin/latex2text replaced,
r2g image rebuilt), `R2R_INTERNAL_URL=http://r2g:7373` was
re-enabled on both backend + worker-lo services so the platform
wrapper's internal calls flow through r2g end-to-end.

## qwen3 thinking-mode + token-budget fix (2026-05-14)

ExtractCorpusWorkflow sweeps initially produced `entities=0
relationships=0` despite successful LLM round-trips. Root cause was
two interacting facts about the workstation's vLLM-served
qwen3.6-27b-nvfp8:

1. qwen3.x defaults to **thinking mode**, returning structured
   chain-of-thought in a separate `reasoning` field and leaving
   `content` null until the CoT terminates. The r2g llm client only
   read `content`, so a CoT cut off by `max_tokens` produced an
   empty answer string and zero parsed entities.
2. With thinking explicitly disabled (`chat_template_kwargs.enable_thinking=false`),
   the model emits one well-formed entity, then degenerates into a
   repetitive loop. The 27B fp8 quant needs CoT to stay coherent for
   structured XML generation.

Fix (r2g/internal/llm/client.go, r2g/internal/extract/extract.go):
- Leave thinking enabled (default).
- Bumped `DefaultMaxTokens` from 8192 → 24576 so CoT + answer both
  fit. Reduced `DefaultMaxBatchTokens` 24000 → 16000 to compensate
  on the input side.
- Llm client now falls back to `reasoning` when `content` is empty
  (defense-in-depth for variant vLLM response shapes).

Verified: doc `0012cb27-fcdd-4fc6-855c-c216a18f69ee` ("Attention Is
All You Need") went from 0 entities to 12 entities + 11 relationships
in a single 5471-token LLM call.

## Schema-enforced JSON output (2026-05-14, commit 0ad7ee6)

Post-soak the sweep still produced `entities=0` on most documents.
Replay against vLLM showed qwen3.6-27b-nvfp8 ignores our XML
schema regardless of prompt strength: sometimes Markdown bullets,
sometimes hallucinated nested tags, sometimes repetitive
degeneration on short inputs. Few-shot anchoring didn't hold;
stronger "XML only" instructions made it worse.

Switched to vLLM's `guided_json` -- outlines/lm-format-enforcer
rails the decoder so output must be valid JSON matching the schema:

```json
{
  "entities":      [{"name","type","description"}],
  "relationships": [{"source","target","relation","description","weight"}]
}
```

The r2g extractor now `json.Unmarshal`s the response and maps fields
1:1 onto the existing `Entity`/`Relationship` structs that persist
into `deepresearch.documents_entities` / `..._relationships`.

Changes:
- `r2g/internal/llm/client.go`: `CompletionOptions.GuidedJSON` added;
  client passes it as the vLLM extra-body param. Non-vLLM backends
  ignore the unknown key.
- `r2g/internal/extract/extract.go`: replaced XML regex parser with
  JSON decoder. `DefaultMaxBatchTokens` reduced to 12000 to leave more
  output headroom for verbose schema output.
- `r2g/internal/extract/prompt.go`: shrunk to a single short few-shot
  anchoring entity granularity; the schema does the structural work.

Verified on doc `94d92fdc-766b-406e-9eaf-543642ee99e1` (BERT abstract,
225 input tokens): **12 entities + 17 relationships** (was 0 of both
under the XML prompt).
