# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git make gcc musl-dev

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make build

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1000 aperod && adduser -D -u 1000 -G aperod aperod

WORKDIR /app
COPY --from=builder /build/build/aperod-node /usr/local/bin/aperod-node
COPY --from=builder /build/build/aperod      /usr/local/bin/aperod
COPY --from=builder /build/config/           /app/config/

RUN chown -R aperod:aperod /app
USER aperod

# P2P port
EXPOSE 30303/tcp
# RPC port (bind to 127.0.0.1 in production, expose via nginx)
EXPOSE 8545/tcp

VOLUME ["/app/data"]

ENTRYPOINT ["/usr/local/bin/aperod-node"]
CMD ["--config", "/app/config/testnet.yaml"]
