# syntax=docker/dockerfile:1.6
#
# Multi-stage build for the v6 gateway.
#
# Uses confluent-kafka-go's bundled librdkafka (no build tag = bundled
# build). Debian bookworm's librdkafka-dev package is 1.9, well below the
# 2.14+ that confluent-kafka-go v2.14.1 requires; the bundled build keeps
# the Dockerfile self-contained without pulling Confluent's APT repo.
#
# Build context must be the repo root so COPY can reach lib/jwks and
# lib/datakey. image build sets context to the repo root.
# For a standalone build run from the repo root:
#
#   docker build -t gateway:dev -f services/gateway/Dockerfile .

# ---------- build ----------
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Build deps for CGo + the bundled librdkafka's compression / SSL / SASL libs.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        zlib1g-dev \
        libssl-dev \
        libsasl2-dev \
        ca-certificates \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Build context is the repo root. The Go source lives under services/gateway/
# within that context. go.mod has `replace ... => ../../lib/X` directives, so we
# must copy each replaced lib at the path the directive resolves to.
COPY services/gateway/go.mod services/gateway/go.sum ./
# Copy the local lib dependencies so `go mod download` + build can resolve the
# `replace ... => ../../lib/X` directives (jwks, datakey).
COPY lib/jwks /lib/jwks
COPY lib/datakey /lib/datakey
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY services/gateway/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags "-s -w" \
        -o /out/gateway ./cmd/gateway

# ---------- runtime ----------
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libsasl2-2 \
        wget \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
EXPOSE 8090
USER nobody
ENTRYPOINT ["/app/gateway"]
