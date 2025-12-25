# Spec: Container Label Caching via Docker Events

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

Docker's Network Plugin API does not provide access to container labels during network setup. The plugin solves this by watching Docker Events API to pre-cache container labels before `Join()` is called.

## Problem Statement

When Docker calls the plugin's `Join()` method, the request only contains:

- `NetworkID` - the network identifier
- `EndpointID` - a random hash
- `SandboxKey` - path to the network namespace

**Container name, labels, and other metadata are NOT provided.**

Additionally, querying the Docker API during `Join()` causes a deadlock because Docker daemon is blocked waiting for the plugin to return.

## Solution

Watch Docker Events API for `network connect` events, which fire BEFORE `Join()` is called. Cache container labels by endpoint ID so they're available instantly when `Join()` runs.

### Event Sequence

```
Docker Timeline:
────────────────────────────────────────────────────────────────
container create  →  network connect event  →  Join() called  →  container start
                           ↓
                     Plugin caches labels
                     by endpoint ID
                           ↓
                     Join() reads from cache
                     immediately
```

## Behavior

### Cache Lookup

When `Join()` is called:

1. Check cache for container info by endpoint ID
2. If found → use cached hostname, tags, service config immediately
3. If not found → wait briefly for event to arrive, then proceed with defaults

### What Gets Cached

| Field | Source | Default if Missing |
|-------|--------|-------------------|
| Hostname | `tslink.hostname` label or container name | Endpoint ID prefix |
| Tags | `tslink.tags` label | None |
| Service | `tslink.service` label | None |
| Endpoints | `tslink.serve.*` labels | None |

### Cache Lifetime

- Entries are created when `network connect` event fires
- Entries are removed when container leaves network (`Leave()`)
- Cache is in-memory only (cleared on plugin restart)

## Benefits

| Aspect | Without Caching | With Caching |
|--------|-----------------|--------------|
| Label availability | Not possible (deadlock) | Instant |
| Hostname | Wrong (uses endpoint ID) | Correct from start |
| Service config | Cannot configure | Applied during startup |

## Error Handling

| Condition | Behavior |
|-----------|----------|
| Event watcher connection lost | Automatic reconnection |
| Cache miss in Join() | Brief wait, then use defaults |
| Container stops before cache hit | Entry cleaned up on Leave() |

## Debugging

```bash
# View event processing logs
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  cat /data/plugin.log | grep -E "(Event|Cache)"
```

Example output:
```
level=INFO msg="Event: network connect - container=f223584394d8 network=tailnet"
level=INFO msg="ContainerCache: stored info for endpoint fe9bafd47085 (hostname=my-host)"
level=INFO msg="Join: using cached container info: hostname=my-host"
```

## Security Considerations

- Cache is per-plugin instance, not shared
- Labels are read from Docker API (trusted source)
- No label data persisted to disk
