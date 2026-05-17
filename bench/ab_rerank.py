"""A/B benchmark: BGE-Reranker-v2-m3 vs Qwen3-Reranker-0.6B.

Shared retrieval pool (BGE-M3 top-50). For each query we have a gold set
of document_ids extracted from past research_jobs.paper_answers, and we
measure recall@5 and MRR@10 of each reranker's output.

Run inside platform-backend container so psycopg is on PYTHONPATH.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import re
import statistics
import time
from typing import Any

import httpx
import psycopg
from psycopg.rows import dict_row


DB_URL = os.environ.get(
    "PLATFORM_DATABASE_URL",
    "postgresql://postgres:postgres@postgres:5432/postgres",
).replace("postgresql+psycopg://", "postgresql://")

SIDECAR_A = os.environ.get("SIDECAR_A", "http://sidecar:8089")    # BGE
SIDECAR_B = os.environ.get("SIDECAR_B", "http://sidecar-bench:8089")  # Octen+Qwen3

TOP_K_RETRIEVAL = 50
TOP_K_EVAL = (1, 5, 10)


def parse_args() -> argparse.Namespace:
    """CLI surface for the A/B harness.

    `--graph-vote` is a *label-only* knob: this bench hits an already-running
    r2g (or the rerank sidecars), so we can't actually toggle
    R2G_GRAPH_VOTE_ENABLED from here. The operator restarts r2g with the
    desired env, then passes the matching label so the output is tagged
    correctly. Pass `both` to rerun the same query set twice (off → on) and
    emit a delta summary; the operator is responsible for bouncing r2g
    between the two passes when prompted.
    """
    p = argparse.ArgumentParser(
        description="BGE vs Qwen3 rerank A/B, optionally labeled by TGS-RAG graph-vote state."
    )
    p.add_argument(
        "--graph-vote",
        choices=("on", "off", "both"),
        default="off",
        help=(
            "Label the run by R2G_GRAPH_VOTE_ENABLED state of the r2g under "
            "test. 'both' reruns the query set with each label and prints a "
            "delta summary. Default: off."
        ),
    )
    return p.parse_args()


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


async def fetch_eval_set(conn) -> list[dict[str, Any]]:
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
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql)
        rows = await cur.fetchall()
    out = []
    for r in rows:
        gold = extract_gold_doc_ids(r["result"])
        if len(gold) >= 4:
            # cap excessive gold sets — 277 papers means the synth was sloppy
            gold = gold[:40]
            out.append({"id": str(r["id"]), "query": r["query"], "gold": gold})
    return out


async def embed_query(client: httpx.AsyncClient, base: str, text: str) -> list[float]:
    r = await client.post(
        f"{base}/v1/embeddings",
        json={"input": [text]},
        timeout=60.0,
    )
    r.raise_for_status()
    return r.json()["data"][0]["embedding"]


async def retrieve_top_k(
    conn, embedding: list[float], k: int
) -> list[dict[str, Any]]:
    """pgvector cosine-distance top-k over deepresearch.chunks."""
    vec_lit = "[" + ",".join(f"{x:.6f}" for x in embedding) + "]"
    sql = f"""
    SELECT id::text AS chunk_id, document_id::text AS document_id, text,
           1 - (vec <=> %s::vector) AS score
    FROM deepresearch.chunks
    WHERE document_id IS NOT NULL
    ORDER BY vec <=> %s::vector
    LIMIT {k}
    """
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql, (vec_lit, vec_lit))
        return list(await cur.fetchall())


async def rerank(
    client: httpx.AsyncClient,
    base: str,
    query: str,
    documents: list[str],
) -> list[dict[str, Any]]:
    r = await client.post(
        f"{base}/v1/rerank",
        json={"query": query, "documents": documents},
        timeout=180.0,
    )
    r.raise_for_status()
    return r.json()["results"]


def reranked_doc_order(
    reranker_results: list[dict[str, Any]],
    chunks: list[dict[str, Any]],
) -> list[str]:
    """Convert chunk-level reranker output → ordered list of distinct
    document_ids, keeping the highest-scoring chunk per doc."""
    seen_doc: set[str] = set()
    ordered: list[str] = []
    for r in reranker_results:  # already sorted by score desc
        chunk = chunks[r["index"]]
        doc = chunk["document_id"]
        if doc in seen_doc:
            continue
        seen_doc.add(doc)
        ordered.append(doc)
    return ordered


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


async def _run_pass(
    conn,
    client: httpx.AsyncClient,
    eval_set: list[dict[str, Any]],
    label: str,
) -> dict[str, Any] | None:
    """Run the full eval loop once under a given graph-vote label.

    Returns a dict of aggregate metrics keyed as
    ``f"{metric}_{stack}"`` (e.g. ``"r@10_bge"``, ``"mrr@10_qwen3"``) plus
    ``"n"`` (queries evaluated) and ``"latency_ms"`` per stack. Returns
    ``None`` if no queries were evaluable. The label is *only* a tag —
    this loop doesn't itself flip R2G_GRAPH_VOTE_ENABLED; the operator is
    responsible for restarting r2g with the matching env value.
    """
    print()
    print("#" * 100)
    print(f"# PASS  graph_vote={label}  "
          f"(expects R2G_GRAPH_VOTE_ENABLED={'true' if label == 'on' else 'false'} on the live r2g)")
    print("#" * 100, flush=True)

    per_query_rows: list[dict[str, Any]] = []
    rerank_latency_ms: dict[str, list[float]] = {"bge": [], "qwen3": []}

    for ex in eval_set:
        gold = set(ex["gold"])
        # Use BGE-M3 to embed (shared retrieval pool)
        q_vec = await embed_query(client, SIDECAR_A, ex["query"])
        chunks = await retrieve_top_k(conn, q_vec, TOP_K_RETRIEVAL)
        if not chunks:
            continue
        texts = [c["text"][:2000] for c in chunks]  # rerankers don't need full chunk

        # Baseline order = pre-rerank pgvector
        baseline_order = []
        seen = set()
        for c in chunks:
            if c["document_id"] not in seen:
                baseline_order.append(c["document_id"])
                seen.add(c["document_id"])

        # BGE reranker
        t0 = time.perf_counter()
        bge_res = await rerank(client, SIDECAR_A, ex["query"], texts)
        rerank_latency_ms["bge"].append(
            (time.perf_counter() - t0) * 1000 / len(texts)
        )
        bge_order = reranked_doc_order(bge_res, chunks)

        # Qwen3 reranker
        t0 = time.perf_counter()
        qwen3_res = await rerank(client, SIDECAR_B, ex["query"], texts)
        rerank_latency_ms["qwen3"].append(
            (time.perf_counter() - t0) * 1000 / len(texts)
        )
        qwen3_order = reranked_doc_order(qwen3_res, chunks)

        row = {
            "query": ex["query"][:60],
            "gold_n": len(gold),
            "candidates_n": len(chunks),
            "unique_docs": len(baseline_order),
        }
        for stack, order in (
            ("baseline", baseline_order),
            ("bge", bge_order),
            ("qwen3", qwen3_order),
        ):
            for k in TOP_K_EVAL:
                row[f"r@{k}_{stack}"] = recall_at_k(order, gold, k)
            row[f"mrr@10_{stack}"] = mrr_at_k(order, gold, 10)
        per_query_rows.append(row)
        print(
            f"[q gv={label}] gold={len(gold):>2}  "
            f"r@5 base={row['r@5_baseline']:.2f} bge={row['r@5_bge']:.2f} qwen3={row['r@5_qwen3']:.2f}  "
            f"|  {ex['query'][:60]!r}",
            flush=True,
        )

    if not per_query_rows:
        print(f"[graph_vote={label}] no queries evaluated", flush=True)
        return None

    print()
    print("=" * 100)
    print(f"AGGREGATE [graph_vote={label}] — mean across {len(per_query_rows)} queries")
    print("=" * 100)
    print(f"{'metric':<14}{'baseline':>14}{'bge-rerank':>14}{'qwen3-rerank':>14}")
    agg: dict[str, Any] = {"n": len(per_query_rows), "label": label}
    for k in TOP_K_EVAL:
        base = statistics.mean(r[f"r@{k}_baseline"] for r in per_query_rows)
        bge = statistics.mean(r[f"r@{k}_bge"] for r in per_query_rows)
        qwen3 = statistics.mean(r[f"r@{k}_qwen3"] for r in per_query_rows)
        agg[f"r@{k}_baseline"] = base
        agg[f"r@{k}_bge"] = bge
        agg[f"r@{k}_qwen3"] = qwen3
        print(f"{'recall@' + str(k):<14}{base:>14.4f}{bge:>14.4f}{qwen3:>14.4f}")
    base_mrr = statistics.mean(r["mrr@10_baseline"] for r in per_query_rows)
    bge_mrr = statistics.mean(r["mrr@10_bge"] for r in per_query_rows)
    qwen3_mrr = statistics.mean(r["mrr@10_qwen3"] for r in per_query_rows)
    agg["mrr@10_baseline"] = base_mrr
    agg["mrr@10_bge"] = bge_mrr
    agg["mrr@10_qwen3"] = qwen3_mrr
    print(f"{'mrr@10':<14}{base_mrr:>14.4f}{bge_mrr:>14.4f}{qwen3_mrr:>14.4f}")
    print()
    bge_lat = statistics.mean(rerank_latency_ms["bge"])
    qwen3_lat = statistics.mean(rerank_latency_ms["qwen3"])
    agg["latency_ms_bge"] = bge_lat
    agg["latency_ms_qwen3"] = qwen3_lat
    print(f"latency / pair (ms):  bge={bge_lat:.2f}  qwen3={qwen3_lat:.2f}")

    # dump full table for the user
    print()
    print(f"PER-QUERY BREAKDOWN [graph_vote={label}]")
    print("-" * 100)
    for row in per_query_rows:
        print(
            f"  gold={row['gold_n']:>2} cands={row['candidates_n']:>2}/{row['unique_docs']:>2}u  "
            f"r@5 base={row['r@5_baseline']:.2f} bge={row['r@5_bge']:.2f} qwen3={row['r@5_qwen3']:.2f}  "
            f"mrr base={row['mrr@10_baseline']:.2f} bge={row['mrr@10_bge']:.2f} qwen3={row['mrr@10_qwen3']:.2f}  "
            f"| {row['query']!r}"
        )

    return agg


def _print_delta(off: dict[str, Any], on: dict[str, Any]) -> None:
    """Print a graph_vote=off → graph_vote=on side-by-side summary."""
    print()
    print("=" * 100)
    print("DELTA  graph_vote=off  →  graph_vote=on")
    print("=" * 100)
    header = f"{'metric':<22}{'off':>12}{'on':>12}{'Δ':>12}"
    print(header)
    metric_keys: list[str] = []
    for k in TOP_K_EVAL:
        for stack in ("baseline", "bge", "qwen3"):
            metric_keys.append(f"r@{k}_{stack}")
    for stack in ("baseline", "bge", "qwen3"):
        metric_keys.append(f"mrr@10_{stack}")
    for key in metric_keys:
        v_off = off.get(key)
        v_on = on.get(key)
        if v_off is None or v_on is None:
            continue
        delta = v_on - v_off
        sign = "+" if delta >= 0 else ""
        print(f"{key:<22}{v_off:>12.4f}{v_on:>12.4f}{sign + f'{delta:.4f}':>12}")
    print()
    print(f"queries (off={off.get('n')}, on={on.get('n')})")


async def main() -> None:
    args = parse_args()
    env_val = os.environ.get("R2G_GRAPH_VOTE_ENABLED", "<unset>")
    print(
        f"[config] --graph-vote={args.graph_vote}  "
        f"(R2G_GRAPH_VOTE_ENABLED in this process env = {env_val!r}; the "
        f"label tags output for the *live r2g*, which the operator must "
        f"restart with the matching env)",
        flush=True,
    )

    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        eval_set = await fetch_eval_set(conn)
        print(f"[setup] {len(eval_set)} queries with ≥4 gold docs each", flush=True)
        for ex in eval_set:
            print(f"  - {ex['query'][:80]!r} (gold={len(ex['gold'])})", flush=True)

        async with httpx.AsyncClient() as client:
            if args.graph_vote == "both":
                print(
                    "\n[both] starting baseline pass — ensure r2g is running "
                    "with R2G_GRAPH_VOTE_ENABLED=false NOW.",
                    flush=True,
                )
                off_agg = await _run_pass(conn, client, eval_set, "off")
                print(
                    "\n[both] baseline pass complete. RESTART r2g with "
                    "R2G_GRAPH_VOTE_ENABLED=true before the treatment pass "
                    "starts in 10s...",
                    flush=True,
                )
                await asyncio.sleep(10)
                on_agg = await _run_pass(conn, client, eval_set, "on")
                if off_agg and on_agg:
                    _print_delta(off_agg, on_agg)
            else:
                await _run_pass(conn, client, eval_set, args.graph_vote)


if __name__ == "__main__":
    asyncio.run(main())
