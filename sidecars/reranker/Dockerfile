# Cross-encoder reranker sidecar. CPU-only PyTorch — runs on the Pi.
# The image is bigger than ideal (~1.5 GB) because torch ships a lot
# of math kernels; we pin to the CPU wheel index to avoid the CUDA
# binaries that triple the size.

FROM python:3.12-slim AS base
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    HF_HOME=/models \
    TRANSFORMERS_OFFLINE=0

# Build-time deps for any wheels that don't have prebuilt arm64 binaries.
# kept minimal — most of torch/sentence-transformers ships prebuilt.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
# CPU-only torch index so we don't pull the CUDA build.
RUN pip install --extra-index-url https://download.pytorch.org/whl/cpu \
      -r requirements.txt

COPY app.py /app/app.py

# Warm the HF cache at build time so cold-starts on the Pi don't have
# to download the model the first time the container comes up.
ARG RERANKER_MODEL=BAAI/bge-reranker-base
RUN python -c "from sentence_transformers import CrossEncoder; \
    CrossEncoder('${RERANKER_MODEL}', device='cpu')" \
 && echo "warmed model: ${RERANKER_MODEL}"

EXPOSE 8091
ENV PORT=8091
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8091", "--log-level", "info"]
