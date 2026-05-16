"""Resumable re-ingestion: re-chunk existing docs with the new 3072-char
chunk_size, then re-embed in both pools.

Resumability model
------------------
``platform.reingest_progress`` tracks per-doc state (pending → in_progress
→ complete/failed). The worker:

* Claims one pending doc at a time via SELECT ... FOR UPDATE SKIP LOCKED,
  so multiple instances can run in parallel without stepping on each
  other.
* Treats each doc as a single transaction: re-assemble text →
  re-chunk → embed → BEGIN; DELETE old + INSERT new + INSERT octen;
  COMMIT. Killed mid-doc → row stays pending or in_progress and gets
  picked up next time (idempotent because DELETE-then-INSERT).
* On every doc commit, ALSO enqueues a kg_task so the KG entities
  (which referenced the old chunk IDs) get re-extracted. The hi_worker
  picks those up at its own pace.

To run:
    docker exec -w /app -e SIDECAR_BGE=http://sidecar:8089 \\
        -e SIDECAR_OCTEN=http://sidecar-octen:8089 \\
        platform-backend-1 python /tmp/reingest.py [--limit N] [--dry-run]

Ctrl-C or SIGTERM at any time is safe. Re-run to resume.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import logging
import os
import signal
import sys
import time
import uuid
from typing import Any

import httpx
import psycopg
from psycopg.rows import dict_row

# R2R's recursive char splitter — same one its IngestionService uses, so
# new chunks land where R2R would have placed them on fresh ingestion.
from core.base.utils import RecursiveCharacterTextSplitter

logging.basicConfig(
    level=os.environ.get("LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
log = logging.getLogger("reingest")


DB_URL = os.environ.get(
    "PLATFORM_DATABASE_URL",
    "postgresql://postgres:postgres@postgres:5432/postgres",
).replace("postgresql+psycopg://", "postgresql://")

SIDECAR_BGE = os.environ.get("SIDECAR_BGE", "http://sidecar:8089").rstrip("/")
SIDECAR_OCTEN = os.environ.get("SIDECAR_OCTEN", "http://sidecar-octen:8089").rstrip("/")

CHUNK_SIZE = int(os.environ.get("CHUNK_SIZE", "3072"))
CHUNK_OVERLAP = int(os.environ.get("CHUNK_OVERLAP", "460"))
EMBED_BATCH = int(os.environ.get("EMBED_BATCH", "32"))

REDIS_URL = os.environ.get("REDIS_URL", "redis://redis:6379")

_stop = False


def _request_stop(*_a: Any) -> None:
    global _stop
    if _stop:
        log.warning("second stop signal — exiting hard")
        sys.exit(1)
    log.info("stop requested, finishing current doc then exiting")
    _stop = True


signal.signal(signal.SIGINT, _request_stop)
signal.signal(signal.SIGTERM, _request_stop)


splitter = RecursiveCharacterTextSplitter(
    chunk_size=CHUNK_SIZE, chunk_overlap=CHUNK_OVERLAP
)


async def claim_next_doc(conn: psycopg.AsyncConnection) -> dict[str, Any] | None:
    """Atomically grab the next pending doc and mark in_progress."""
    async with conn.cursor(row_factory=dict_row) as cur:
        # Recover any in_progress rows older than 10 min (worker died).
        await cur.execute(
            """
            UPDATE platform.reingest_progress
            SET status='pending', updated_at=now()
            WHERE status='in_progress' AND updated_at < now() - INTERVAL '10 minutes'
            """
        )
        await cur.execute(
            """
            UPDATE platform.reingest_progress p
            SET status='in_progress',
                attempts=attempts+1,
                started_at=COALESCE(started_at, now()),
                updated_at=now()
            FROM (
                SELECT document_id FROM platform.reingest_progress
                WHERE status='pending'
                ORDER BY updated_at
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            ) c
            WHERE p.document_id = c.document_id
            RETURNING p.document_id, p.attempts
            """
        )
        row = await cur.fetchone()
        await conn.commit()
        return row


async def get_doc_chunks(
    conn: psycopg.AsyncConnection, document_id: str
) -> list[dict[str, Any]]:
    """Pull existing chunks for a doc, ordered so concatenation matches
    the original document. R2R stores ``chunk_order`` in metadata."""
    async with conn.cursor(row_factory=dict_row) as cur:
        await cur.execute(
            """
            SELECT id::text AS id, text, metadata, owner_id, collection_ids
            FROM deepresearch.chunks
            WHERE document_id = %s
            ORDER BY COALESCE((metadata->>'chunk_order')::int, 0), id
            """,
            (document_id,),
        )
        return list(await cur.fetchall())


async def embed_batch(
    client: httpx.AsyncClient, base: str, texts: list[str], input_type: str | None
) -> list[list[float]]:
    payload: dict[str, Any] = {"input": texts}
    if input_type is not None:
        payload["input_type"] = input_type
    r = await client.post(f"{base}/v1/embeddings", json=payload, timeout=180.0)
    r.raise_for_status()
    return [item["embedding"] for item in r.json()["data"]]


async def embed_all(
    client: httpx.AsyncClient, base: str, texts: list[str], input_type: str | None
) -> list[list[float]]:
    out: list[list[float]] = []
    for i in range(0, len(texts), EMBED_BATCH):
        sub = texts[i : i + EMBED_BATCH]
        # truncate ultra-long chunks before sending (sidecar already truncates internally)
        out.extend(await embed_batch(client, base, [t[:24000] for t in sub], input_type))
    return out


async def enqueue_kg_task(document_id: str) -> None:
    """Enqueue a hi_worker kg_task to re-extract entities for the new chunks."""
    try:
        import redis.asyncio as redis_async
        from arq.connections import RedisSettings, create_pool

        # Reuse the platform_hi pool naming convention from hi_worker.py
        from yarl import URL

        url = URL(REDIS_URL)
        settings = RedisSettings(
            host=url.host or "redis",
            port=url.port or 6379,
            database=int((url.path or "/0").lstrip("/") or 0),
        )
        pool = await create_pool(settings, default_queue_name="platform:hi")
        await pool.enqueue_job("kg_task", document_id=document_id)
        await pool.close()
    except Exception as exc:  # noqa: BLE001
        # Non-fatal — the doc is still re-chunked; KG just stays stale
        # until the next manual sweep. Don't block re-ingestion on this.
        log.warning("kg_task enqueue failed for %s: %s", document_id[:8], exc)


async def reingest_one(
    conn: psycopg.AsyncConnection,
    http_client: httpx.AsyncClient,
    document_id: str,
    *,
    dry_run: bool,
) -> tuple[int, int]:
    """Returns (chunks_before, chunks_after)."""
    old_chunks = await get_doc_chunks(conn, document_id)
    if not old_chunks:
        log.info("[%s] no chunks — marking complete with 0/0", document_id[:8])
        return 0, 0

    # Re-assemble doc text. Most R2R chunks already overlap; concatenating
    # gives some duplication but the recursive splitter handles it fine
    # by producing fewer/wider chunks.
    doc_text = "\n\n".join(c["text"] or "" for c in old_chunks if c["text"])
    if not doc_text.strip():
        log.info("[%s] empty doc text — marking complete", document_id[:8])
        return len(old_chunks), 0

    new_texts = splitter.split_text(doc_text)
    if not new_texts:
        log.warning("[%s] splitter returned 0 chunks — leaving doc as-is", document_id[:8])
        return len(old_chunks), len(old_chunks)

    if dry_run:
        log.info(
            "[%s] DRY-RUN  old=%d (avg %d chars)  new=%d (avg %d chars)",
            document_id[:8],
            len(old_chunks),
            sum(len(c["text"] or "") for c in old_chunks) // max(len(old_chunks), 1),
            len(new_texts),
            sum(len(t) for t in new_texts) // max(len(new_texts), 1),
        )
        return len(old_chunks), len(new_texts)

    # Both embedders in parallel — they live on the GPU but the bench
    # sidecar typically has spare capacity while ingestion runs.
    bge_vecs, octen_vecs = await asyncio.gather(
        embed_all(http_client, SIDECAR_BGE, new_texts, input_type=None),
        embed_all(http_client, SIDECAR_OCTEN, new_texts, input_type="document"),
    )

    # Use the first old chunk to clone owner_id / collection_ids — these
    # are tenant-isolation keys and must stay consistent across re-chunk.
    template = old_chunks[0]
    owner_id = template["owner_id"]
    collection_ids = template["collection_ids"] or []

    new_chunks: list[dict[str, Any]] = []
    for i, text in enumerate(new_texts):
        new_chunks.append({
            "id": str(uuid.uuid4()),
            "document_id": document_id,
            "owner_id": owner_id,
            "collection_ids": collection_ids,
            "text": text,
            "metadata": {"chunk_order": i, "reingested": True},
            "vec_bge": "[" + ",".join(f"{x:.6f}" for x in bge_vecs[i]) + "]",
            "vec_octen": "[" + ",".join(f"{x:.6f}" for x in octen_vecs[i]) + "]",
        })

    async with conn.cursor() as cur:
        # CASCADE handles chunks_octen_embed rows for the deleted chunks
        await cur.execute(
            "DELETE FROM deepresearch.chunks WHERE document_id = %s", (document_id,)
        )
        await cur.executemany(
            """
            INSERT INTO deepresearch.chunks
                (id, document_id, owner_id, collection_ids, vec, text, metadata)
            VALUES (%s, %s, %s, %s, %s::vector, %s, %s::jsonb)
            """,
            [
                (
                    c["id"], c["document_id"], c["owner_id"], c["collection_ids"],
                    c["vec_bge"], c["text"],
                    psycopg.types.json.Jsonb(c["metadata"]),
                )
                for c in new_chunks
            ],
        )
        await cur.executemany(
            """
            INSERT INTO platform.chunks_octen_embed (chunk_id, document_id, vec)
            VALUES (%s, %s, %s::vector)
            """,
            [(c["id"], c["document_id"], c["vec_octen"]) for c in new_chunks],
        )
    await conn.commit()
    return len(old_chunks), len(new_chunks)


async def mark_done(
    conn: psycopg.AsyncConnection,
    document_id: str,
    chunks_before: int,
    chunks_after: int,
) -> None:
    async with conn.cursor() as cur:
        await cur.execute(
            """
            UPDATE platform.reingest_progress
            SET status='complete',
                chunks_before=%s, chunks_after=%s,
                finished_at=now(), updated_at=now(),
                last_error=NULL
            WHERE document_id=%s
            """,
            (chunks_before, chunks_after, document_id),
        )
    await conn.commit()


async def mark_failed(
    conn: psycopg.AsyncConnection, document_id: str, error: str
) -> None:
    try:
        async with conn.cursor() as cur:
            await cur.execute(
                """
                UPDATE platform.reingest_progress
                SET status='failed', last_error=%s, updated_at=now()
                WHERE document_id=%s
                """,
                (error[:2000], document_id),
            )
        await conn.commit()
    except Exception:  # noqa: BLE001
        log.exception("failed to mark %s as failed", document_id[:8])


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--limit", type=int, default=0, help="max docs to process (0 = unbounded)")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--no-kg", action="store_true", help="skip kg_task enqueue (faster)")
    args = parser.parse_args()

    t_start = time.perf_counter()
    processed = 0

    async with await psycopg.AsyncConnection.connect(DB_URL, autocommit=False) as conn, \
              httpx.AsyncClient() as http_client:
        async with conn.cursor() as cur:
            await cur.execute(
                "SELECT status, count(*) FROM platform.reingest_progress GROUP BY status ORDER BY 1"
            )
            rows = list(await cur.fetchall())
        await conn.commit()
        log.info("queue state: %s", {r[0]: r[1] for r in rows})

        while not _stop:
            if args.limit and processed >= args.limit:
                log.info("limit reached (%d), exiting", args.limit)
                break

            claimed = await claim_next_doc(conn)
            if not claimed:
                log.info("no pending docs, exiting")
                break

            doc_id = str(claimed["document_id"])
            attempt = claimed["attempts"]
            try:
                before, after = await reingest_one(
                    conn, http_client, doc_id, dry_run=args.dry_run
                )
                if not args.dry_run:
                    await mark_done(conn, doc_id, before, after)
                    if not args.no_kg and after > 0:
                        await enqueue_kg_task(doc_id)
                processed += 1
                rate = processed / max(time.perf_counter() - t_start, 1e-6)
                log.info(
                    "[%s] OK  %d→%d chunks  attempt=%d  total=%d  rate=%.2f docs/s",
                    doc_id[:8], before, after, attempt, processed, rate,
                )
            except Exception as exc:  # noqa: BLE001
                log.exception("[%s] FAILED on attempt %d", doc_id[:8], attempt)
                await mark_failed(conn, doc_id, str(exc))
                processed += 1

        # Final tally
        async with conn.cursor() as cur:
            await cur.execute(
                "SELECT status, count(*) FROM platform.reingest_progress GROUP BY status ORDER BY 1"
            )
            rows = list(await cur.fetchall())
        log.info("done. queue: %s  elapsed=%.1fs", {r[0]: r[1] for r in rows},
                 time.perf_counter() - t_start)


if __name__ == "__main__":
    with contextlib.suppress(KeyboardInterrupt):
        asyncio.run(main())
