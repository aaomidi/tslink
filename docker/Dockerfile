# Build stage
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tslink ./cmd/tslink

# Final stage
FROM alpine:3.23@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62

RUN apk add --no-cache \
    iptables \
    ip6tables \
    iptables-legacy \
    iproute2 \
    util-linux \
    ca-certificates

# Use iptables-nft to match modern Docker (which uses iptables-nft on Ubuntu 24.04+).
# This ensures our FORWARD/MASQUERADE rules go into the same tables Docker uses.
# Tailscale bundles its own iptables runner that handles its own rules.
RUN ln -sf /sbin/iptables-nft /sbin/iptables && \
    ln -sf /sbin/ip6tables-nft /sbin/ip6tables

# Create plugin directories
RUN mkdir -p /run/docker/plugins /data /data/tailscale-bin

# Copy plugin binary only - Tailscale binaries are downloaded at runtime
COPY --from=builder /tslink /tslink

ENTRYPOINT ["/tslink"]
