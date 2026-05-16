"""A/B benchmark: BGE-Reranker-v2-m3 vs Qwen3-Reranker-0.6B.

Shared retrieval pool (BGE-M3 top-50). For each query we have a gold set
of document_ids extracted from past research_jobs.paper_answers, and we
measure recall@5 and MRR@10 of each reranker's output.

Run inside platform-backend container so psycopg is on PYTHONPATH.
"""

from __future__ import annotations

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


async def main() -> None:
    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        eval_set = await fetch_eval_set(conn)
        print(f"[setup] {len(eval_set)} queries with ≥4 gold docs each", flush=True)
        for ex in eval_set:
            print(f"  - {ex['query'][:80]!r} (gold={len(ex['gold'])})", flush=True)

        per_query_rows = []
        rerank_latency_ms = {"bge": [], "qwen3": []}

        async with httpx.AsyncClient() as client:
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
                    f"[q] gold={len(gold):>2}  "
                    f"r@5 base={row['r@5_baseline']:.2f} bge={row['r@5_bge']:.2f} qwen3={row['r@5_qwen3']:.2f}  "
                    f"|  {ex['query'][:60]!r}",
                    flush=True,
                )

        if not per_query_rows:
            print("no queries evaluated")
            return

        print()
        print("=" * 100)
        print("AGGREGATE — mean across", len(per_query_rows), "queries")
        print("=" * 100)
        print(f"{'metric':<14}{'baseline':>14}{'bge-rerank':>14}{'qwen3-rerank':>14}")
        for k in TOP_K_EVAL:
            base = statistics.mean(r[f"r@{k}_baseline"] for r in per_query_rows)
            bge = statistics.mean(r[f"r@{k}_bge"] for r in per_query_rows)
            qwen3 = statistics.mean(r[f"r@{k}_qwen3"] for r in per_query_rows)
            print(f"{'recall@' + str(k):<14}{base:>14.4f}{bge:>14.4f}{qwen3:>14.4f}")
        base_mrr = statistics.mean(r["mrr@10_baseline"] for r in per_query_rows)
        bge_mrr = statistics.mean(r["mrr@10_bge"] for r in per_query_rows)
        qwen3_mrr = statistics.mean(r["mrr@10_qwen3"] for r in per_query_rows)
        print(f"{'mrr@10':<14}{base_mrr:>14.4f}{bge_mrr:>14.4f}{qwen3_mrr:>14.4f}")
        print()
        print(f"latency / pair (ms):  bge={statistics.mean(rerank_latency_ms['bge']):.2f}  qwen3={statistics.mean(rerank_latency_ms['qwen3']):.2f}")

        # dump full table for the user
        print()
        print("PER-QUERY BREAKDOWN")
        print("-" * 100)
        for row in per_query_rows:
            print(
                f"  gold={row['gold_n']:>2} cands={row['candidates_n']:>2}/{row['unique_docs']:>2}u  "
                f"r@5 base={row['r@5_baseline']:.2f} bge={row['r@5_bge']:.2f} qwen3={row['r@5_qwen3']:.2f}  "
                f"mrr base={row['mrr@10_baseline']:.2f} bge={row['mrr@10_bge']:.2f} qwen3={row['mrr@10_qwen3']:.2f}  "
                f"| {row['query']!r}"
            )


if __name__ == "__main__":
    asyncio.run(main())
