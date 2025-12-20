# Spec: Supervisor Resilience

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

Each container's tailscaled process is managed by a supervisor that handles automatic restart on failure, exponential backoff, and crash loop detection. This ensures containers maintain Tailscale connectivity even when tailscaled crashes.

## Goals

- Automatically restart tailscaled if it crashes
- Prevent resource exhaustion from rapid restart loops
- Detect persistent failures and stop retrying
- Avoid thundering herd on host reboot

## Non-Goals

- Restarting the container itself (Docker handles this)
- Health checks beyond process liveness
- Custom restart policies per container

## Behavior

### Automatic Restart

When tailscaled exits unexpectedly, the supervisor automatically restarts it. Each container has its own supervisor instance.

### Exponential Backoff

Restart delays grow exponentially to prevent resource exhaustion:

- Starts at 100ms, doubles each attempt
- Caps at 30 seconds maximum
- Resets after 30 seconds of stable operation

This prevents tight restart loops while still recovering quickly from transient failures.

### Crash Loop Detection

If tailscaled crashes 5 times within 30 seconds, the supervisor stops retrying and enters a failed state. This prevents infinite loops when there's a persistent problem.

To recover from crash loop state, restart the container.

### Startup Jitter

On initial startup, each supervisor waits a random 0-500ms before starting tailscaled. This spreads out container startups after host reboot, reducing:

- Load on Tailscale control plane
- Burst network traffic
- CPU spikes

### Health Checking

The supervisor checks if tailscaled is still running every 5 seconds. If the process has exited, restart logic is triggered.

## Error Handling

| Condition | Behavior |
|-----------|----------|
| Daemon exits cleanly | Restart with backoff |
| Daemon crashes | Record crash, restart with backoff |
| 5 crashes in 30 seconds | Stop retrying (crash loop) |
| Socket timeout on startup | Treat as crash, restart |
| `tailscale up` fails | Treat as crash, restart |
| Supervisor stopped | No restart (intentional shutdown) |

## Debugging

```bash
# View supervisor logs
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  cat /data/plugin.log | grep -E "(restart|crash|backoff)"
```

Example output:
```
level=INFO msg="tailscaled running (restart count: 1)"
level=WARN msg="tailscaled exited, restarting in 200ms (attempt 2)"
level=INFO msg="tailscaled running (restart count: 2)"
level=ERROR msg="Crash loop detected: 5 crashes in 30s, giving up"
```

### Diagnostics

```bash
# Check endpoint status
docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
  --entrypoint /tslink \
  ghcr.io/aaomidi/tslink:latest diag
```

## Security Considerations

- **Resource limits** - Backoff prevents CPU exhaustion from restart loops
- **Crash loop protection** - Gives up after threshold to prevent infinite loops
- **No privilege escalation** - Supervisor runs as same user as plugin

## Future Enhancements

1. **Configurable thresholds** - Allow users to adjust crash loop parameters
2. **Crash reason logging** - Capture tailscaled exit reason for diagnostics
3. **Metrics export** - Expose restart count via diagnostics
4. **Graceful drain** - Run `tailscale logout` before intentional stops
