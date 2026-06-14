# syntax=docker/dockerfile:1.6
# ---------- build ----------
FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache module downloads on a separate layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
        -o /out/control-plane ./cmd/control-plane

# ---------- runtime ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
WORKDIR /app
COPY --from=build /out/control-plane /app/control-plane
EXPOSE 8080
USER nobody
ENTRYPOINT ["/app/control-plane"]
