# Spec: Networking Setup

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

Each container gets a veth pair connecting it to the host, with NAT for internet access. This provides the network path for tailscaled to reach Tailscale's coordination servers and DERP relays. The Tailscale IP (100.x.x.x) is separate from the veth IP (10.200.x.x).

## Goals

- Provide containers with internet access for Tailscale connectivity
- Isolate container networks from each other (no direct L2 connectivity)
- Avoid IP conflicts between containers
- Clean up networking resources on container stop

## Non-Goals

- Direct container-to-container communication via veth (use Tailscale for this)
- IPv6 support for veth pairs (Tailscale handles IPv6 via tunnel)
- Custom MTU configuration

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                           HOST                                       │
│                                                                      │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────┐         │
│  │ Container A  │     │ Container B  │     │ Container C  │         │
│  │              │     │              │     │              │         │
│  │ 10.200.0.2   │     │ 10.200.0.6   │     │ 10.200.0.10  │         │
│  │ (+ 100.x.x.x)│     │ (+ 100.x.x.x)│     │ (+ 100.x.x.x)│         │
│  └──────┬───────┘     └──────┬───────┘     └──────┬───────┘         │
│         │ veth               │ veth               │ veth            │
│         │                    │                    │                  │
│  ┌──────┴───────┐     ┌──────┴───────┐     ┌──────┴───────┐         │
│  │ 10.200.0.1   │     │ 10.200.0.5   │     │ 10.200.0.9   │         │
│  └──────────────┘     └──────────────┘     └──────────────┘         │
│                                                                      │
│                    NAT (MASQUERADE) via iptables                    │
│                              │                                       │
│                              ▼                                       │
│                    ┌─────────────────┐                              │
│                    │  Internet       │                              │
│                    └─────────────────┘                              │
└─────────────────────────────────────────────────────────────────────┘
```

## Address Space

| Range | Purpose |
|-------|---------|
| `10.200.0.0/16` | Veth pair addresses (host ↔ container) |
| `100.x.x.x/32` | Tailscale addresses (assigned by Tailscale) |

## IP Allocation

Each endpoint receives a unique `/30` subnet from the `10.200.0.0/16` range:

- Host side: First usable IP (e.g., 10.200.X.1)
- Container side: Second usable IP (e.g., 10.200.X.2)

IPs are deterministically assigned based on endpoint ID to avoid conflicts. This supports ~16,000 concurrent containers.

## Setup Flow

When a container joins the network:

1. A veth pair is created connecting container to host
2. Container end is moved into the container's network namespace
3. Unique IPs are assigned to both ends
4. Default route through host veth is configured in container
5. NAT (MASQUERADE) is configured for internet access

## Cleanup Flow

When a container leaves:

- NAT rules are removed
- Veth pair is destroyed (automatically removes both ends and routes)

## Error Handling

| Condition | Behavior |
|-----------|----------|
| Veth creation fails | Container start fails with error |
| Namespace not found | Error returned (container likely stopped) |
| NAT setup fails | Warning logged, continue (DERP relay still works) |
| iptables rule exists | No error (idempotent) |

## Debugging

```bash
# View veth pairs on host
ip link show type veth

# View NAT rules
iptables -t nat -L POSTROUTING -v | grep 10.200

# Check routes
ip route show | grep 10.200

# View interfaces inside container
docker exec <container> ip addr
docker exec <container> ip route
```

## Security Considerations

- **No direct L2 connectivity** - Containers cannot see each other's traffic on veth
- **NAT isolation** - Outbound only, no inbound from internet
- **Tailscale for inter-container** - All container-to-container traffic goes through Tailscale (encrypted)

## Future Enhancements

1. **IPv6 veth support** - Dual-stack veth addressing
2. **Custom MTU** - Configurable MTU for veth pairs
3. **Traffic shaping** - Rate limiting per container
