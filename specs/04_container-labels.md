# Spec: Container Labels Schema

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

Container labels configure Tailscale behavior for individual containers. Labels control the hostname, ACL tags, service registration, and serve endpoints.

## Goals

- Provide a consistent, intuitive label schema for all Tailscale configuration
- Support multiple serve endpoints per container (L3, L4, L7)
- Enable path-based routing for HTTP/HTTPS endpoints

## Non-Goals

- Network-level configuration (handled via `--opt` flags)
- Runtime label updates (labels are read at container start)

## Configuration

### Label Reference

| Label | Required | Format | Description |
|-------|----------|--------|-------------|
| `ts.hostname` | No | string | Override Tailscale hostname (default: container name) |
| `ts.tags` | No | `tag:name,tag:name` | Comma-separated ACL tags |
| `ts.service` | No | `svc:name` | Service name to register as backend |
| `ts.serve.<port>` | If service set | `<proto>:<target>[/<path>]` | Serve endpoint configuration |

### Serve Endpoint Format

```
ts.serve.<external-port> = <protocol>:<target-port>[/<path>]
```

**Components:**

| Component | Required | Description |
|-----------|----------|-------------|
| `<external-port>` | Yes | Port Tailscale exposes (e.g., `443`, `80`, `5432`) |
| `<protocol>` | Yes | One of: `http`, `https`, `tcp`, `tls-terminated-tcp`, `tun` |
| `<target-port>` | No | Container port to forward to (default: same as external) |
| `/<path>` | No | L7 only - Path prefix for routing (e.g., `/api`) |

### Protocol Reference

| Protocol | Layer | TLS | Use Case | Access Pattern |
|----------|-------|-----|----------|----------------|
| `http` | L7 | No | HTTP web servers | `http://service.tailnet.ts.net/` |
| `https` | L7 | Yes | HTTPS web servers | `https://service.tailnet.ts.net/` |
| `tcp` | L4 | No | Databases, Redis, raw TCP | `tcp://service:port` |
| `tls-terminated-tcp` | L4 | Yes | Databases with TLS | `tcp://service:port` (TLS terminated) |
| `tun` | L3 | No | IP-level forwarding | Direct IP access |

### Examples

#### Simple Web Server (HTTP)

```yaml
labels:
  ts.hostname: "my-web"
  ts.service: "svc:web"
  ts.serve.80: "http:8080"
```

Accessible at: `http://web.tailnet.ts.net/`

#### HTTPS Web Server

```yaml
labels:
  ts.hostname: "secure-web"
  ts.service: "svc:web"
  ts.serve.443: "https:80"
```

Accessible at: `https://web.tailnet.ts.net/`

Tailscale terminates TLS and reverse-proxies HTTP to container port 80.

#### Multiple Endpoints

```yaml
labels:
  ts.service: "svc:api"
  ts.serve.443: "https:8080"      # Main API
  ts.serve.9090: "http:9090"      # Metrics
```

#### Path-Based Routing (L7)

```yaml
labels:
  ts.service: "svc:monolith"
  ts.serve.443: "https:3000/api"      # /api/* -> container:3000
  ts.serve.8080: "http:4000/admin"    # /admin/* -> container:4000
```

#### Database (TCP)

```yaml
labels:
  ts.service: "svc:database"
  ts.serve.5432: "tcp:5432"
```

#### Database with TLS

```yaml
labels:
  ts.service: "svc:secure-db"
  ts.serve.5432: "tls-terminated-tcp:5432"
```

Tailscale terminates TLS; traffic to container is plain TCP.

#### Full Configuration

```yaml
services:
  backend:
    image: nginx:alpine
    networks:
      - tailnet
    labels:
      ts.hostname: "backend-1"
      ts.tags: "tag:web,tag:prod"
      ts.service: "svc:hello-world"
      ts.serve.443: "https:80"
```

## Behavior

### Label Processing Order

1. Container joins network
2. Docker Events API fires `network connect` event
3. Plugin inspects container to read labels
4. Labels are cached by endpoint ID
5. On `Join()`, cached labels are applied before starting tailscaled
6. If cache miss, labels are applied asynchronously after startup

### Service Configuration Flow

1. Container starts and joins network
2. Plugin reads `ts.*` labels
3. Tailscale connects with configured hostname and tags
4. Each `ts.serve.<port>` endpoint is registered with the service

### Protocol Selection

Choose protocol based on your service type:

| Service Type | Recommended Protocol |
|--------------|---------------------|
| Web server (nginx, apache) | `https` for production, `http` for dev |
| REST API | `https` |
| gRPC | `tls-terminated-tcp` or `tcp` |
| PostgreSQL/MySQL | `tcp` or `tls-terminated-tcp` |
| Redis | `tcp` |
| Custom TCP service | `tcp` |

### Common Mistakes

| Mistake | Problem | Fix |
|---------|---------|-----|
| Using `tls-terminated-tcp` for web | Returns raw TCP, not HTTP | Use `https` instead |
| Missing `ts.service` with `ts.serve.*` | Endpoints won't be configured | Add `ts.service` label |
| Using path with TCP protocol | Paths only work with L7 | Remove path or use `http`/`https` |

## Error Handling

| Condition | Error Message | Recovery |
|-----------|--------------|----------|
| `ts.serve.*` without `ts.service` | `ts.service requires at least one ts.serve.<port> endpoint` | Add ts.service label |
| Invalid protocol | `unsupported protocol: xyz` | Use http, https, tcp, tls-terminated-tcp, or tun |
| Invalid port | `invalid external port "abc": must be numeric` | Use numeric port |
| Path with L4 protocol | Path is silently ignored | Use http/https for path routing |

## Security Considerations

- **Tags propagate to Tailscale ACLs** - Only set tags you intend the container to have
- **HTTPS recommended for production** - Use `https` protocol for web services
- **Service pre-creation required** - Services must exist in Tailscale admin console

## Future Enhancements

1. **Wildcard paths** - Support `ts.serve.443: "https:80/*"` for catch-all routing
2. **Header injection** - Add custom headers via labels
3. **Health check paths** - Configure health endpoints for service backends
4. **Hot reload** - Support label changes without container restart

