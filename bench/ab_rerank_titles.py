"""Harder A/B: 100 papers, title-as-query, exactly 1 gold doc each.

Tests whether each reranker can promote the actual paper to the top
of the candidate list given its title — a clean precision signal that
the paper_answers-derived test couldn't give us.
"""

from __future__ import annotations

import asyncio
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

SIDECAR_A = os.environ.get("SIDECAR_A", "http://sidecar:8089")
SIDECAR_B = os.environ.get("SIDECAR_B", "http://sidecar-bench:8089")

TOP_K_RETRIEVAL = 50
N_QUERIES = 100
SEED = 17  # reproducible random sample


def clean_title(title: str) -> str:
    t = title.removesuffix(".md").removesuffix(".pdf").strip()
    # strip leading arxiv id like "2310.06825 - "
    t = re.sub(r"^\d{4}\.\d{4,5}(v\d+)?\s*[-:]\s*", "", t)
    return t


async def fetch_papers(conn, n: int, seed: int) -> list[dict[str, Any]]:
    sql = f"""
    SELECT d.id::text AS document_id, d.title
    FROM deepresearch.documents d
    WHERE d.title IS NOT NULL
      AND length(d.title) > 20
      AND EXISTS (
        SELECT 1 FROM deepresearch.chunks c
        WHERE c.document_id = d.id
        LIMIT 1
      )
    ORDER BY md5(d.id::text || '{seed}')
    LIMIT {n}
    """
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql)
        rows = await cur.fetchall()
    return [{"document_id": r["document_id"], "title": clean_title(r["title"])} for r in rows]


async def embed_query(client: httpx.AsyncClient, base: str, text: str) -> list[float]:
    r = await client.post(
        f"{base}/v1/embeddings", json={"input": [text]}, timeout=60.0
    )
    r.raise_for_status()
    return r.json()["data"][0]["embedding"]


async def retrieve_top_k(conn, embedding: list[float], k: int) -> list[dict[str, Any]]:
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
    client: httpx.AsyncClient, base: str, query: str, documents: list[str]
) -> list[dict[str, Any]]:
    r = await client.post(
        f"{base}/v1/rerank",
        json={"query": query, "documents": documents},
        timeout=180.0,
    )
    r.raise_for_status()
    return r.json()["results"]


def reranked_doc_order(
    reranker_results: list[dict[str, Any]], chunks: list[dict[str, Any]]
) -> list[str]:
    seen = set()
    out = []
    for r in reranker_results:
        doc = chunks[r["index"]]["document_id"]
        if doc in seen:
            continue
        seen.add(doc)
        out.append(doc)
    return out


def hit_at_k(predicted: list[str], gold_doc: str, k: int) -> float:
    return 1.0 if gold_doc in predicted[:k] else 0.0


def rr(predicted: list[str], gold_doc: str) -> float:
    for i, d in enumerate(predicted, start=1):
        if d == gold_doc:
            return 1.0 / i
    return 0.0


async def main() -> None:
    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        papers = await fetch_papers(conn, N_QUERIES, SEED)
        print(f"[setup] sampled {len(papers)} papers", flush=True)

        rows = []
        lat = {"bge": [], "qwen3": []}
        skipped_no_recall = 0

        async with httpx.AsyncClient() as client:
            for i, p in enumerate(papers):
                q = p["title"]
                gold = p["document_id"]
                q_vec = await embed_query(client, SIDECAR_A, q)
                chunks = await retrieve_top_k(conn, q_vec, TOP_K_RETRIEVAL)
                if not chunks:
                    continue

                baseline = []
                seen = set()
                for c in chunks:
                    if c["document_id"] not in seen:
                        baseline.append(c["document_id"])
                        seen.add(c["document_id"])

                if gold not in baseline:
                    # Gold not retrievable — neither reranker can save it.
                    skipped_no_recall += 1
                    continue

                texts = [c["text"][:2000] for c in chunks]

                t0 = time.perf_counter()
                bge_res = await rerank(client, SIDECAR_A, q, texts)
                lat["bge"].append((time.perf_counter() - t0) * 1000 / len(texts))
                bge_order = reranked_doc_order(bge_res, chunks)

                t0 = time.perf_counter()
                qwen3_res = await rerank(client, SIDECAR_B, q, texts)
                lat["qwen3"].append((time.perf_counter() - t0) * 1000 / len(texts))
                qwen3_order = reranked_doc_order(qwen3_res, chunks)

                row = {
                    "title": q[:60],
                    "base_pos": baseline.index(gold) + 1,
                    "bge_pos": (bge_order.index(gold) + 1) if gold in bge_order else 999,
                    "qwen3_pos": (qwen3_order.index(gold) + 1) if gold in qwen3_order else 999,
                }
                for stack, order in (("base", baseline), ("bge", bge_order), ("qwen3", qwen3_order)):
                    row[f"h1_{stack}"] = hit_at_k(order, gold, 1)
                    row[f"h5_{stack}"] = hit_at_k(order, gold, 5)
                    row[f"rr_{stack}"] = rr(order, gold)
                rows.append(row)
                if i < 20 or i % 10 == 0:
                    print(
                        f"  [{i+1:>3}] base={row['base_pos']:>2} bge={row['bge_pos']:>2} qwen3={row['qwen3_pos']:>2}  | {q[:70]!r}",
                        flush=True,
                    )

        if not rows:
            print("no usable queries")
            return

        n = len(rows)
        print()
        print(f"in-pool: {n} / {len(papers)} queries ({skipped_no_recall} gold not in top-{TOP_K_RETRIEVAL})")
        print()
        print(f"{'metric':<14}{'baseline':>14}{'bge-rerank':>14}{'qwen3-rerank':>14}")
        for label, key in (("hit@1", "h1"), ("hit@5", "h5"), ("MRR", "rr")):
            b = statistics.mean(r[f"{key}_base"] for r in rows)
            bg = statistics.mean(r[f"{key}_bge"] for r in rows)
            qw = statistics.mean(r[f"{key}_qwen3"] for r in rows)
            print(f"{label:<14}{b:>14.4f}{bg:>14.4f}{qw:>14.4f}")

        print()
        print(f"latency / pair (ms):  bge={statistics.mean(lat['bge']):.2f}  qwen3={statistics.mean(lat['qwen3']):.2f}")

        # head-to-head wins
        bge_better = sum(1 for r in rows if r["bge_pos"] < r["qwen3_pos"])
        tie = sum(1 for r in rows if r["bge_pos"] == r["qwen3_pos"])
        qwen3_better = sum(1 for r in rows if r["qwen3_pos"] < r["bge_pos"])
        print(f"\nhead-to-head positions:  bge<qwen3: {bge_better}   tie: {tie}   qwen3<bge: {qwen3_better}")

        # base→reranker movement
        moved_up_bge = sum(1 for r in rows if r["bge_pos"] < r["base_pos"])
        moved_up_qwen3 = sum(1 for r in rows if r["qwen3_pos"] < r["base_pos"])
        moved_down_bge = sum(1 for r in rows if r["bge_pos"] > r["base_pos"])
        moved_down_qwen3 = sum(1 for r in rows if r["qwen3_pos"] > r["base_pos"])
        print(f"vs baseline       :  bge up:{moved_up_bge} down:{moved_down_bge}   qwen3 up:{moved_up_qwen3} down:{moved_down_qwen3}")


if __name__ == "__main__":
    asyncio.run(main())
