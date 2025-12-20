# Spec: Tailscale Binary Management

> **Status:** Implemented
> **Last Updated:** 2025-12-22

## Overview

The plugin downloads Tailscale binaries at runtime rather than bundling them. This keeps the plugin image small (~10MB vs ~50MB) and allows users to pin versions or receive automatic updates.

## Goals

- Keep the plugin image small by not bundling binaries
- Allow users to pin a specific Tailscale version for stability
- Allow users to use their own Tailscale binaries (air-gapped environments)
- Cache downloads to avoid repeated network requests

## Non-Goals

- Automatic updates while containers are running (requires container restart)
- Managing multiple Tailscale versions simultaneously
- Building Tailscale from source

## Configuration

| Option | Default | Description |
|--------|---------|-------------|
| `TS_VERSION` | `latest` | Tailscale version to use. Either `latest` or a specific version like `1.76.6` |
| `TS_PATH` | _(empty)_ | Path to user-provided binaries. If set, `TS_VERSION` is ignored |

### Examples

```bash
# Use latest stable (default)
docker plugin install ghcr.io/aaomidi/tslink:latest

# Pin to specific version
docker plugin set ghcr.io/aaomidi/tslink TS_VERSION=1.76.6

# Use custom binaries (air-gapped)
docker plugin set ghcr.io/aaomidi/tslink TS_PATH=/opt/tailscale/bin
```

## Behavior

### Resolution Priority

When binaries are needed:

1. If `TS_PATH` is set → use binaries from that path
2. Otherwise → download/use cached version based on `TS_VERSION`

### Download Timing

Binaries are downloaded **lazily on first container join**, not at plugin enable time.

- Plugin enable always succeeds (no network dependency at startup)
- First container start may be slightly slower (~2-5 seconds for download)
- Subsequent containers use cached binaries instantly

### Caching

Downloaded binaries are cached in the plugin's data directory and persist across:

- Container restarts
- Plugin restarts
- Host reboots

The cache is keyed by version number. Multiple versions can coexist.

### Version Resolution

When `TS_VERSION=latest`:

- The latest stable version is fetched from Tailscale's package API
- The resolved version is cached for the plugin's lifetime
- To pick up a newer "latest", restart the plugin or clear the cache

### Concurrency

If multiple containers start simultaneously, only one download occurs. Other containers wait for the download to complete and then use the cached result.

## Error Handling

| Condition | User Sees | Recovery |
|-----------|-----------|----------|
| Network unreachable | `failed to download Tailscale: network unreachable` | Check network, retry container start |
| Version not found | `Tailscale version 1.99.99 not found` | Use valid version from pkgs.tailscale.com |
| `TS_PATH` binaries missing | `tailscale binary not found at /path` | Verify path and binary permissions |
| Disk full | `failed to extract Tailscale: no space left` | Free disk space |
| Corrupt download | Automatic retry once, then fail | Clear cache directory, retry |

## Security Considerations

- **HTTPS only** - All downloads use TLS with certificate verification
- **Official source** - Downloads only from `pkgs.tailscale.com`
- **Binary permissions** - Downloaded binaries are root-owned, mode 0755
- **No signature verification** _(yet)_ - Relies on TLS for integrity

## Future Enhancements

1. **Checksum verification** - Verify SHA256 of downloaded tarballs against published checksums
2. **Cache cleanup** - Automatically remove old versions when disk space is low
3. **Pre-download option** - Setting to download at plugin enable rather than first use
4. **Unstable track** - Support `TS_VERSION=unstable` for beta versions

