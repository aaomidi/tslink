# Spec: Lifecycle Recovery

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

The plugin automatically recovers container Tailscale connectivity after host reboot or plugin restart. Docker does NOT call `Join()` for existing containers after daemon restart, so the plugin must actively scan and restore orphaned endpoints.

## Goals

- Containers automatically regain Tailscale connectivity after plugin restart
- Containers keep same Tailscale IP after host reboot (state reuse)
- No manual intervention required
- Recovery completes within 10-30 seconds

## Non-Goals

- Zero-downtime plugin upgrades (Tailscale daemons must restart)
- Recovering containers that were stopped before plugin restart
- Preserving in-flight TCP connections across restarts

## Background: The Docker Problem

When Docker daemon restarts, it does NOT call the plugin's `Join()` method for already-running containers. Instead Docker reads its internal database and expects the plugin to already have state. This is a known limitation affecting all Docker network plugins.

**Result without recovery:** Plugin has no endpoints in memory, containers have no Tailscale connectivity.

## Solution

The plugin uses three recovery mechanisms:

1. **Event watching** - Handles new containers joining the network
2. **Initial recovery** - Scans Docker on startup for orphaned endpoints
3. **Periodic reconciliation** - Catches containers that restart later

When an orphaned endpoint is found, the plugin restores it by reading container labels, recreating internal state, and restarting Tailscale using preserved state files.

## State Reuse

Tailscale state is stored by hostname in `/data/by-hostname/<hostname>/`. When a container with the same hostname restarts:

- Same Tailscale node identity is reused
- Same Tailscale IP is assigned
- No new device appears in admin console

This enables containers to maintain stable Tailscale IPs across restarts.

## What Gets Preserved

| Item | Preserved | Notes |
|------|-----------|-------|
| Tailscale IP | Yes | Same hostname = same state = same IP |
| Node identity | Yes | Stored in state directory |
| ACL tags | Yes | Re-applied from container labels |
| Service registration | Yes | Re-configured on startup |

## What Gets Disrupted

- Tailscale connectivity down for 10-30 seconds during plugin restart
- TCP connections drop and must be re-established
- UDP/QUIC connections drop

## Error Handling

| Condition | Behavior |
|-----------|----------|
| Container has no sandbox key | Skip (container not fully started) |
| Network auth key missing | Skip with error log |
| Tailscale startup fails | Log error, continue with other endpoints |
| Docker API unreachable | Log error, retry on next cycle |

Recovery is best-effort: individual endpoint failures don't affect other endpoints.

## Debugging

```bash
# View recovery logs
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  cat /data/plugin.log | grep -i "recover\|orphan"
```

Example output after plugin restart:
```
level=INFO msg="RecoverEndpoints: found orphaned endpoint 77a4ecf2b6a3 for container d6636f344239"
level=INFO msg="RecoverEndpoints: recovered 2 orphaned endpoint(s)"
level=INFO msg="Endpoint 77a4ecf2b6a3 got Tailscale IP: 100.119.179.75 (hostname=test-client)"
```

## Security Considerations

- **Auth key from config**: Recovery uses `TS_AUTHKEY` from plugin config
- **State directory permissions**: `/data/` is root-owned, mode 0700
- **No credential storage**: Auth keys are not persisted to disk

## Future Enhancements

1. **Faster recovery** - Reduce initial delay
2. **Recovery metrics** - Expose recovery count via diagnostics
3. **Graceful drain** - Run `tailscale serve drain` before plugin disable
