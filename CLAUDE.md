# Development Guide

## Quick Start

```bash
echo "TS_AUTHKEY=tskey-auth-xxx" > .env
make reinstall
make test-network test-container
```

## Prerequisites

- Go 1.25+
- Docker (via OrbStack, Docker Desktop, or native Linux)
- Tailscale auth key from https://login.tailscale.com/admin/settings/keys

## Development Cycle

```bash
# Edit code in pkg/ or cmd/, then:
make reinstall

# Test
source .env
docker network create --driver ghcr.io/aaomidi/tslink:latest --opt ts.authkey=$TS_AUTHKEY tailnet
docker run --rm --network tailnet alpine sh -c "ip addr && ping -c 2 8.8.8.8"
docker network rm tailnet
```

## Key Design Decisions

**Hostname-based state directories**: Tailscale state is stored in `/data/by-hostname/<hostname>/` not by endpoint ID. This enables identity reuse - if a container restarts with the same name, it keeps its Tailscale identity and IP.

**Async Tailscale setup**: Docker's `Join()` must return quickly, but Tailscale auth can take 60+ seconds. Solution:
1. `Join()` sets up veth networking and returns immediately
2. Docker event watcher detects container start, gets container name
3. Tailscale setup runs async in background goroutine

**Veth IP allocation**: Each container gets a unique /30 subnet from 10.200.0.0/16, derived by hashing the endpoint ID. This avoids IP conflicts without coordination.

## Concurrency Notes

**Lock ordering**: Never hold `driver.mu` when calling endpoint methods (they acquire `endpoint.mu`). Always: driver.mu → endpoint.mu, never reversed.

**Long operations outside locks**: Network syscalls, Tailscale binary downloads, and `tailscale up` can block for seconds. Don't hold locks during these.

## Debugging

```bash
# View endpoint debug logs
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  sh -c 'for d in /data/*/; do echo "=== $d ==="; cat "$d/debug.log" 2>/dev/null | tail -20; done'

# Plugin logs (Linux)
journalctl -u docker -f | grep -i tailscale

# Plugin logs (macOS/OrbStack)
docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
# Then: journalctl -u docker -f
```

## Project Structure

```
pkg/
├── docker/     # Docker network driver (driver.go, events.go)
├── core/       # Endpoint/network logic (endpoint.go, network.go)
├── tailscale/  # Daemon lifecycle (daemon.go, supervisor.go, binary.go)
├── netutil/    # Linux networking (veth.go - veth, routing, NAT)
└── logger/     # Structured logging
```

**Key paths at runtime:**
- State: `/data/by-hostname/<hostname>/tailscaled.state`
- Socket: `/data/by-hostname/<hostname>/tailscaled.sock`
- Debug: `/data/by-hostname/<hostname>/debug.log`

## Code Style

### Linting

```bash
golangci-lint run        # Check for issues
golangci-lint run --fix  # Auto-fix where possible
golangci-lint fmt        # Format code
```

Config is in `.golangci.toml`. Key linters enabled:
- `errcheck`, `errorlint`, `nilerr` - error handling
- `gosec` - security
- `govet`, `staticcheck` - correctness
- `modernize` - Go 1.22+ idioms

### Error Handling

**Always handle errors explicitly** - never ignore silently:

```go
// GOOD - log cleanup errors
if err := cleanup(); err != nil {
    logger.Warn("cleanup failed: %v", err)
}

// BAD - silent ignore
cleanup()
_ = cleanup()
```

**Use `errors.Is`/`errors.As`** for sentinel errors:

```go
// GOOD
if errors.Is(err, io.EOF) { ... }
if errors.As(err, &netlink.LinkNotFoundError{}) { ... }

// BAD - breaks with wrapped errors
if err == io.EOF { ... }
```

**Wrap errors with context** using `%w`:

```go
return fmt.Errorf("failed to create endpoint: %w", err)
```

## Troubleshooting

### Plugin won't enable

Check that the state directory exists:
```bash
docker run --rm --privileged -v /var/lib:/var/lib alpine \
  mkdir -p /var/lib/docker-plugins/tailscale
```

### Options not being passed

Docker passes options with the full key. Debug by adding logging:
```go
log.Printf("Options: %+v", req.Options)
```

### Container networking issues

Check that tailscaled is running in the container's netns:
```bash
# From inside container
ps aux | grep tailscale
ip addr
```
