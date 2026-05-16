FROM alpine:3.21

RUN apk --no-cache add ca-certificates

# Static aarch64 binary cross-compiled with
#   zig build -Dtarget=aarch64-linux-musl -Doptimize=ReleaseSmall
# Run `make pi-arxiv-skip` from the repo root to produce it.
COPY arxiv-skip /usr/local/bin/arxiv-skip

EXPOSE 9871
ENTRYPOINT ["/usr/local/bin/arxiv-skip"]
