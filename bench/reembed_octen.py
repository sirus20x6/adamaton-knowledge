"""Batch re-embedder: deepresearch.chunks → platform.chunks_octen_embed.

Streams chunks server-side, batches them to the bench sidecar's
/v1/embeddings endpoint, and upserts results. Resumable: skips chunk_ids
already present in chunks_octen_embed.
"""

from __future__ import annotations

import asyncio
import os
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

SIDECAR = os.environ.get("SIDECAR", "http://sidecar-embed-bench:8089")
BATCH_SIZE = int(os.environ.get("BATCH_SIZE", "64"))
MAX_CHARS = int(os.environ.get("MAX_CHARS", "8000"))  # ~2K tokens, well under Octen's 32K
LOG_EVERY = int(os.environ.get("LOG_EVERY", "1000"))


async def embed_batch(client: httpx.AsyncClient, texts: list[str]) -> list[list[float]]:
    r = await client.post(
        f"{SIDECAR}/v1/embeddings",
        json={"input": texts},
        timeout=120.0,
    )
    r.raise_for_status()
    return [item["embedding"] for item in r.json()["data"]]


async def main() -> None:
    t_start = time.perf_counter()

    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=True) as conn:
        async with conn.cursor() as cur:
            await cur.execute("SELECT count(*) FROM platform.chunks_octen_embed")
            row = await cur.fetchone()
            done_before = row[0] if row else 0
            await cur.execute(
                "SELECT count(*) FROM deepresearch.chunks WHERE document_id IS NOT NULL"
            )
            total_row = await cur.fetchone()
            total = total_row[0] if total_row else 0

        remaining = total - done_before
        print(f"[setup] total={total} done={done_before} remaining={remaining}", flush=True)
        if remaining <= 0:
            print("nothing to do")
            return

        # Fetch all pending chunk_ids upfront (38K UUIDs ~= 1.5 MB).
        async with conn.cursor() as cur:
            await cur.execute(
                """
                SELECT c.id::text
                FROM deepresearch.chunks c
                LEFT JOIN platform.chunks_octen_embed o ON o.chunk_id = c.id
                WHERE c.document_id IS NOT NULL AND o.chunk_id IS NULL
                ORDER BY c.id
                """
            )
            pending_ids = [r[0] for r in await cur.fetchall()]
        print(f"[setup] fetched {len(pending_ids)} pending chunk ids", flush=True)

        async with httpx.AsyncClient() as client:
            processed = 0
            for i in range(0, len(pending_ids), BATCH_SIZE):
                id_slice = pending_ids[i : i + BATCH_SIZE]
                # Materialize chunk rows for this batch
                async with conn.cursor(row_factory=dict_row) as cur:
                    await cur.execute(
                        """
                        SELECT id::text AS chunk_id, document_id::text AS document_id, text
                        FROM deepresearch.chunks
                        WHERE id = ANY(%s::uuid[])
                        """,
                        (id_slice,),
                    )
                    rows = list(await cur.fetchall())
                processed += await _flush(client, conn, rows)
                if processed % LOG_EVERY < BATCH_SIZE:
                    rate = processed / (time.perf_counter() - t_start + 1e-6)
                    eta = (remaining - processed) / max(rate, 0.01)
                    print(
                        f"  [{processed}/{remaining}] {rate:.1f} chunks/s  ETA {eta/60:.1f} min",
                        flush=True,
                    )

        elapsed = time.perf_counter() - t_start
        async with conn.cursor() as cur:
            await cur.execute("SELECT count(*) FROM platform.chunks_octen_embed")
            row = await cur.fetchone()
            done_after = row[0] if row else 0

        print(
            f"\n[done] embedded {done_after - done_before} chunks in {elapsed:.1f}s "
            f"({(done_after - done_before) / max(elapsed, 1e-6):.1f}/s)  total now {done_after}",
            flush=True,
        )


async def _flush(
    client: httpx.AsyncClient,
    conn: psycopg.AsyncConnection,
    rows: list[dict[str, Any]],
) -> int:
    """Embed a batch and upsert into platform.chunks_octen_embed."""
    texts = [(r["text"] or "")[:MAX_CHARS] for r in rows]
    # Octen needs non-empty text; substitute a single space for empties.
    texts = [t if t.strip() else " " for t in texts]

    try:
        vectors = await embed_batch(client, texts)
    except Exception as exc:
        print(f"  [error] batch of {len(rows)} failed: {exc}", file=sys.stderr, flush=True)
        # Try halving the batch — usually OOM on a single long chunk.
        if len(rows) > 1:
            mid = len(rows) // 2
            return await _flush(client, conn, rows[:mid]) + await _flush(client, conn, rows[mid:])
        raise

    values = []
    for r, vec in zip(rows, vectors):
        vec_lit = "[" + ",".join(f"{x:.6f}" for x in vec) + "]"
        values.append((r["chunk_id"], r["document_id"], vec_lit))

    async with conn.cursor() as cur:
        await cur.executemany(
            """
            INSERT INTO platform.chunks_octen_embed (chunk_id, document_id, vec)
            VALUES (%s, %s, %s::vector)
            ON CONFLICT (chunk_id) DO UPDATE SET vec = EXCLUDED.vec, embed_ts = now()
            """,
            values,
        )
    return len(rows)


if __name__ == "__main__":
    asyncio.run(main())
