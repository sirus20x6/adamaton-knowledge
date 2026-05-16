"""4-way A/B: {BGE-M3, Octen} × {none, Qwen3-Rerank}.

Same title-as-query benchmark as ab_rerank_titles.py, but now retrieval
candidates come from two independent pgvector pools — deepresearch.chunks
(BGE-M3) and platform.chunks_octen_embed (Octen) — and the reranker is
the new Qwen3-Reranker on both. Reports hit@1, hit@5, MRR for the four
combinations.

The reranker runs against the chunk *text* — same text in both pools —
so a query that retrieves the right chunk in one pool but not the other
is the embedder difference. A query where rerank promotes the gold doc
in pool A but not pool B is the *interaction* between embedder and rerank
candidate quality.
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

SIDECAR_BGE = os.environ.get("SIDECAR_BGE", "http://sidecar:8089")  # main, BGE-M3 + Qwen3-Rerank
SIDECAR_OCTEN = os.environ.get("SIDECAR_OCTEN", "http://sidecar-embed-bench:8089")  # Octen embed

TOP_K_RETRIEVAL = 50
N_QUERIES = 100
SEED = 17  # same as ab_rerank_titles for direct comparison


def clean_title(title: str) -> str:
    t = title.removesuffix(".md").removesuffix(".pdf").strip()
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
      AND EXISTS (
        SELECT 1 FROM platform.chunks_octen_embed o
        WHERE o.document_id = d.id
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
    r = await client.post(f"{base}/v1/embeddings", json={"input": [text]}, timeout=60.0)
    r.raise_for_status()
    return r.json()["data"][0]["embedding"]


async def retrieve_bge(conn, embedding: list[float], k: int) -> list[dict[str, Any]]:
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


async def retrieve_octen(conn, embedding: list[float], k: int) -> list[dict[str, Any]]:
    vec_lit = "[" + ",".join(f"{x:.6f}" for x in embedding) + "]"
    sql = f"""
    SELECT o.chunk_id::text AS chunk_id,
           o.document_id::text AS document_id,
           c.text,
           1 - (o.vec <=> %s::vector) AS score
    FROM platform.chunks_octen_embed o
    JOIN deepresearch.chunks c ON c.id = o.chunk_id
    ORDER BY o.vec <=> %s::vector
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


def doc_order_from_chunks(chunks: list[dict[str, Any]]) -> list[str]:
    seen = set()
    out = []
    for c in chunks:
        if c["document_id"] in seen:
            continue
        seen.add(c["document_id"])
        out.append(c["document_id"])
    return out


def doc_order_from_rerank(
    rerank_results: list[dict[str, Any]], chunks: list[dict[str, Any]]
) -> list[str]:
    seen = set()
    out = []
    for r in rerank_results:
        doc = chunks[r["index"]]["document_id"]
        if doc in seen:
            continue
        seen.add(doc)
        out.append(doc)
    return out


def hit_at_k(predicted: list[str], gold: str, k: int) -> float:
    return 1.0 if gold in predicted[:k] else 0.0


def rr(predicted: list[str], gold: str) -> float:
    for i, d in enumerate(predicted, start=1):
        if d == gold:
            return 1.0 / i
    return 0.0


async def main() -> None:
    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        papers = await fetch_papers(conn, N_QUERIES, SEED)
        print(f"[setup] sampled {len(papers)} papers (both pools)", flush=True)

        rows = []
        skipped_bge_oop = 0
        skipped_octen_oop = 0
        skipped_both_oop = 0
        lat_embed = {"bge": [], "octen": []}
        lat_rerank = []

        async with httpx.AsyncClient() as client:
            for i, p in enumerate(papers):
                q = p["title"]
                gold = p["document_id"]

                # Embed with both
                t0 = time.perf_counter()
                bge_vec = await embed_query(client, SIDECAR_BGE, q)
                lat_embed["bge"].append((time.perf_counter() - t0) * 1000)

                t0 = time.perf_counter()
                octen_vec = await embed_query(client, SIDECAR_OCTEN, q)
                lat_embed["octen"].append((time.perf_counter() - t0) * 1000)

                # Retrieve from each pool
                bge_chunks = await retrieve_bge(conn, bge_vec, TOP_K_RETRIEVAL)
                octen_chunks = await retrieve_octen(conn, octen_vec, TOP_K_RETRIEVAL)

                bge_base_order = doc_order_from_chunks(bge_chunks)
                octen_base_order = doc_order_from_chunks(octen_chunks)

                gold_in_bge = gold in bge_base_order
                gold_in_octen = gold in octen_base_order
                if not gold_in_bge and not gold_in_octen:
                    skipped_both_oop += 1
                    continue
                if not gold_in_bge:
                    skipped_bge_oop += 1
                if not gold_in_octen:
                    skipped_octen_oop += 1

                # Rerank both pools
                bge_texts = [c["text"][:2000] for c in bge_chunks]
                octen_texts = [c["text"][:2000] for c in octen_chunks]

                t0 = time.perf_counter()
                bge_rr_results = await rerank(client, SIDECAR_BGE, q, bge_texts)
                octen_rr_results = await rerank(client, SIDECAR_BGE, q, octen_texts)
                lat_rerank.append((time.perf_counter() - t0) * 1000)

                bge_rr_order = doc_order_from_rerank(bge_rr_results, bge_chunks)
                octen_rr_order = doc_order_from_rerank(octen_rr_results, octen_chunks)

                row = {
                    "title": q[:60],
                    "h1_bge_base": hit_at_k(bge_base_order, gold, 1),
                    "h5_bge_base": hit_at_k(bge_base_order, gold, 5),
                    "rr_bge_base": rr(bge_base_order, gold),
                    "h1_bge_rr": hit_at_k(bge_rr_order, gold, 1),
                    "h5_bge_rr": hit_at_k(bge_rr_order, gold, 5),
                    "rr_bge_rr": rr(bge_rr_order, gold),
                    "h1_octen_base": hit_at_k(octen_base_order, gold, 1),
                    "h5_octen_base": hit_at_k(octen_base_order, gold, 5),
                    "rr_octen_base": rr(octen_base_order, gold),
                    "h1_octen_rr": hit_at_k(octen_rr_order, gold, 1),
                    "h5_octen_rr": hit_at_k(octen_rr_order, gold, 5),
                    "rr_octen_rr": rr(octen_rr_order, gold),
                }
                rows.append(row)
                if i < 20 or i % 10 == 0:
                    print(
                        f"  [{i+1:>3}] "
                        f"bge(b/r)={row['h1_bge_base']:.0f}/{row['h1_bge_rr']:.0f}  "
                        f"octen(b/r)={row['h1_octen_base']:.0f}/{row['h1_octen_rr']:.0f}  "
                        f"| {q[:70]!r}",
                        flush=True,
                    )

        n = len(rows)
        if not n:
            print("no rows")
            return

        print()
        print(f"in-pool: {n} / {len(papers)}   gold-OOP: bge_only_oop={skipped_octen_oop}  octen_only_oop={skipped_bge_oop}  both_oop={skipped_both_oop}")
        print()
        print(f"{'metric':<10}{'bge-base':>12}{'bge+rerank':>14}{'octen-base':>14}{'octen+rerank':>16}")
        for label, key in (("hit@1", "h1"), ("hit@5", "h5"), ("MRR", "rr")):
            bb = statistics.mean(r[f"{key}_bge_base"] for r in rows)
            br = statistics.mean(r[f"{key}_bge_rr"] for r in rows)
            ob = statistics.mean(r[f"{key}_octen_base"] for r in rows)
            orr = statistics.mean(r[f"{key}_octen_rr"] for r in rows)
            print(f"{label:<10}{bb:>12.4f}{br:>14.4f}{ob:>14.4f}{orr:>16.4f}")

        print()
        print(
            f"latency (ms):  embed bge={statistics.mean(lat_embed['bge']):.1f}  octen={statistics.mean(lat_embed['octen']):.1f}  "
            f"rerank-pair={statistics.mean(lat_rerank) / (2 * TOP_K_RETRIEVAL):.2f}"
        )


if __name__ == "__main__":
    asyncio.run(main())
