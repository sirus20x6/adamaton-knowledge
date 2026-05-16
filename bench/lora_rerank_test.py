"""Load Octen + extracted LoRA + extras, reconstruct a Qwen3ForCausalLM
state, score (query, doc) pairs the same way Qwen3-Reranker would.

Compares against the actual Qwen3-Reranker on a small set of pairs to
sanity-check correctness, then runs the title benchmark.
"""

from __future__ import annotations

import argparse
import os
import sys
import time
from pathlib import Path

import torch
import re
import statistics

LORA_DIR = Path("/tmp/qwen3_rerank_lora")
CACHE = "/cache"
BASE_NAME = "Octen/Octen-Embedding-0.6B"
TARGET_NAME = "Qwen/Qwen3-Reranker-0.6B"

os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
torch.set_num_threads(int(os.environ.get("CPU_THREADS", "24")))


def reconstruct_state(base_state: dict, lora: dict, extras: dict) -> dict:
    """Build target state from base + lora + extras."""
    # Strip Qwen3Model prefix mismatch — base has bare names (layers.X.…),
    # target wraps them under "model." prefix.
    out: dict[str, torch.Tensor] = {}

    # First: copy base weights, possibly modified by LoRA/extra deltas
    lora_targets = {k[:-2] for k in lora.keys() if k.endswith(".A")}
    extra_norms = {k for k in extras.keys() if "norm" in k}
    extra_replacements = {k for k in extras.keys() if k not in extra_norms}

    for name, weight in base_state.items():
        if name in lora_targets:
            A = lora[f"{name}.A"].to(torch.float32)
            B = lora[f"{name}.B"].to(torch.float32)
            delta = A @ B
            out[name] = (weight.to(torch.float32) + delta).to(torch.float32)
        elif name in extra_norms:
            delta = extras[name].to(torch.float32)
            out[name] = (weight.to(torch.float32) + delta).to(torch.float32)
        elif name in extra_replacements:
            out[name] = extras[name].to(torch.float32)
        else:
            out[name] = weight.to(torch.float32)

    # Anything in extras NOT in base — add it (e.g., final norm.weight,
    # embed_tokens if base names differ)
    for name in extras:
        if name in out:
            continue
        out[name] = extras[name].to(torch.float32)

    return out


def load_reconstructed_model() -> "tuple[Any, Any]":
    from transformers import AutoModel, AutoModelForCausalLM, AutoTokenizer

    print("loading base + adapter…", flush=True)
    t0 = time.perf_counter()
    base = AutoModel.from_pretrained(BASE_NAME, cache_dir=CACHE, torch_dtype=torch.float32)
    base_state = base.state_dict()
    # base state has bare key prefixes (no "model.") — that's what we
    # extracted against, so this matches.

    lora = torch.load(LORA_DIR / "lora.pt", weights_only=True)
    extras = torch.load(LORA_DIR / "extras.pt", weights_only=True)

    print(f"  loaded base + adapter in {time.perf_counter()-t0:.1f}s", flush=True)

    print("reconstructing target state…", flush=True)
    t0 = time.perf_counter()
    target_state = reconstruct_state(base_state, lora, extras)
    print(f"  reconstructed in {time.perf_counter()-t0:.1f}s  ({len(target_state)} tensors)", flush=True)

    # Build a fresh Qwen3ForCausalLM with config from the reranker, then
    # load our reconstructed state into the body.
    from transformers import AutoConfig
    cfg = AutoConfig.from_pretrained(TARGET_NAME, cache_dir=CACHE)
    model = AutoModelForCausalLM.from_config(cfg, dtype=torch.float32)

    # Map base-state keys (which lack "model." prefix) to
    # Qwen3ForCausalLM's key naming (which has "model." prefix).
    namespaced = {f"model.{k}": v for k, v in target_state.items()}
    # lm_head ties to embed_tokens. With tie_word_embeddings=True, HF
    # creates lm_head as a Linear that shares weight with embed_tokens.
    # We just need embed_tokens loaded; the tie should follow.

    missing, unexpected = model.load_state_dict(namespaced, strict=False)
    print(f"  load_state_dict: missing={len(missing)} unexpected={len(unexpected)}", flush=True)
    if missing:
        print(f"    missing (first 5): {missing[:5]}")
    if unexpected:
        print(f"    unexpected (first 5): {unexpected[:5]}")

    model.eval()
    tokenizer = AutoTokenizer.from_pretrained(TARGET_NAME, cache_dir=CACHE, padding_side="left")
    return model, tokenizer


def load_original_reranker() -> "tuple[Any, Any]":
    from transformers import AutoModelForCausalLM, AutoTokenizer
    print("loading original reranker for ground truth…", flush=True)
    model = AutoModelForCausalLM.from_pretrained(TARGET_NAME, cache_dir=CACHE, torch_dtype=torch.float32)
    model.eval()
    tokenizer = AutoTokenizer.from_pretrained(TARGET_NAME, cache_dir=CACHE, padding_side="left")
    return model, tokenizer


# Qwen3-Reranker scoring protocol
INSTRUCTION = "Given a web search query, retrieve relevant passages that answer the query"
PREFIX = "<|im_start|>system\nJudge whether the Document meets the requirements based on the Query and the Instruct provided. Note that the answer can only be \"yes\" or \"no\".<|im_end|>\n<|im_start|>user\n"
SUFFIX = "<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"


def format_pair(query: str, doc: str) -> str:
    return f"{PREFIX}<Instruct>: {INSTRUCTION}\n<Query>: {query}\n<Document>: {doc}{SUFFIX}"


@torch.no_grad()
def score_pairs(model, tokenizer, query: str, docs: list[str], batch_size: int = 8) -> list[float]:
    yes_id = tokenizer.convert_tokens_to_ids("yes")
    no_id = tokenizer.convert_tokens_to_ids("no")
    scores: list[float] = []
    for i in range(0, len(docs), batch_size):
        batch = [format_pair(query, d) for d in docs[i : i + batch_size]]
        enc = tokenizer(batch, padding=True, truncation=True, max_length=2048, return_tensors="pt")
        out = model(**enc).logits[:, -1, :]
        yes_logit = out[:, yes_id]
        no_logit = out[:, no_id]
        # P(yes) / (P(yes) + P(no))
        stacked = torch.stack([yes_logit, no_logit], dim=-1)
        probs = torch.softmax(stacked, dim=-1)[:, 0]
        scores.extend(probs.tolist())
    return scores


def sanity_check(recon_model, orig_model, tokenizer) -> None:
    print("\n=== sanity check: 3 pairs, compare logits ===")
    pairs = [
        ("flash attention IO awareness",
         "FlashAttention is an IO-aware exact attention algorithm that tiles attention to fit in SRAM"),
        ("flash attention IO awareness",
         "A recipe for chocolate chip cookies with flour, sugar, and butter."),
        ("muon optimizer",
         "AdaMuon: Adaptive Muon Optimizer for training large language models."),
    ]
    queries = [p[0] for p in pairs]
    docs = [p[1] for p in pairs]
    # Score one-at-a-time so each pair gets its own query
    recon_scores = [score_pairs(recon_model, tokenizer, q, [d])[0] for q, d in pairs]
    orig_scores = [score_pairs(orig_model, tokenizer, q, [d])[0] for q, d in pairs]
    for (q, d), rs, os_ in zip(pairs, recon_scores, orig_scores):
        diff = rs - os_
        print(f"  q={q[:35]!r:38s}  recon={rs:.4f}  orig={os_:.4f}  diff={diff:+.4f}")


def clean_title(title: str) -> str:
    t = title.removesuffix(".md").removesuffix(".pdf").strip()
    t = re.sub(r"^\d{4}\.\d{4,5}(v\d+)?\s*[-:]\s*", "", t)
    return t


def title_benchmark_with_model(model, tokenizer, label: str, n_queries: int = 30) -> None:
    """Mini title benchmark: pick N papers, use title as query, see if
    the reranker promotes the correct doc to position 1 in a small pool."""
    import json, random
    with open("/tmp/lora_test_data.json") as f:
        rows = json.load(f)
    random.seed(17)
    docs = []
    for r in rows[:n_queries]:
        docs.append({"title": clean_title(r["title"]), "chunks": [r["chunk_text"]]})
    correct = 0
    n = 0
    times = []
    for i, gold in enumerate(docs):
        if not gold["chunks"]:
            continue
        gold_chunk = gold["chunks"][0]
        distractors = [d for d in docs if d is not gold]
        random.shuffle(distractors)
        pool = [gold_chunk] + [d["chunks"][0] for d in distractors[:9]]
        # Track gold position (index 0)
        t0 = time.perf_counter()
        scores = score_pairs(model, tokenizer, gold["title"], pool, batch_size=8)
        times.append(time.perf_counter() - t0)
        rank = sorted(range(len(pool)), key=lambda j: scores[j], reverse=True).index(0)
        if rank == 0:
            correct += 1
        n += 1
    print(f"\n=== {label} ===")
    print(f"  hit@1 over {n} title→doc queries (10-doc pool): {correct/n:.4f}")
    print(f"  avg latency per 10-doc rerank: {statistics.mean(times)*1000:.1f} ms")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--n-queries", type=int, default=20)
    parser.add_argument("--skip-orig", action="store_true")
    parser.add_argument("--int8", action="store_true", help="also test INT8 dynamic quant variants")
    parser.add_argument("--fp16", action="store_true", help="also test FP16 cast")
    parser.add_argument("--bf16", action="store_true", help="also test BF16 cast")
    args = parser.parse_args()

    recon_model, recon_tok = load_reconstructed_model()
    print(f"\nreconstructed model params: {sum(p.numel() for p in recon_model.parameters())/1e6:.1f}M")

    if not args.skip_orig:
        orig_model, orig_tok = load_original_reranker()
        sanity_check(recon_model, orig_model, recon_tok)

    title_benchmark_with_model(recon_model, recon_tok, "RECONSTRUCTED (LoRA r=128, FP32)", args.n_queries)
    if not args.skip_orig:
        title_benchmark_with_model(orig_model, recon_tok, "ORIGINAL Qwen3-Reranker (FP32)", args.n_queries)

    if args.int8:
        print("\nquantizing reconstructed model to INT8 (dynamic, Linear only)…")
        t0 = time.perf_counter()
        recon_int8 = torch.quantization.quantize_dynamic(
            recon_model, {torch.nn.Linear}, dtype=torch.qint8
        )
        print(f"  done in {time.perf_counter()-t0:.1f}s")
        title_benchmark_with_model(recon_int8, recon_tok, "RECONSTRUCTED + INT8", args.n_queries)

        if not args.skip_orig:
            print("\nquantizing original reranker to INT8 (dynamic, Linear only)…")
            t0 = time.perf_counter()
            orig_int8 = torch.quantization.quantize_dynamic(
                orig_model, {torch.nn.Linear}, dtype=torch.qint8
            )
            print(f"  done in {time.perf_counter()-t0:.1f}s")
            title_benchmark_with_model(orig_int8, recon_tok, "ORIGINAL + INT8", args.n_queries)

    if args.fp16:
        print("\ncasting reconstructed model to FP16…")
        recon_fp16 = recon_model.to(torch.float16)
        title_benchmark_with_model(recon_fp16, recon_tok, "RECONSTRUCTED + FP16", args.n_queries)
        if not args.skip_orig:
            print("\ncasting original reranker to FP16…")
            orig_fp16 = orig_model.to(torch.float16)
            title_benchmark_with_model(orig_fp16, recon_tok, "ORIGINAL + FP16", args.n_queries)

    if args.bf16:
        print("\ncasting reconstructed model to BF16…")
        # reload from disk since we may have mutated above
        recon_bf16, _ = load_reconstructed_model()
        recon_bf16 = recon_bf16.to(torch.bfloat16)
        title_benchmark_with_model(recon_bf16, recon_tok, "RECONSTRUCTED + BF16", args.n_queries)


if __name__ == "__main__":
    main()
