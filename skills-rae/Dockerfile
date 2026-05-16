# Multi-stage build for skills-rae. Mirrors r2g/Dockerfile but without
# the cgo/MuPDF dependency — skills-rae is pure Go.
#
# Build:
#   docker buildx build --platform linux/arm64 \
#     -t evo-skills-rae:dev --output type=docker \
#     -f skills-rae/Dockerfile .
#
# Context: repo root (not skills-rae/) so the builder stage can see
# go.work + sibling modules if cross-references creep in later.

FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY skills-rae/go.mod skills-rae/go.sum* ./
RUN go mod download || true
COPY skills-rae/ .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/skills-rae ./cmd/skills-rae

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/skills-rae /usr/local/bin/skills-rae
ENTRYPOINT ["/usr/local/bin/skills-rae"]
