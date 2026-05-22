# Multi-stage build for r2g.
#
# CGO_ENABLED=1 because r2g links go-fitz (MuPDF cgo bindings) for figure
# thumbnailing. go-fitz statically embeds MuPDF so we only need a libc-
# compatible C toolchain, not the arm64 mupdf system libraries.
#
# We cross-compile from a native amd64 builder rather than running the Go
# toolchain under qemu — qemu user-mode segfaults the arm64 Go compiler
# on certain packages (e.g. html-to-markdown/v2). See memory
# r2g-arm64-cgo-qemu-segfault.md. The pattern:
#   - --platform=$BUILDPLATFORM pins the builder stage to the host arch
#     (amd64) so the Go compiler itself runs natively.
#   - apt installs the arm64 C cross-toolchain (`crossbuild-essential-arm64`).
#   - CGO_ENABLED=1 + CC=aarch64-linux-gnu-gcc + GOARCH=$TARGETARCH lets Go
#     drive a cross-compile of both pure-Go and cgo code.
#
# Switched from alpine:3.21 (CGO_ENABLED=0) -> debian:trixie-slim
# multi-stage on 2026-05-15 when r2g picked up go-fitz.
#
# Builder is on trixie (glibc 2.40) — not bookworm — because go-fitz's
# pre-built libmupdfthird_linux_arm64.a references __isoc23_strtol /
# __isoc23_strtoul (C23 symbols added in glibc 2.38). Bookworm ships
# 2.36 and the link fails with "undefined reference to `__isoc23_strtol'".
# Runtime image is trixie too so the dynamic libc on both sides matches.
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS builder
ARG TARGETARCH

RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
         crossbuild-essential-arm64 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY r2g/go.mod r2g/go.sum ./
RUN go mod download
COPY r2g/ .

# Branch on TARGETARCH so a non-arm64 build still works (the workstation
# `bin/adam ship workstation r2g` path would land here with amd64).
RUN set -eux; \
    if [ "$TARGETARCH" = "arm64" ]; then \
        CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ \
        CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
            go build -ldflags="-s -w" -o /out/r2g ./cmd/r2g; \
    else \
        CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" \
            go build -ldflags="-s -w" -o /out/r2g ./cmd/r2g; \
    fi

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/dr-uploads
COPY --from=builder /out/r2g /usr/local/bin/r2g
ENTRYPOINT ["/usr/local/bin/r2g"]
