"""CPU reranker shootout. For each candidate, load on CPU and score
title→doc pairs (10-doc pool, gold at index 0). Reports hit@1 and
50-pair latency.

Reads /tmp/lora_test_data.json (already present in the sidecar).
"""

from __future__ import annotations

import gc
import json
import os
import random
import re
import statistics
import time
import traceback
from pathlib import Path

import torch

os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
torch.set_num_threads(int(os.environ.get("CPU_THREADS", "24")))

CACHE = "/cache"
DATA = Path("/tmp/lora_test_data.json")
N_QUERIES = 20
POOL_SIZE = 10


def clean_title(title: str) -> str:
    t = title.removesuffix(".md").removesuffix(".pdf").strip()
    t = re.sub(r"^\d{4}\.\d{4,5}(v\d+)?\s*[-:]\s*", "", t)
    return t


def load_pool() -> list[dict]:
    with open(DATA) as f:
        rows = json.load(f)
    return [{"title": clean_title(r["title"]), "chunk": r["chunk_text"]} for r in rows]


def score_with_ce(model, pairs: list[list[str]], batch_size: int = 8) -> list[float]:
    """Generic CrossEncoder.predict path with sigmoid activation."""
    raw = model.predict(pairs, batch_size=batch_size, activation_fn=torch.nn.Sigmoid())
    try:
        return [float(x) for x in raw]
    except TypeError:
        return [float(raw)]


def bench_one(name: str, loader, score_fn, pool: list[dict]) -> dict:
    print(f"\n=== {name} ===", flush=True)
    t_load = time.perf_counter()
    try:
        model = loader()
    except Exception as exc:
        traceback.print_exc()
        return {"name": name, "error": str(exc)[:200]}
    load_s = time.perf_counter() - t_load
    print(f"  loaded in {load_s:.1f}s", flush=True)

    docs_for_query = pool[:N_QUERIES]
    correct = 0
    times: list[float] = []
    random.seed(17)
    for i, gold in enumerate(docs_for_query):
        distractors = [d for d in pool if d is not gold]
        random.shuffle(distractors)
        pool_chunks = [gold["chunk"]] + [d["chunk"] for d in distractors[: POOL_SIZE - 1]]
        pairs = [[gold["title"], c] for c in pool_chunks]
        t0 = time.perf_counter()
        try:
            scores = score_fn(model, pairs)
        except Exception as exc:
            print(f"  [{i}] score error: {exc}", flush=True)
            continue
        times.append(time.perf_counter() - t0)
        order = sorted(range(len(scores)), key=lambda j: scores[j], reverse=True)
        if order[0] == 0:
            correct += 1

    n = len(times)
    p_per_pair = statistics.mean(times) * 1000 / POOL_SIZE if times else float("nan")
    h1 = correct / n if n else 0.0
    print(f"  hit@1: {h1:.4f}  per-pair: {p_per_pair:.1f} ms  (50-pair extrapolated: {p_per_pair*50/1000:.2f}s)", flush=True)
    try:
        del model
        gc.collect()
    except Exception:
        pass
    return {"name": name, "hit@1": h1, "per_pair_ms": p_per_pair}


def _ce(name: str, **kw):
    from sentence_transformers import CrossEncoder
    return CrossEncoder(name, device="cpu", cache_folder=CACHE, **kw)


def main() -> None:
    pool = load_pool()
    print(f"pool size: {len(pool)} docs; running {N_QUERIES} queries × {POOL_SIZE}-doc pool", flush=True)

    # Encoder-based only. The three generative candidates (mxbai v2,
    # bge-v2.5-gemma2) are all Qwen2/Gemma ForCausalLM — slow on Zen 3
    # for the same reason Qwen3-Reranker is slow (no AVX-512/VNNI/AMX).
    candidates = [
        ("bge-reranker-v2-m3 (XLM-R-large encoder)",
            lambda: _ce("BAAI/bge-reranker-v2-m3"), score_with_ce),
        ("jina-reranker-v2-multilingual (XLM-R encoder)",
            lambda: _ce("jinaai/jina-reranker-v2-base-multilingual", trust_remote_code=True),
            score_with_ce),
    ]

    results = []
    for name, loader, scorer in candidates:
        results.append(bench_one(name, loader, scorer, pool))

    print("\n=== SUMMARY ===")
    print(f"{'model':<42}{'hit@1':>8}{'per-pair':>12}{'50-pair':>10}")
    for r in results:
        if "error" in r:
            print(f"{r['name']:<42}{'ERROR: '+r['error'][:30]:>30}")
        else:
            est_50 = r["per_pair_ms"] * 50 / 1000
            print(f"{r['name']:<42}{r['hit@1']:>8.4f}{r['per_pair_ms']:>10.1f}ms{est_50:>9.2f}s")


if __name__ == "__main__":
    main()
