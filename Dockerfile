# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26

# ---- deps cache ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS base
WORKDIR /src
RUN apk add --no-cache git ca-certificates tzdata
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# ---- build ----
FROM base AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- runtime ----
# A API em distroless (sem shell, non-root). O ai-worker tem imagem própria (Dockerfile.ai-worker).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/api /app/api
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app/api"]
