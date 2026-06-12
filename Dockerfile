# Build the git-bridge binary
FROM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

RUN apk add --no-cache git

# Copy the Go Modules manifests
COPY go.mod go.sum ./

# Cache deps in a dedicated layer with mount cache for faster rebuilds
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the go source
COPY cmd/ cmd/
COPY internal/ internal/

# Build with cache mounts for Go build cache and module cache
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w \
      -X git-bridge/internal/version.Version=${VERSION} \
      -X git-bridge/internal/version.GitCommit=${GIT_COMMIT} \
      -X git-bridge/internal/version.BuildDate=${BUILD_DATE}" \
    -o git-bridge ./cmd/git-bridge

# ---
# Runtime image (alpine — git CLI is required for mirroring)
FROM alpine:3.24
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

# OCI image labels
LABEL org.opencontainers.image.title="git-bridge" \
      org.opencontainers.image.description="Multi-provider, bidirectional Git repository mirroring service" \
      org.opencontainers.image.url="https://github.com/somaz94/git-bridge" \
      org.opencontainers.image.source="https://github.com/somaz94/git-bridge" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

RUN apk add --no-cache git ca-certificates tzdata

RUN adduser -D -u 1000 appuser
WORKDIR /app

COPY --from=builder /workspace/git-bridge /app/git-bridge

RUN mkdir -p /tmp/git-bridge && chown appuser:appuser /tmp/git-bridge
USER appuser

EXPOSE 8080

ENTRYPOINT ["/app/git-bridge"]
