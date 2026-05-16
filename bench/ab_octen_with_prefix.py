"""4-way A/B (variant): Octen queries get the Qwen3 instruction prefix.

Same harness as ab_full_matrix.py, but queries embedded against the Octen
sidecar are wrapped with the canonical Qwen3-Embedding query-side prompt:

    Instruct: Given a web search query, retrieve relevant passages that
    answer the query
    Query: <q>

Documents in platform.chunks_octen_embed remain prefix-free (per spec —
the Qwen3 family uses an asymmetric encoding where only queries get the
instruction).

This is a fair-shot test for Octen against BGE-M3.
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

SIDECAR_BGE = os.environ.get("SIDECAR_BGE", "http://sidecar:8089")
SIDECAR_OCTEN = os.environ.get("SIDECAR_OCTEN", "http://sidecar-embed-bench:8089")

TOP_K_RETRIEVAL = 50
N_QUERIES = 100
SEED = 17  # same as ab_full_matrix.py — direct comparison

QWEN3_QUERY_PREFIX = (
    "Instruct: Given a web search query, retrieve relevant passages that "
    "answer the query\nQuery: "
)


def clean_title(title: str) -> str:
    t = title.removesuffix(".md").removesuffix(".pdf").strip()
    t = re.sub(r"^\d{4}\.\d{4,5}(v\d+)?\s*[-:]\s*", "", t)
    return t


async def fetch_papers(conn, n: int, seed: int) -> list[dict[str, Any]]:
    sql = f"""
    SELECT d.id::text AS document_id, d.title
    FROM deepresearch.documents d
    WHERE d.title IS NOT NULL AND length(d.title) > 20
      AND EXISTS (SELECT 1 FROM deepresearch.chunks c WHERE c.document_id = d.id LIMIT 1)
      AND EXISTS (SELECT 1 FROM platform.chunks_octen_embed o WHERE o.document_id = d.id LIMIT 1)
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
    SELECT id::text AS chunk_id, document_id::text AS document_id, text
    FROM deepresearch.chunks
    WHERE document_id IS NOT NULL
    ORDER BY vec <=> %s::vector LIMIT {k}
    """
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql, (vec_lit,))
        return list(await cur.fetchall())


async def retrieve_octen(conn, embedding: list[float], k: int) -> list[dict[str, Any]]:
    vec_lit = "[" + ",".join(f"{x:.6f}" for x in embedding) + "]"
    sql = f"""
    SELECT o.chunk_id::text AS chunk_id, o.document_id::text AS document_id, c.text
    FROM platform.chunks_octen_embed o
    JOIN deepresearch.chunks c ON c.id = o.chunk_id
    ORDER BY o.vec <=> %s::vector LIMIT {k}
    """
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(sql, (vec_lit,))
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
    seen, out = set(), []
    for c in chunks:
        if c["document_id"] in seen: continue
        seen.add(c["document_id"]); out.append(c["document_id"])
    return out


def doc_order_from_rerank(
    rerank_results: list[dict[str, Any]], chunks: list[dict[str, Any]]
) -> list[str]:
    seen, out = set(), []
    for r in rerank_results:
        doc = chunks[r["index"]]["document_id"]
        if doc in seen: continue
        seen.add(doc); out.append(doc)
    return out


def hit_at_k(predicted: list[str], gold: str, k: int) -> float:
    return 1.0 if gold in predicted[:k] else 0.0


def rr(predicted: list[str], gold: str) -> float:
    for i, d in enumerate(predicted, start=1):
        if d == gold: return 1.0 / i
    return 0.0


async def main() -> None:
    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        papers = await fetch_papers(conn, N_QUERIES, SEED)
        print(f"[setup] sampled {len(papers)} papers; Octen queries use Qwen3 instruction prefix", flush=True)

        rows = []
        skipped_oop = {"bge": 0, "octen_raw": 0, "octen_prefix": 0}

        async with httpx.AsyncClient() as client:
            for i, p in enumerate(papers):
                q = p["title"]
                gold = p["document_id"]

                # BGE: no prefix
                bge_vec = await embed_query(client, SIDECAR_BGE, q)
                # Octen RAW (for back-compat with the previous bench)
                octen_raw_vec = await embed_query(client, SIDECAR_OCTEN, q)
                # Octen PREFIX (the right way)
                octen_pre_vec = await embed_query(client, SIDECAR_OCTEN, QWEN3_QUERY_PREFIX + q)

                bge_chunks = await retrieve_bge(conn, bge_vec, TOP_K_RETRIEVAL)
                octen_raw_chunks = await retrieve_octen(conn, octen_raw_vec, TOP_K_RETRIEVAL)
                octen_pre_chunks = await retrieve_octen(conn, octen_pre_vec, TOP_K_RETRIEVAL)

                bge_base = doc_order_from_chunks(bge_chunks)
                octen_raw_base = doc_order_from_chunks(octen_raw_chunks)
                octen_pre_base = doc_order_from_chunks(octen_pre_chunks)

                if gold not in bge_base: skipped_oop["bge"] += 1
                if gold not in octen_raw_base: skipped_oop["octen_raw"] += 1
                if gold not in octen_pre_base: skipped_oop["octen_prefix"] += 1

                # Rerank all three pools using Qwen3-Reranker on the main sidecar
                bge_texts = [c["text"][:2000] for c in bge_chunks]
                octen_raw_texts = [c["text"][:2000] for c in octen_raw_chunks]
                octen_pre_texts = [c["text"][:2000] for c in octen_pre_chunks]

                bge_rr = await rerank(client, SIDECAR_BGE, q, bge_texts)
                octen_raw_rr = await rerank(client, SIDECAR_BGE, q, octen_raw_texts)
                octen_pre_rr = await rerank(client, SIDECAR_BGE, q, octen_pre_texts)

                bge_rr_order = doc_order_from_rerank(bge_rr, bge_chunks)
                octen_raw_rr_order = doc_order_from_rerank(octen_raw_rr, octen_raw_chunks)
                octen_pre_rr_order = doc_order_from_rerank(octen_pre_rr, octen_pre_chunks)

                row = {}
                for stack, order in (
                    ("bge_base", bge_base), ("bge_rr", bge_rr_order),
                    ("octen_raw_base", octen_raw_base), ("octen_raw_rr", octen_raw_rr_order),
                    ("octen_pre_base", octen_pre_base), ("octen_pre_rr", octen_pre_rr_order),
                ):
                    row[f"h1_{stack}"] = hit_at_k(order, gold, 1)
                    row[f"h5_{stack}"] = hit_at_k(order, gold, 5)
                    row[f"rr_{stack}"] = rr(order, gold)
                rows.append(row)

                if i < 10 or i % 10 == 0:
                    print(
                        f"  [{i+1:>3}] bge={row['h1_bge_base']:.0f}/{row['h1_bge_rr']:.0f} "
                        f"octen-raw={row['h1_octen_raw_base']:.0f}/{row['h1_octen_raw_rr']:.0f} "
                        f"octen-pre={row['h1_octen_pre_base']:.0f}/{row['h1_octen_pre_rr']:.0f}  | {q[:60]!r}",
                        flush=True,
                    )

        n = len(rows)
        print()
        print(f"in-pool: {n} / {len(papers)}   gold-OOP@50: bge={skipped_oop['bge']}  octen_raw={skipped_oop['octen_raw']}  octen_prefix={skipped_oop['octen_prefix']}")
        print()
        cols = [
            ("bge_base", "BGE base"),
            ("bge_rr", "BGE+rerank"),
            ("octen_raw_base", "Octen-raw"),
            ("octen_raw_rr", "Octen-raw+rr"),
            ("octen_pre_base", "Octen+prefix"),
            ("octen_pre_rr", "Octen+prefix+rr"),
        ]
        hdr = "metric".ljust(10) + "".join(f"{lbl:>16}" for _, lbl in cols)
        print(hdr)
        for label, key in (("hit@1", "h1"), ("hit@5", "h5"), ("MRR", "rr")):
            line = label.ljust(10)
            for stack, _ in cols:
                v = statistics.mean(r[f"{key}_{stack}"] for r in rows)
                line += f"{v:>16.4f}"
            print(line)


if __name__ == "__main__":
    asyncio.run(main())
