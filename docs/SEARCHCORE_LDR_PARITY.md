# searchcore ⟷ LDR — Phase B.9 parity matrix

> **Purpose.** This is the retirement checklist for the upstream Python LDR
> (LearningCircuit/local-deep-research) deployed on pi5. LDR is **transient**:
> it ships the deep-research retrieval path *today* via an HTTP bridge while
> the Go-native `searchcore` module is finished. When every row in the
> **Parity gaps** table below is closed (or explicitly waived), the **B.9
> parity gate** passes and the pi5 `ldr` docker service + Caddy `ldr.local`
> vhost + the `LDRDeepSearch` bridge activity get archived.
>
> Cross-references:
> - `~/.claude/projects/-thearray-git-Adamaton/memory/ldr_deploy_state.md`
>   (the `ldr-deploy-state` memory — Phase A/B coexistence, deploy PRs).
> - `deepresearch/searchcore/README.md` + `doc.go` — searchcore's own phasing.
> - `~/.claude/plans/frolicking-churning-bubble.md` — the umbrella roadmap the
>   B-phase task IDs (B.2–B.9, C.6/C.8) refer to.
>
> **Scope note.** `searchcore` physically lives in
> `deepresearch/searchcore/` (a Go module under the *deepresearch* sub-repo),
> not under `knowledge/`. This doc lives in `knowledge/docs/` because the
> knowledge sub-repo owns the RAG / retrieval data plane and the r2g adapter
> (`deepresearch/searchcore/sources/r2g.go`) that bridges searchcore to
> knowledge's curated corpus. Keep the two in sync when either side moves.

## The two coexisting paths through nano-research's `DEEP_SEARCH` stage

Driven by `deepresearch/nano-research/workflows/stage_deepsearch.go`. The
stage reads two config keys and picks a path:

| Config key | Value | Path taken |
|---|---|---|
| `searchcore_strategy` | non-empty strategy id | **Phase B** — `ExecuteChildWorkflow(StrategyDispatchWorkflow)` into a Go strategy. Takes precedence. |
| `use_deep_search` | `true` (and no strategy id) | **Phase A** — `LDRDeepSearch` activity → HTTP bridge to the Python LDR service. |
| neither set | — | stage returns `Skipped=true`, no external call. |

Both paths persist a `summary.md` artifact and feed the same downstream
nano-research stages, so the rest of the pipeline never branches on which
retrieval engine ran. That is what makes LDR safely swappable.

## What searchcore already has (parity achieved)

Grounded in `deepresearch/searchcore/*`:

| Capability | LDR (Python) | searchcore (Go) | Evidence |
|---|---|---|---|
| Strategy: iterative refinement | `iterative_refinement_strategy.py` | `strategies/iterative_refinement.go` | registered, tested (`*_test.go`) |
| Strategy: IterDRAG question chain | `iterdrag_strategy.py` | `strategies/iterdrag.go` | derives_from edge chain = question tree |
| Strategy: dual-confidence + rejection | `dual_confidence_with_rejection.py` | `strategies/dual_confidence.go` | two-pass + Jaccard agree/contradict |
| Strategy: parallel-constrained | `parallel_constrained_strategy.py` | `strategies/parallel_constrained.go` | K sub-Qs, elaborates/derives_from edges |
| Strategy: evidence-based v2 | `evidence_based_strategy_v2.py` | `strategies/evidence_based_v2.go` | per-evidence stance, evidence×trust ranking |
| Source: web search (SearxNG) | searxng engine | `sources/searxng.go` | JSON API, trust 0.4 |
| Source: arXiv primary literature | arxiv engine | `sources/arxiv.go` | Atom-XML API, trust 0.85 |
| Source: GitHub code/repo | github engine | `sources/github.go` | `gh` CLI wrapper, trust 0.6 |
| Source: curated corpus retrieval | (LDR "local docs") | `sources/r2g.go` | r2g `/v3/retrieval/search` (pgvector+BM25+RRF), trust 0.7 |
| Structured output | free-text findings | **claim/evidence graph** | `evo.research_claims / research_evidence / claim_edges` via `ClaimSink` |
| Temporal orchestration | external Python process polled over HTTP | native child workflows | `workflows/dispatch.go`, per-strategy workflows |
| Memory priors | context-prefix prepend only | `MemorySnapshot` (insights + prior claims) | `types.go` `Input.Memory`, capped 100/200 |
| Budget envelope | iterations / questions_per_iteration | `Budget{MaxLLMTokens, MaxAdapterCalls, MaxWallClock}` | `types.go` |

searchcore is **structurally ahead** of LDR in one respect that matters for
the rest of Adamaton: it emits a typed claim/evidence/edge graph rather than
free-text findings, which is what the evals (`deepsearch-bench`) and
downstream synthesis consume. That is the whole reason the port exists.

## Parity gaps — the B.9 retirement checklist

Each row is something the deployed Python LDR does (or the bench needs) that
searchcore does **not yet** do. Closing all of these (or waiving with a
recorded reason) is the gate to archive LDR. Grounded in the `Phase C` /
`TODO` / `not yet` markers in `deepresearch/searchcore/*`.

| # | Gap | LDR has it | searchcore status | Where it lands | Source-of-truth |
|---|---|---|---|---|---|
| G1 | **Deep-read / full-content fetch.** LDR follows a result URL and reads full document content, not just the snippet/abstract. | yes | snippet/abstract only; `AdapterHit.ArtifactPath=""` for web; arXiv stops at abstract | Phase C deep-read activity | `sources/searxng.go:88`, `sources/arxiv.go:24,137,141` (`TODO(phase-c)`) |
| G2 | **Confidence scoring / calibration.** LDR scores answer confidence; searchcore only records raw `SourceUsage`. | yes | `SourceUsage` is logged but not scored; no calibration activity | Phase C confidence-calibration | `types.go:96`, `README.md:32` |
| G3 | **Evidence `stance` column.** evidence-based v2 wants SUPPORT/CONTRADICT/IRRELEVANT persisted; Phase B has no column. | yes (in-strategy) | polarity smuggled in-band via a `"(contradicts) "` snippet prefix | Phase C schema migration | `strategies/evidence_based_v2.go:24,215`, `types.go:126` |
| G4 | **Adversarial / counter-search replay.** `ModeCounterSearch` red-teams an existing claim subgraph. | partial (rejection logic) | `Mode` enum reserves `ModeCounterSearch` but no strategy implements it | Phase C C.6/C.8 | `types.go:25-32`, `dual_confidence.go:19` |
| G5 | **Multi-engine search.** LDR bridge accepts `search_engines[]` (defaults `["searxng"]`, can fan to others). searchcore strategies hard-call `"searxng"`. | yes | every strategy calls adapter id `"searxng"` directly; no per-call engine override | Phase C planner adapter routing | `dual_confidence.go:60`, `iterdrag.go:91`, `evidence_based_v2.go:49,62` |
| G6 | **GovStats / gov-data sources.** | yes (data.gov etc.) | `sources/govstats.go` is a **registered placeholder** — `Search` returns empty, `Fetch` returns `ErrNotApplicable` | Phase C concrete endpoints | `sources/govstats.go` |
| G7 | **arXiv ingestion into corpus.** LDR's local-docs path can ingest fetched papers; searchcore's arXiv adapter does not POST to r2g. | yes | `Fetch` returns abstract only; no HTTP-GET→latex2text→`POST /v3/documents` | Phase C deep-read | `sources/arxiv.go:137,141` |
| G8 | **True parallel sub-question fan-out.** `parallel_constrained` is named "parallel" but runs **sequentially** (Temporal determinism). | yes (process-level) | sequential by design; real fan-out deferred to workflow level | B.6 / Phase C planner | `strategies/parallel_constrained.go:22-27` |
| G9 | **skills-rae prompt compile.** strategy prompts should compile via skills-rae; Phase B stubs it. | n/a (LDR has own prompts) | `wfSkillsClient` is a **stub** | Phase B/C wiring | `workflows/deps.go:71` |
| G10 | **Continue-as-new carry-state for unbounded strategies.** | n/a (Python loops freely) | `dual_confidence` warns it has no carry-state CAN (bounded loop only, OK for now) | Phase C if any strategy unbounds | `workflows/strategy_dual_confidence.go:28` |
| G11 | **Source-trust learning.** LDR trust is static per engine; the design reserves learned trust. | no (both static today) | trust hardcoded per adapter; learned `evo.source_trust` deferred | Phase C | `sources/types.go:23` |
| G12 | **`deepsearch-bench` searchcore arm.** The 4-arm harness can run `ldr-raw`, `nano-no-ds`, `nano-with-ds`, but the **searchcore arm is short-circuited** — the parity gate can't be measured head-to-head yet. | n/a | `buildArm` has no `searchcore` case; `nano-with-ds` arm is itself stubbed pending A.2/A.9 wiring | **B.9 blocker** | `cmd/deepsearch-bench/arms.go:30-46,311-365` |

### Gate semantics

The B.9 gate is **quantitative**: run `deepsearch-bench` with the
searchcore arm against the LDR arm (`ldr-raw`) on the in-house dataset and
confirm searchcore is at least at parity on the judged metrics. That gate is
**currently un-runnable** because of **G12** — wiring a `searchcore` arm into
`deepsearch-bench` is the precondition for measuring any of the other gaps.
Treat G12 as the first item on the checklist.

Gaps that can be **waived** (don't block archival of the *Python service*,
even if still open as Phase-C work):
- **G8** (parallel fan-out) — a performance property, not a correctness one;
  sequential parallel-constrained still emits the same graph.
- **G9** (skills-rae compile) — internal prompt sourcing; LDR doesn't have it
  either, so it isn't a parity item, only a polish item.
- **G10** (carry-state CAN) — the only affected strategy is bounded today.
- **G11** (trust learning) — LDR's trust is also static, so this is
  feature-ahead work, not a parity gap.

Gaps that **must** close (or be explicitly accepted as a quality regression)
before LDR is archived, because they change retrieval *quality* the bench
will see: **G1** (deep-read), **G2** (confidence), **G5** (multi-engine),
**G7** (arXiv ingest). **G3/G4/G6** are graph-richness items — needed for
full Phase C value but not strictly for "searchcore answers at least as well
as LDR" on the bench. **G12** is the measurement precondition for all of the
above.

## Retirement steps once the gate passes

When the gate is green, archive the Phase-A path (per the `ldr-deploy-state`
memory, these are the moving parts):

1. Remove the pi5 `ldr` docker-compose service
   (`localdeepresearch/local-deep-research:latest`).
2. Remove the Caddy `ldr.local` vhost + `X-LDR-API-Key` edge auth.
3. Retire the bridge activity `LDRDeepSearch`
   (`deepresearch/nano-research/activities/ldrdeepsearch.go`) and its
   `use_deep_search` config branch in `stage_deepsearch.go`.
4. Drop the `LDR_BASE_URL` / `LDR_API_KEY` env + `deploy/pi5/LDR_BRINGUP.md`
   runbook.
5. Leave the `searchcore_strategy` path as the sole DEEP_SEARCH retrieval
   engine.

Until every "must-close" gap above is addressed, **do not** archive LDR and
**do not** bake long-term assumptions on the Python service surviving.
