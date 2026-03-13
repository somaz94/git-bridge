FROM golang:1.26-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /git-bridge ./cmd/git-bridge

# ---

FROM alpine:3.23

RUN apk add --no-cache git ca-certificates tzdata

RUN adduser -D -u 1000 appuser
WORKDIR /app

COPY --from=builder /git-bridge /app/git-bridge

RUN mkdir -p /tmp/git-bridge && chown appuser:appuser /tmp/git-bridge
USER appuser

EXPOSE 8080

ENTRYPOINT ["/app/git-bridge"]
