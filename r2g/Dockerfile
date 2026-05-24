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
# --- ztoklib: cross-compile libztok.a with Zig --------------------------------
# r2g/chunking tokenizes via ztok's cgo binding. Cross-build libztok.a with
# Zig (BUILDPLATFORM/amd64 host, cross-targeting the runtime arch) and
# static-link it below. go-fitz already made this image CGO=1/debian, so
# ztok is purely additive.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS ztoklib
ARG TARGETARCH
ARG ZIG_VERSION=0.16.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz" \
      | tar -xJ -C /opt \
    && ln -s /opt/zig-x86_64-linux-${ZIG_VERSION}/zig /usr/local/bin/zig
WORKDIR /src
COPY ztok ./ztok
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) ZTARGET=x86_64-linux-gnu ;; \
      arm64) ZTARGET=aarch64-linux-gnu ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    cd ztok && zig build -Doptimize=ReleaseFast -Dtarget="${ZTARGET}" -p /opt/ztok; \
    rm -f /opt/ztok/lib/libztok.so*   # force the cgo link to pick libztok.a

FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS builder
ARG TARGETARCH

RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
         crossbuild-essential-arm64 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY --from=ztoklib /opt/ztok /opt/ztok
# r2g/go.mod replaces ztok-go with ../../ztok/bindings/go; from the module
# root at /src that resolves to /ztok/bindings/go, so stage the submodule
# there before `go mod download` reads the replace target. (Build context is
# the umbrella root, so ztok/ and knowledge/r2g/ are umbrella-relative.)
COPY ztok /ztok
COPY knowledge/r2g/go.mod knowledge/r2g/go.sum ./
RUN go mod download
COPY knowledge/r2g/ .

# Branch on TARGETARCH so a non-arm64 build still works (the workstation
# `bin/adam ship workstation r2g` path would land here with amd64). Build
# the ztok binding with -tags nopkgconfig and point cgo at the cross-built
# libztok via CGO_CFLAGS/CGO_LDFLAGS.
RUN set -eux; \
    if [ "$TARGETARCH" = "arm64" ]; then \
        CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ \
        CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
        CGO_CFLAGS="-I/opt/ztok/include" CGO_LDFLAGS="-L/opt/ztok/lib" \
            go build -tags nopkgconfig -ldflags="-s -w" -o /out/r2g ./cmd/r2g; \
    else \
        CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" \
        CGO_CFLAGS="-I/opt/ztok/include" CGO_LDFLAGS="-L/opt/ztok/lib" \
            go build -tags nopkgconfig -ldflags="-s -w" -o /out/r2g ./cmd/r2g; \
    fi

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata poppler-utils \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/dr-uploads
COPY --from=builder /out/r2g /usr/local/bin/r2g
ENTRYPOINT ["/usr/local/bin/r2g"]
