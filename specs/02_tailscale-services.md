# Spec: Tailscale Services

> **Status:** Implemented
> **Last Updated:** 2025-12-22
> **See Also:** [04_container-labels.md](04_container-labels.md) for complete label schema

## Overview

Containers can register as backends for Tailscale Services, enabling load-balanced access via TailVIP. Services are defined in the Tailscale admin console; containers declare which service they belong to via Docker labels.

## Goals

- Allow containers to register as service backends using container labels
- Support custom hostnames and ACL tags via labels
- Fail fast if service configuration is invalid or service doesn't exist

## Non-Goals

- Auto-creating services (must be pre-defined in Tailscale admin console)
- Service health check configuration (uses Tailscale defaults)
- UDP services (Tailscale Services is TCP-only currently)

## Configuration

> **Note:** See [04_container-labels.md](04_container-labels.md) for the complete label schema.

Container labels:

| Label | Required | Description |
|-------|----------|-------------|
| `ts.hostname` | No | Override Tailscale hostname (default: container name) |
| `ts.tags` | No | Comma-separated ACL tags (e.g., `tag:web,tag:prod`) |
| `ts.service` | No | Service name to join (e.g., `svc:hello-world`) |
| `ts.serve.<port>` | If `ts.service` set | Serve endpoint: `<proto>:<target>[/<path>]` |

### Examples

```bash
# Simple container with custom hostname
docker run --network tailnet \
  --label ts.hostname=my-web-server \
  nginx:alpine

# Container as HTTPS service backend
docker run --network tailnet \
  --label ts.service=svc:hello-world \
  --label ts.serve.443=https:80 \
  nginx:alpine

# Full configuration with tags
docker run --network tailnet \
  --label ts.hostname=web-1 \
  --label ts.tags=tag:web-service,tag:prod \
  --label ts.service=svc:hello-world \
  --label ts.serve.443=https:80 \
  nginx:alpine
```

```yaml
# docker-compose.yml
services:
  web:
    image: nginx:alpine
    networks:
      - tailnet
    labels:
      ts.service: "svc:hello-world"
      ts.serve.443: "https:80"
    deploy:
      replicas: 3  # All replicas become service backends
```

## Behavior

### Label Reading

Labels are read from the container when it joins the network. The plugin queries the Docker API using the container's sandbox key to retrieve labels.

### Service Registration Flow

1. Container joins network with service labels
2. Plugin validates configuration (service requires at least one serve endpoint)
3. Plugin checks Tailscale version >= 1.86.0
4. Container registers as service backend with specified endpoints
5. If service doesn't exist in admin console, container join fails

### Version Requirements

Tailscale Services requires version 1.86.0 or later. The plugin only enforces this check when service labels are present. Containers without service labels work with any Tailscale version.

### Tag-Based Authentication

Tailscale Services requires tag-based authentication. The auth key used for the network must include appropriate tags. Tags specified in `ts.tags` are passed to `tailscale up --advertise-tags`.

## Error Handling

| Condition | Error Message | Recovery |
|-----------|--------------|----------|
| `ts.service` without `ts.serve.*` | `ts.service requires at least one ts.serve.<port> endpoint` | Add ts.serve.<port> label |
| Invalid port format | `invalid external port "abc": must be numeric` | Use numeric port |
| Invalid protocol | `unsupported protocol: xyz` | Use http, https, tcp, tls-terminated-tcp, or tun |
| Tailscale < 1.86.0 | `Tailscale 1.76.0 does not support Services (minimum: 1.86.0)` | Set TS_VERSION=1.86.0 or later |
| Service doesn't exist | `service svc:foo not found: create it in Tailscale admin console first` | Create service in admin console |
| Missing tags | `tailscale serve failed: requires tagged auth key` | Use auth key with tags |

## Security Considerations

- **Tags are opt-in** - Only containers with `ts.tags` label advertise tags
- **Service pre-creation** - Services must exist in admin console, preventing accidental service creation
- **Auth key scoping** - Recommend using auth keys scoped to specific tags

## Future Enhancements

1. **Service health endpoint** - Configure custom health check endpoint via label
2. **Graceful drain** - Run `tailscale serve drain` before container stops
3. **Service auto-creation** - Option to create services via Tailscale API

