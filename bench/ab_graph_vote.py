"""A/B benchmark: TGS-RAG Graph-to-Text Global Voting on/off via r2g.

Hits r2g's POST /v3/retrieval/search twice per query — once with
search_settings.graph_vote=false (baseline) and once with
search_settings.graph_vote=true (treatment) — then reports recall@k
and MRR@10 deltas. Single process, no operator-coordinated r2g
restart: the per-request override (wired in r2g/routes/search.go on
this branch) lets us A/B both states in one run.

Plan: /home/sirus/.claude/plans/mellow-doodling-tiger.md (Component 2).
TGS-RAG paper: https://arxiv.org/abs/2605.05643

See also: bench/ab_rerank.py for the rerank-sidecar A/B; this bench
is the retrieval-path A/B (full r2g pipeline: semantic + BM25 + RRF
+ optional graph-vote + cross-encoder rerank).
"""

from __future__ import annotations

import argparse
import asyncio
import os
import statistics
import sys
import time
from typing import Any

import httpx
import psycopg
from psycopg.rows import dict_row


DB_URL = os.environ.get(
    "PLATFORM_DATABASE_URL",
    "postgresql://postgres:postgres@postgres:5432/postgres",
).replace("postgresql+psycopg://", "postgresql://")

DEFAULT_R2G_URL = os.environ.get("R2G_URL", "http://localhost:7373")
DEFAULT_TIMEOUT_S = 120.0  # retrieval can be slow with reranker on cold start
SEARCH_LIMIT = 20  # matches r2g "advanced" default


def parse_args() -> argparse.Namespace:
    """CLI surface for the graph-vote A/B bench."""
    p = argparse.ArgumentParser(
        description=(
            "Measure TGS-RAG Graph-to-Text impact on retrieval quality.\n\n"
            "A/Bs in a single process by passing search_settings.graph_vote "
            "per request (no r2g restart required). Baseline pass is "
            "graph_vote=false; treatment is graph_vote=true. Same query "
            "set is reused for both passes."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--r2g-url",
        metavar="URL",
        default=DEFAULT_R2G_URL,
        help=f"base URL for r2g (default: env R2G_URL or {DEFAULT_R2G_URL!s})",
    )
    p.add_argument(
        "--limit",
        metavar="N",
        type=int,
        default=50,
        help="max queries to evaluate (default 50; 0 = unlimited)",
    )
    p.add_argument(
        "--corpus",
        metavar="UUID",
        default=None,
        help=(
            "optional corpus UUID to scope retrieval via "
            "collection_ids.$overlap (default: unscoped — searches across "
            "all corpora the chunks belong to)"
        ),
    )
    p.add_argument(
        "--top-k",
        metavar="LIST",
        default="1,5,10",
        help='comma-separated cutoffs for recall@k (default "1,5,10")',
    )
    p.add_argument(
        "--concurrency",
        metavar="N",
        type=int,
        default=8,
        help="max concurrent requests (default 8)",
    )
    return p.parse_args()


def parse_top_ks(spec: str) -> list[int]:
    """Parse the --top-k comma-separated list into a sorted ascending list."""
    out: list[int] = []
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            n = int(part)
        except ValueError as e:
            raise SystemExit(f"--top-k: invalid integer {part!r}") from e
        if n <= 0:
            raise SystemExit(f"--top-k: cutoffs must be positive, got {n}")
        out.append(n)
    if not out:
        raise SystemExit("--top-k: at least one cutoff required")
    return sorted(set(out))


def extract_gold_doc_ids(result: dict[str, Any]) -> list[str]:
    """Pull the canonical paper_answers list — those are the docs the swarm
    actually used to write the synthesis, so they're our gold standard."""
    pa = result.get("paper_answers") or []
    seen: set[str] = set()
    out: list[str] = []
    for item in pa:
        doc = item.get("document_id")
        if doc and doc not in seen:
            seen.add(doc)
            out.append(doc)
    return out


async def fetch_eval_set(conn, limit: int) -> list[dict[str, Any]]:
    """Reuse the same eval set ab_rerank.py uses: deduped past
    research_jobs with >=4 gold docs. When ``limit > 0`` we cap the outer
    query; ``limit == 0`` returns everything available."""
    sql = """
    WITH dedup AS (
      SELECT DISTINCT ON (query) id, query, result
      FROM platform.research_jobs
      WHERE status='succeeded'
        AND query NOT IN ('x', 'test')
        AND length(query) > 10
      ORDER BY query, jsonb_array_length(COALESCE(result->'paper_answers', '[]'::jsonb)) DESC
    )
    SELECT id, query, result FROM dedup
    WHERE jsonb_array_length(COALESCE(result->'paper_answers', '[]'::jsonb)) >= 4
    ORDER BY jsonb_array_length(COALESCE(result->'paper_answers', '[]'::jsonb)) DESC
    """
    params: tuple[Any, ...] = ()
    if limit > 0:
        sql += "\n    LIMIT %s"
        params = (limit,)
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql, params)
        rows = await cur.fetchall()
    out: list[dict[str, Any]] = []
    for r in rows:
        gold = extract_gold_doc_ids(r["result"])
        if len(gold) >= 4:
            # cap excessive gold sets — 277 papers means the synth was sloppy
            gold = gold[:40]
            out.append({"id": str(r["id"]), "query": r["query"], "gold": gold})
    return out


async def search_via_r2g(
    client: httpx.AsyncClient,
    base: str,
    query: str,
    corpus: str | None,
    graph_vote: bool,
    limit: int = SEARCH_LIMIT,
) -> tuple[list[str], float]:
    """POST one query to /v3/retrieval/search and return
    ``(ordered_doc_ids, elapsed_ms)``. Chunks are walked in response
    order, deduped by document_id (highest-scoring chunk per doc wins),
    mirroring reranked_doc_order in bench/ab_rerank.py."""
    body: dict[str, Any] = {
        "query": query,
        "search_mode": "advanced",
        "search_settings": {
            "limit": limit,
            "graph_vote": graph_vote,
        },
    }
    if corpus:
        body["search_settings"]["filters"] = {
            "collection_ids": {"$overlap": [corpus]}
        }
    t0 = time.perf_counter()
    r = await client.post(
        f"{base.rstrip('/')}/v3/retrieval/search",
        json=body,
        timeout=DEFAULT_TIMEOUT_S,
    )
    elapsed_ms = (time.perf_counter() - t0) * 1000.0
    r.raise_for_status()
    data = r.json()
    chunks = (data.get("results") or {}).get("chunk_search_results") or []
    seen: set[str] = set()
    ordered: list[str] = []
    for chunk in chunks:
        doc = chunk.get("document_id")
        if not doc or doc in seen:
            continue
        seen.add(doc)
        ordered.append(doc)
    return ordered, elapsed_ms


def recall_at_k(predicted: list[str], gold: set[str], k: int) -> float:
    if not gold:
        return 0.0
    hits = sum(1 for d in predicted[:k] if d in gold)
    return hits / min(len(gold), k)


def mrr_at_k(predicted: list[str], gold: set[str], k: int) -> float:
    for i, d in enumerate(predicted[:k], start=1):
        if d in gold:
            return 1.0 / i
    return 0.0


async def _eval_one(
    sem: asyncio.Semaphore,
    client: httpx.AsyncClient,
    base: str,
    ex: dict[str, Any],
    corpus: str | None,
    graph_vote: bool,
    top_ks: list[int],
) -> dict[str, Any]:
    """Run one query, returning per-query metrics. HTTP/timeout failures
    log to stderr and degrade to zero hits so a single bad query doesn't
    abort the pass."""
    gold = set(ex["gold"])
    async with sem:
        try:
            predicted, elapsed_ms = await search_via_r2g(
                client, base, ex["query"], corpus, graph_vote
            )
        except Exception as exc:  # noqa: BLE001 — same broad-catch policy as siblings
            print(
                f"[warn] query {ex['id']} graph_vote={graph_vote} failed: {exc!r}",
                file=sys.stderr,
                flush=True,
            )
            predicted = []
            elapsed_ms = 0.0
    row: dict[str, Any] = {
        "id": ex["id"],
        "n_predicted": len(predicted),
        "latency_ms": elapsed_ms,
    }
    for k in top_ks:
        row[f"r@{k}"] = recall_at_k(predicted, gold, k)
    row["mrr@10"] = mrr_at_k(predicted, gold, 10)
    return row


async def run_pass(
    client: httpx.AsyncClient,
    base: str,
    eval_set: list[dict[str, Any]],
    corpus: str | None,
    graph_vote: bool,
    top_ks: list[int],
    concurrency: int,
) -> dict[str, Any]:
    """Async-fanout one A/B pass across ``eval_set``. Returns a dict of
    aggregate metrics: ``n``, per-cutoff ``r@k``, ``mrr@10``,
    ``latency_ms``, ``zero_hit_queries``."""
    label = "on" if graph_vote else "off"
    print(
        f"[pass graph_vote={label}] running {len(eval_set)} queries "
        f"(concurrency={concurrency})",
        flush=True,
    )
    sem = asyncio.Semaphore(max(1, concurrency))
    rows = await asyncio.gather(
        *(
            _eval_one(sem, client, base, ex, corpus, graph_vote, top_ks)
            for ex in eval_set
        )
    )
    agg: dict[str, Any] = {
        "label": label,
        "graph_vote": graph_vote,
        "n": len(rows),
        "zero_hit_queries": sum(1 for r in rows if r["n_predicted"] == 0),
    }
    if not rows:
        agg["latency_ms"] = 0.0
        for k in top_ks:
            agg[f"r@{k}"] = 0.0
        agg["mrr@10"] = 0.0
        return agg
    for k in top_ks:
        agg[f"r@{k}"] = statistics.mean(r[f"r@{k}"] for r in rows)
    agg["mrr@10"] = statistics.mean(r["mrr@10"] for r in rows)
    # Latency mean excludes failed (0.0) requests so a wedge doesn't
    # silently pull the number down; if everything failed we report 0.
    good_lat = [r["latency_ms"] for r in rows if r["latency_ms"] > 0.0]
    agg["latency_ms"] = statistics.mean(good_lat) if good_lat else 0.0
    return agg


def print_pass_summary(label: str, agg: dict[str, Any], top_ks: list[int]) -> None:
    """Pretty-print one A/B pass."""
    print()
    print("=" * 60)
    print(f"PASS  graph_vote={label}  (n={agg['n']})")
    print("=" * 60)
    for k in top_ks:
        print(f"  recall@{k:<3}  {agg[f'r@{k}']:.4f}")
    print(f"  mrr@10     {agg['mrr@10']:.4f}")
    print(f"  latency_ms {agg['latency_ms']:.1f}")
    print(f"  zero-hit queries: {agg['zero_hit_queries']} / {agg['n']}")


def print_delta(
    baseline: dict[str, Any],
    treatment: dict[str, Any],
    top_ks: list[int],
) -> None:
    """Print a side-by-side off→on summary. Column widths are fixed so
    the table is grep-friendly regardless of magnitude."""
    print()
    print("=" * 60)
    print("DELTA  graph_vote=off  →  graph_vote=on")
    print("=" * 60)
    print(f"{'metric':<14}{'off':>10}{'on':>10}{'Δ':>10}")
    print("─" * 44)

    def row(name: str, off_v: float, on_v: float) -> None:
        delta = on_v - off_v
        sign = "+" if delta >= 0 else ""
        print(
            f"{name:<14}{off_v:>10.4f}{on_v:>10.4f}"
            f"{sign + f'{delta:.4f}':>10}"
        )

    for k in top_ks:
        row(f"recall@{k}", baseline[f"r@{k}"], treatment[f"r@{k}"])
    row("mrr@10", baseline["mrr@10"], treatment["mrr@10"])

    # Latency row uses 1-decimal precision and ms units; format inline
    # so it slots into the same column widths as the recall rows.
    off_lat = baseline["latency_ms"]
    on_lat = treatment["latency_ms"]
    d_lat = on_lat - off_lat
    sign = "+" if d_lat >= 0 else ""
    print(
        f"{'latency_ms':<14}{off_lat:>10.1f}{on_lat:>10.1f}"
        f"{sign + f'{d_lat:.1f}':>10}"
    )
    print()
    print(
        f"queries (off={baseline['n']}, on={treatment['n']})  "
        f"zero-hit (off={baseline['zero_hit_queries']}, "
        f"on={treatment['zero_hit_queries']})"
    )


async def main() -> None:
    args = parse_args()
    top_ks = parse_top_ks(args.top_k)
    base = args.r2g_url
    print(
        f"[config] r2g={base}  limit={args.limit}  "
        f"corpus={args.corpus or '<unscoped>'}  top_k={top_ks}  "
        f"concurrency={args.concurrency}",
        flush=True,
    )

    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        eval_set = await fetch_eval_set(conn, args.limit)

    if not eval_set:
        print(
            "[setup] no eval queries found (need >=4 gold docs each). "
            "Nothing to evaluate; exiting.",
            flush=True,
        )
        return

    print(f"[setup] {len(eval_set)} queries with >=4 gold docs each", flush=True)

    async with httpx.AsyncClient(timeout=DEFAULT_TIMEOUT_S) as client:
        baseline = await run_pass(
            client, base, eval_set, args.corpus, False, top_ks, args.concurrency
        )
        print_pass_summary("off", baseline, top_ks)
        treatment = await run_pass(
            client, base, eval_set, args.corpus, True, top_ks, args.concurrency
        )
        print_pass_summary("on", treatment, top_ks)
        print_delta(baseline, treatment, top_ks)


if __name__ == "__main__":
    asyncio.run(main())
