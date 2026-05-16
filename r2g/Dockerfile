# Multi-stage build: compile r2g inside the same Debian bookworm-slim
# environment we run it in, so the cgo binary links against bookworm
# glibc (2.36) instead of whatever lives on the workstation.
#
# Build with:
#   docker buildx build --platform linux/arm64 \
#     -t evo-r2g:dev --output type=docker \
#     -f r2g/Dockerfile .
#
# (Note: build context is the repo root, not bin/. The builder stage
# needs go.mod/go.sum/source in scope.)
#
# Switched from alpine:3.21 (CGO_ENABLED=0) -> debian:bookworm-slim
# multi-stage on 2026-05-15 when r2g picked up go-fitz (MuPDF cgo)
# for figure thumbnailing.

# Builder runs under buildx-managed QEMU when targeting a different
# arch than the host. Slower than a true cross-compile, but it dodges
# the cgo sysroot complication: go-fitz statically links libmupdf, but
# the resulting binary still depends on libc, and that libc must be
# the target's. Letting buildx-QEMU run the Go toolchain on arm64
# directly produces a binary linked against bookworm's glibc 2.36 by
# construction.
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY r2g/go.mod r2g/go.sum ./
RUN go mod download
COPY r2g/ .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /out/r2g ./cmd/r2g

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/dr-uploads
COPY --from=builder /out/r2g /usr/local/bin/r2g
ENTRYPOINT ["/usr/local/bin/r2g"]
