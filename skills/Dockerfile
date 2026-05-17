# skills-worker multi-stage Dockerfile.
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because skills/go.mod uses replace directives like ../../core that
# point at sibling sub-repos. --platform=$BUILDPLATFORM pins this stage
# to the workstation's native arch (amd64); GOARCH=$TARGETARCH cross-
# compiles to whatever the runtime image will be.
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
    -o /out/skills-worker ./knowledge/skills/cmd/skills-worker

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=build /out/skills-worker /usr/local/bin/skills-worker
ENTRYPOINT ["/usr/local/bin/skills-worker"]
