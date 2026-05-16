# Mirror of skills/Dockerfile. Built from a host cross-compile, run
# inside the Pi's deepresearch docker-compose stack alongside temporal,
# postgres, backend, evo-api.
#
# Build with `make pi-reindex-worker` (cross-compiles to bin/) then:
#   docker buildx build --platform linux/arm64 \
#     -t evo-reindex-worker:dev --output type=docker \
#     -f reindex/Dockerfile bin/

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY reindex-worker.arm64 /usr/local/bin/reindex-worker
ENTRYPOINT ["/usr/local/bin/reindex-worker"]
