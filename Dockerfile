# syntax=docker/dockerfile:1.7
# Multi-arch build: pass --platform linux/amd64,linux/arm64 to buildx.
# BuildKit cross-compiles Go via TARGETOS/TARGETARCH; we stay on a native
# builder image (--platform=$BUILDPLATFORM) for speed and let `go build`
# emit the target binary.

ARG GO_VERSION=1.25.6
ARG ALPINE_VERSION=3.21

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${DATE}" \
      -o /out/coremcp ./cmd/coremcp


FROM alpine:${ALPINE_VERSION}

ARG VERSION=dev

LABEL org.opencontainers.image.title="CoreMCP" \
      org.opencontainers.image.description="MCP server for your databases — wire MSSQL, Firebird, PostgreSQL into Claude, Cursor, and any MCP client." \
      org.opencontainers.image.source="https://github.com/corebasehq/coremcp" \
      org.opencontainers.image.url="https://corebasehq.com" \
      org.opencontainers.image.documentation="https://docs.corebasehq.com" \
      org.opencontainers.image.vendor="CoreBase" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 coremcp && \
    adduser -D -u 1000 -G coremcp coremcp

WORKDIR /app

COPY --from=builder /out/coremcp /usr/local/bin/coremcp
COPY coremcp.example.yaml /app/coremcp.example.yaml

USER coremcp

ENTRYPOINT ["coremcp"]
CMD ["serve", "--config", "/app/coremcp.yaml"]
