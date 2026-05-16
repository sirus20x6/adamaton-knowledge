# Mirrors dashboard/Dockerfile: cross-compile on host, bake a tiny
# Alpine runtime image. The binary is statically linked (CGO_ENABLED=0)
# so the runtime needs nothing beyond ca-certificates + tzdata.
#
# Build with the wrapper script:
#   ./skills/scripts/deploy.sh
# Or manually (run from the repo root):
#   make pi-skills-worker
#   docker buildx build --platform linux/arm64 \
#     -t evo-skills-worker:dev --output type=docker \
#     -f skills/Dockerfile bin/

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY skills-worker.arm64 /usr/local/bin/skills-worker
ENTRYPOINT ["/usr/local/bin/skills-worker"]
