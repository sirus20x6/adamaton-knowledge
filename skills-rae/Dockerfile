# skills-rae multi-stage Dockerfile.
#
# Builds BOTH binaries (skills-rae service + skills-rae-worker) into
# the same image; the compose service block picks one via entrypoint.
# Default entrypoint is the HTTP service; the catalog's skills-rae-
# worker.yml overrides it to /usr/local/bin/skills-rae-worker. Same
# pattern as deepresearch/nano-research.
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because skills-rae/go.mod uses replace directives like ../../core
# that point at sibling sub-repos. --platform=$BUILDPLATFORM pins this
# stage to the workstation's native arch (amd64); GOARCH=$TARGETARCH
# cross-compiles to whatever the runtime image will be.
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
WORKDIR /src
COPY go.work go.work.sum* ./
COPY core ./core
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/skills-rae ./knowledge/skills-rae/cmd/skills-rae
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/skills-rae-worker ./knowledge/skills-rae/cmd/skills-rae-worker

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/skills-rae /usr/local/bin/skills-rae
COPY --from=build /out/skills-rae-worker /usr/local/bin/skills-rae-worker
ENTRYPOINT ["/usr/local/bin/skills-rae"]
