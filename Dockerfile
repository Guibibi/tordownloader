# Multi-stage Docker build for tordownloader.
# Stage 1: compile a static Go binary (no cgo).
# Stage 2: minimal alpine runtime with su-exec for PUID/PGID support.

# ── Build ────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags="-w -s -X main.version=${VERSION}" \
  -o tordownloader \
  ./cmd/tordownloader

# ── Runtime ──────────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache \
  ca-certificates \
  tzdata \
  su-exec

COPY --from=builder /build/tordownloader /usr/local/bin/tordownloader
COPY scripts/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

RUN mkdir -p /data /downloads

ENV TORBOX_API_KEY=""
ENV PUID=99
ENV PGID=100

VOLUME ["/data", "/downloads"]
EXPOSE 6500

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- --timeout=3 http://localhost:6500/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["tordownloader"]
