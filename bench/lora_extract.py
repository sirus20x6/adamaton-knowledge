"""Extract a low-rank adapter from delta(Qwen3-Reranker - Octen) per
transformer layer, save it as a torch state dict.

Approach: for each Linear layer in the transformer body (q_proj, k_proj,
v_proj, o_proj, gate_proj, up_proj, down_proj), compute delta = W_r - W_o
and SVD it down to rank ``r``. Store A = U * sqrt(S), B = sqrt(S) * Vt.

At inference time the reconstructed weight is W_o + A @ B (so we can
either merge it back into a single matrix or apply on-the-fly).

Embedding matrix (151669x1024) is the largest weight in the model; we
leave it as-is (use Octen's). If quality cratters this is the most likely
culprit and we'd extend the extraction.
"""

from __future__ import annotations

import argparse
import json
import os
import time
from pathlib import Path

import torch
from transformers import AutoModel, AutoModelForCausalLM

CACHE = "/cache"
BASE_NAME = "Octen/Octen-Embedding-0.6B"
TARGET_NAME = "Qwen/Qwen3-Reranker-0.6B"
OUT_DIR = Path("/tmp/qwen3_rerank_lora")

# Which Linear modules to LoRA-target. Skip the embedding (huge) and the
# LM head (tied to embedding when tie_word_embeddings=True).
TARGET_PATTERNS = [
    "self_attn.q_proj.weight",
    "self_attn.k_proj.weight",
    "self_attn.v_proj.weight",
    "self_attn.o_proj.weight",
    "mlp.gate_proj.weight",
    "mlp.up_proj.weight",
    "mlp.down_proj.weight",
]
# Per-layer norms have very few params; we just copy the deltas verbatim
# (no SVD). Same for any residual params not covered by TARGET_PATTERNS.
COPY_PATTERNS = [
    "input_layernorm.weight",
    "post_attention_layernorm.weight",
    "self_attn.q_norm.weight",
    "self_attn.k_norm.weight",
]


def _matches(name: str, patterns: list[str]) -> bool:
    return any(p in name for p in patterns)


def _strip_prefix(state: dict[str, torch.Tensor]) -> dict[str, torch.Tensor]:
    """Reranker has prefix 'model.' from Qwen3ForCausalLM wrapper; Octen
    is just 'layers.', etc. Strip prefixes so keys align."""
    out = {}
    for k, v in state.items():
        new_k = k
        if new_k.startswith("model."):
            new_k = new_k[len("model.") :]
        out[new_k] = v
    return out


def extract_lora(rank: int) -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    print(f"loading base ({BASE_NAME}) and target ({TARGET_NAME})…", flush=True)
    t0 = time.perf_counter()
    base = AutoModel.from_pretrained(BASE_NAME, cache_dir=CACHE, torch_dtype=torch.float32)
    target = AutoModelForCausalLM.from_pretrained(
        TARGET_NAME, cache_dir=CACHE, torch_dtype=torch.float32
    )
    print(f"  loaded in {time.perf_counter()-t0:.1f}s", flush=True)

    base_state = _strip_prefix(base.state_dict())
    target_state = _strip_prefix(target.state_dict())

    lora: dict[str, torch.Tensor] = {}
    extras: dict[str, torch.Tensor] = {}
    skipped: list[str] = []

    target_pat_count = 0
    copy_pat_count = 0

    for name in sorted(target_state.keys()):
        if name == "lm_head.weight":
            # tied to embed_tokens (we'll reuse Octen's tied head)
            continue
        if name not in base_state:
            # not in base — keep verbatim from target (e.g., reranker-specific)
            extras[name] = target_state[name].clone()
            skipped.append(name)
            continue

        if not torch.is_floating_point(target_state[name]):
            continue

        if _matches(name, TARGET_PATTERNS):
            # full SVD extraction
            delta = (target_state[name] - base_state[name]).to(torch.float32)
            # delta shape: [out_features, in_features]
            U, S, Vh = torch.linalg.svd(delta, full_matrices=False)
            r = min(rank, S.shape[0])
            sqrt_S = torch.sqrt(S[:r])
            A = U[:, :r] * sqrt_S            # [out, r]
            B = sqrt_S.unsqueeze(1) * Vh[:r] # [r, in]
            lora[f"{name}.A"] = A.to(torch.float16)
            lora[f"{name}.B"] = B.to(torch.float16)
            target_pat_count += 1
            # report retained energy
            retained = (S[:r] ** 2).sum() / (S ** 2).sum()
            if target_pat_count <= 3 or target_pat_count % 56 == 0:
                print(f"  [{target_pat_count:3d}] {name:60s} shape={tuple(delta.shape)}  rank={r}  retained={retained:.3f}", flush=True)
        elif _matches(name, COPY_PATTERNS):
            extras[name] = (target_state[name] - base_state[name]).to(torch.float16)
            copy_pat_count += 1
        else:
            # Catch-all for layers not in either pattern — copy the
            # raw target weight to be safe. embed_tokens lives here.
            extras[name] = target_state[name].clone().to(torch.float16)

    meta = {
        "rank": rank,
        "base_model": BASE_NAME,
        "target_model": TARGET_NAME,
        "num_lora_pairs": target_pat_count,
        "num_copy_deltas": copy_pat_count,
        "num_extras": len(extras),
        "skipped_keys": skipped,
    }
    print(f"\nlora pairs: {target_pat_count}, copy deltas: {copy_pat_count}, extras: {len(extras)}")

    # serialize
    torch.save(lora, OUT_DIR / "lora.pt")
    torch.save(extras, OUT_DIR / "extras.pt")
    (OUT_DIR / "meta.json").write_text(json.dumps(meta, indent=2))

    # size report
    sz_lora = (OUT_DIR / "lora.pt").stat().st_size / 1e6
    sz_extras = (OUT_DIR / "extras.pt").stat().st_size / 1e6
    print(f"\nadapter on disk:")
    print(f"  lora.pt   = {sz_lora:7.1f} MB")
    print(f"  extras.pt = {sz_extras:7.1f} MB")
    print(f"  total     = {sz_lora + sz_extras:7.1f} MB")
    print(f"\nbaseline target model (fp16, weights only): ~1200 MB")
    print(f"savings if shared: ~{1200 - sz_lora - sz_extras:.0f} MB")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--rank", type=int, default=128)
    args = parser.parse_args()
    extract_lora(args.rank)
