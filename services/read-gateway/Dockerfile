# syntax=docker/dockerfile:1.6
#
# Multi-stage build for the read-gateway.
#
# Pure-Go (no CGo): no Kafka client, no bundled librdkafka. CGO_ENABLED=0
# lets us use debian:bookworm-slim at runtime without any C runtime deps.
#
# Build context must be the repo root so COPY can reach lib/jwks.
# image build sets context to the repo root.
# For a standalone build run from the repo root:
#
#   docker build -t read-gateway:dev \
#       -f services/read-gateway/Dockerfile .

# ---------- build ----------
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Build context is the repo root (needed for the COPY lib/* below).
# The Go source lives under services/read-gateway/ within that context.
# go.mod has `replace github.com/portfolio-management/jwks => ../../lib/jwks`
# so we must copy lib/jwks at the path the replace directive resolves to.
COPY services/read-gateway/go.mod services/read-gateway/go.sum ./
# Copy the local lib dependency so `go mod download` + build can resolve the
# `replace github.com/portfolio-management/jwks => ../../lib/jwks` directive.
# dsl now comes from github.com/opencapital-dev/oc-plugin-sdk (fetched via go
# modules), not a local copy.
COPY lib/jwks /lib/jwks
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY services/read-gateway/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
        -o /out/read-gateway ./cmd/read-gateway

# ---------- runtime ----------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        wget \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/read-gateway /app/read-gateway

EXPOSE 8095
USER nobody
ENTRYPOINT ["/app/read-gateway"]
