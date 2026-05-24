# reindex-worker multi-stage Dockerfile.
#
# DEPRECATED: superseded by the consolidated platform/worker image. Kept so
# `bin/adam deploy --rollback` can rebuild the standalone worker. It still
# needs to compile because reindex/cmd is part of the workspace.
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because reindex/go.mod uses replace directives like ../../core that
# point at sibling sub-repos. --platform=$BUILDPLATFORM pins this stage
# to the workstation's native arch (amd64); GOARCH=$TARGETARCH cross-
# compiles to whatever the runtime image will be.
#
# CGO note: reindex pulls in r2g/chunking, which tokenizes via ztok's cgo
# binding (the ./ztok submodule). So this is now CGO_ENABLED=1 on
# debian/glibc — not the old alpine/musl CGO=0 static build. The `ztoklib`
# stage cross-compiles libztok.a with Zig and we static-link it.

# --- ztoklib: cross-compile libztok.a with Zig --------------------------------
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

# --- builder ----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends build-essential crossbuild-essential-arm64 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY --from=ztoklib /opt/ztok /opt/ztok
# Copy every sub-repo + go.work so workspace mode resolves the union
# of all member go.mods.
COPY go.work go.work.sum* ./
COPY core ./core
COPY ztok ./ztok
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

RUN set -eux; \
    CC=gcc; \
    if [ "${TARGETARCH}" = "arm64" ]; then CC=aarch64-linux-gnu-gcc; fi; \
    CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} CC="$CC" \
    CGO_CFLAGS="-I/opt/ztok/include" \
    CGO_LDFLAGS="-L/opt/ztok/lib" \
    go build -tags nopkgconfig -trimpath -ldflags='-s -w' \
    -o /out/reindex-worker ./knowledge/reindex/cmd/reindex-worker

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/reindex-worker /usr/local/bin/reindex-worker
ENTRYPOINT ["/usr/local/bin/reindex-worker"]
