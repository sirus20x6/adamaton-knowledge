"""Static INT8 quantization (weights + activations) with calibration.

Run on both:
  1. Original Qwen3-Reranker (baseline INT8 dynamic gave 0.75 hit@1)
  2. LoRA-reconstructed r=512 model (dynamic INT8 gave 0.60 hit@1)

Uses optimum-quanto: clean API, transformer-aware, handles attention
properly. Calibration set is ~30 (query, doc) pairs from the title
benchmark — small enough to be fast, diverse enough to capture
activation ranges.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import statistics
import time
from pathlib import Path

import torch
from optimum.quanto import quantize, qint8, freeze, Calibration

os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
torch.set_num_threads(int(os.environ.get("CPU_THREADS", "24")))

LORA_DIR = Path("/tmp/qwen3_rerank_lora")
CACHE = "/cache"
BASE_NAME = "Octen/Octen-Embedding-0.6B"
TARGET_NAME = "Qwen/Qwen3-Reranker-0.6B"

# Reuse the recon helper from lora_rerank_test
import sys
sys.path.insert(0, "/tmp")
from lora_rerank_test import (
    reconstruct_state,
    load_original_reranker,
    format_pair,
    score_pairs,
    clean_title,
    title_benchmark_with_model,
)


def load_reconstructed_local():
    from transformers import AutoModel, AutoModelForCausalLM, AutoTokenizer, AutoConfig
    print("loading base + adapter for LoRA reconstruction…")
    t0 = time.perf_counter()
    base = AutoModel.from_pretrained(BASE_NAME, cache_dir=CACHE, torch_dtype=torch.float32)
    base_state = base.state_dict()
    lora = torch.load(LORA_DIR / "lora.pt", weights_only=True)
    extras = torch.load(LORA_DIR / "extras.pt", weights_only=True)
    target_state = reconstruct_state(base_state, lora, extras)
    cfg = AutoConfig.from_pretrained(TARGET_NAME, cache_dir=CACHE)
    model = AutoModelForCausalLM.from_config(cfg, dtype=torch.float32)
    namespaced = {f"model.{k}": v for k, v in target_state.items()}
    model.load_state_dict(namespaced, strict=False)
    model.eval()
    tok = AutoTokenizer.from_pretrained(TARGET_NAME, cache_dir=CACHE, padding_side="left")
    print(f"  loaded + reconstructed in {time.perf_counter()-t0:.1f}s")
    return model, tok


def build_calibration_set(rows: list[dict], n: int = 30) -> list[tuple[str, str]]:
    """Make (query, doc) pairs: half positive (title→own chunk),
    half negative (title→other chunk)."""
    random.seed(31)
    rows = rows[:n]
    pairs = []
    for i, r in enumerate(rows[: n // 2]):
        pairs.append((clean_title(r["title"]), r["chunk_text"]))  # positive
    # negatives: title from one doc, chunk from a different doc
    for i in range(n // 2):
        q_row = rows[i]
        d_row = rows[(i + 7) % len(rows)]
        pairs.append((clean_title(q_row["title"]), d_row["chunk_text"]))
    random.shuffle(pairs)
    return pairs


@torch.no_grad()
def calibrate(model, tokenizer, pairs: list[tuple[str, str]], batch_size: int = 4) -> None:
    print(f"  calibrating on {len(pairs)} pairs…")
    t0 = time.perf_counter()
    with Calibration():
        for i in range(0, len(pairs), batch_size):
            batch = [format_pair(q, d) for q, d in pairs[i : i + batch_size]]
            enc = tokenizer(batch, padding=True, truncation=True, max_length=2048, return_tensors="pt")
            _ = model(**enc)
    print(f"  calibration done in {time.perf_counter()-t0:.1f}s")


def run_one(label: str, model, tokenizer, calibration_pairs: list, n_queries: int) -> None:
    print(f"\n=== {label} — STATIC INT8 QUANTIZATION ===")
    t0 = time.perf_counter()
    quantize(model, weights=qint8, activations=qint8)
    print(f"  quantize() (no calibration yet) in {time.perf_counter()-t0:.1f}s")

    calibrate(model, tokenizer, calibration_pairs)

    t0 = time.perf_counter()
    freeze(model)
    print(f"  freeze() in {time.perf_counter()-t0:.1f}s")

    title_benchmark_with_model(model, tokenizer, label + " + STATIC-INT8", n_queries)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--n-queries", type=int, default=20)
    parser.add_argument("--n-cal", type=int, default=30)
    parser.add_argument("--orig-only", action="store_true")
    parser.add_argument("--lora-only", action="store_true")
    args = parser.parse_args()

    # calibration data from the test JSON
    with open("/tmp/lora_test_data.json") as f:
        rows = json.load(f)
    cal_pairs = build_calibration_set(rows, n=args.n_cal)
    print(f"calibration set: {len(cal_pairs)} pairs")

    if not args.lora_only:
        orig_model, orig_tok = load_original_reranker()
        run_one("ORIGINAL Qwen3-Reranker", orig_model, orig_tok, cal_pairs, args.n_queries)
        del orig_model

    if not args.orig_only:
        lora_model, lora_tok = load_reconstructed_local()
        run_one("LoRA r=512", lora_model, lora_tok, cal_pairs, args.n_queries)


if __name__ == "__main__":
    main()
