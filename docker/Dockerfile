# Build stage
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tslink ./cmd/tslink

# Final stage
FROM alpine:3.20

RUN apk add --no-cache \
    iptables \
    ip6tables \
    iptables-legacy \
    iproute2 \
    util-linux \
    ca-certificates

# Use iptables-legacy for tailscaled compatibility (nft doesn't support --set-mark)
RUN ln -sf /sbin/iptables-legacy /sbin/iptables && \
    ln -sf /sbin/ip6tables-legacy /sbin/ip6tables

# Create plugin directories
RUN mkdir -p /run/docker/plugins /data /data/tailscale-bin

# Copy plugin binary only - Tailscale binaries are downloaded at runtime
COPY --from=builder /tslink /tslink

ENTRYPOINT ["/tslink"]
