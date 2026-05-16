"""Cross-encoder reranker sidecar.

Stage-2 reranker for r2g's hybrid retrieval. r2g's HybridSearch
fuses BM25 + pgvector with RRF, then forwards the top-K hits here.
We score every (query, passage) pair with a BGE cross-encoder and
return the new ordering.

Why a cross-encoder, not another bi-encoder pass: the bi-encoder
embeddings r2g already uses encode query and document INDEPENDENTLY,
which is fast to ANN-index but blind to query-document interactions
(negations, exact-phrase requirements, n-ary relations). Cross-encoders
score the concatenated pair in a single forward pass, so they catch
those interactions at the cost of being O(K) rather than O(log N).
This is exactly why they show up at stage 2: K is small.

Default model: ``BAAI/bge-reranker-base``. The 'v2-m3' variant exists
but is multilingual and 2x the size; for an English research corpus
the base model wins on latency. Override via ``RERANKER_MODEL`` env.

The model loads at startup so the first /rerank call is fast. Memory
budget: ~500MB peak (model weights + activations for batch=16 on CPU).
The Pi has ~1.7GB available so we sit comfortably.
"""

from __future__ import annotations

import logging
import os
import time
from contextlib import asynccontextmanager
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from sentence_transformers import CrossEncoder

logger = logging.getLogger(__name__)
logging.basicConfig(
    level=os.environ.get("LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)

# Module-level singleton so the model isn't reloaded per request.
_model: CrossEncoder | None = None
_model_name: str = "unloaded"


def _load_model() -> CrossEncoder:
    """Load the cross-encoder. Cached locally by sentence-transformers."""

    global _model, _model_name
    if _model is not None:
        return _model
    name = os.environ.get("RERANKER_MODEL", "BAAI/bge-reranker-base")
    # max_length=512 is the model's native limit; longer passages get
    # truncated. That's an acceptable loss for paragraph-length chunks.
    max_length = int(os.environ.get("RERANKER_MAX_LENGTH", "512"))
    logger.info("loading reranker model: %s (max_length=%d)", name, max_length)
    t0 = time.monotonic()
    _model = CrossEncoder(name, max_length=max_length, device="cpu")
    elapsed = time.monotonic() - t0
    _model_name = name
    logger.info("reranker model loaded in %.1fs", elapsed)
    return _model


@asynccontextmanager
async def lifespan(_app: FastAPI):
    """Eager-load at startup so the first /rerank doesn't pay model-load cost."""

    _load_model()
    yield


app = FastAPI(title="reranker", version="0.1.0", lifespan=lifespan)


class RerankRequest(BaseModel):
    query: str = Field(..., min_length=1, description="The user query.")
    passages: list[str] = Field(
        ...,
        min_length=1,
        max_length=200,
        description="Candidate passages to score against the query.",
    )
    top_k: int | None = Field(
        None,
        ge=1,
        le=200,
        description=(
            "If set, return only the top_k highest-scoring passages "
            "(preserving original indices). Caller still gets all "
            "scores in ``scores`` when omitted."
        ),
    )


class RerankedHit(BaseModel):
    index: int  # original passage index in the request
    score: float


class RerankResponse(BaseModel):
    model: str
    elapsed_ms: int
    ranked: list[RerankedHit]


@app.get("/health")
def health() -> dict[str, Any]:
    return {"ok": True, "model": _model_name, "loaded": _model is not None}


@app.post("/rerank", response_model=RerankResponse)
def rerank(req: RerankRequest) -> RerankResponse:
    """Score each (query, passage) pair and return descending-score order.

    ``index`` is the caller's original passage index so the caller can
    re-attach metadata without sending it through us. We never see
    document ids — just text — to keep the surface narrow.
    """

    model = _load_model()
    t0 = time.monotonic()
    # CrossEncoder.predict batches internally; passing a flat list of
    # pairs is fastest. ``convert_to_numpy`` keeps the return type
    # predictable (we then coerce to floats).
    pairs = [(req.query, p) for p in req.passages]
    try:
        scores = model.predict(pairs, convert_to_numpy=True, show_progress_bar=False)
    except Exception as exc:  # noqa: BLE001
        logger.exception("rerank: model.predict failed")
        raise HTTPException(status_code=500, detail=f"predict failed: {exc}") from exc

    ranked = sorted(
        [RerankedHit(index=i, score=float(s)) for i, s in enumerate(scores)],
        key=lambda h: h.score,
        reverse=True,
    )
    if req.top_k is not None and req.top_k < len(ranked):
        ranked = ranked[: req.top_k]
    elapsed_ms = int((time.monotonic() - t0) * 1000)
    return RerankResponse(
        model=_model_name,
        elapsed_ms=elapsed_ms,
        ranked=ranked,
    )
