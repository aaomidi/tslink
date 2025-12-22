# Tailscale Container Network Plugin

A Docker network plugin that gives each container its own Tailscale identity. Containers appear as individual nodes in your tailnet with their own Tailscale IPs.

## How It Works

When you create a Docker network with this plugin and run containers on it:

1. Each container gets its own Tailscale node identity
2. The container appears in your Tailscale admin console as a separate device
3. The container can reach other nodes in your tailnet
4. The container also has internet access via NAT

```
┌──────────────────────────────────────────────────────────┐
│ Docker Host                                              │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │ Container (Tailscale IP: 100.x.y.z)                │  │
│  │                                                    │  │
│  │   App ──► tailscaled ──► Tailnet                   │  │
│  │                                                    │  │
│  │   eth0 ──► veth ──► NAT ──► Internet               │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
│  ┌──────────────────────┐                                │
│  │ Plugin               │                                │
│  │ • Manages tailscaled │                                │
│  │ • Creates veth pairs │                                │
│  │ • Sets up NAT        │                                │
│  └──────────────────────┘                                │
└──────────────────────────────────────────────────────────┘
```

## Installation

### Prerequisites

- Docker 19.03+ or OrbStack
- A Tailscale auth key from https://login.tailscale.com/admin/settings/keys

### Install the Plugin

```bash
# Create the data directory (required)
sudo mkdir -p /var/lib/docker-plugins/tailscale
```

Docker plugins require architecture-specific tags:

```bash
# For amd64 (Intel/AMD, most cloud VMs)
docker plugin install ghcr.io/aaomidi/tslink:latest-amd64

# For arm64 (Apple Silicon, AWS Graviton, Raspberry Pi)
docker plugin install ghcr.io/aaomidi/tslink:latest-arm64

# Or install a specific version
docker plugin install ghcr.io/aaomidi/tslink:v0.0.1-amd64

# Or follow main branch (latest development)
docker plugin install ghcr.io/aaomidi/tslink:main-amd64

# The plugin will be enabled automatically
docker plugin ls
```

## Usage

### Create a Network

```bash
# With auth key in command (ephemeral nodes by default)
# Replace :latest-amd64 with :latest-arm64 for ARM systems
docker network create \
  --driver ghcr.io/aaomidi/tslink:latest-amd64 \
  --opt ts.authkey=tskey-auth-xxxxx \
  my-tailnet

# Or set the auth key globally when installing the plugin
docker plugin set ghcr.io/aaomidi/tslink:latest-amd64 TS_AUTHKEY=tskey-auth-xxxxx
docker network create --driver ghcr.io/aaomidi/tslink:latest-amd64 my-tailnet
```

### Run Containers

```bash
# Run a container - it automatically gets a Tailscale IP
docker run --rm --network my-tailnet alpine sh

# Inside the container:
# - Check your Tailscale IP in the Tailscale admin console
# - Ping other tailnet nodes
# - Access the internet normally
```

### Example: Web Server on Tailnet

```bash
# Run nginx, accessible from your tailnet
docker run -d --name web --network my-tailnet nginx

# From any device on your tailnet, access via the node's Tailscale hostname
curl http://web.your-tailnet.ts.net
```

### Example: Docker Compose

```yaml
# Use :latest-amd64 or :latest-arm64 depending on your system
networks:
  tailnet:
    driver: ghcr.io/aaomidi/tslink:latest-amd64
    driver_opts:
      ts.authkey: ${TS_AUTHKEY}

services:
  api:
    image: my-api
    networks:
      - tailnet

  worker:
    image: my-worker
    networks:
      - tailnet
```

## Configuration

### Network Options

| Option | Description | Default |
|--------|-------------|---------|
| `ts.authkey` | Tailscale auth key | Required (or set via plugin env) |

### Container Labels

Configure per-container Tailscale settings using labels:

| Label | Description | Example |
|-------|-------------|---------|
| `ts.hostname` | Tailscale hostname | `ts.hostname=my-api` |
| `ts.tags` | ACL tags (comma-separated) | `ts.tags=tag:server,tag:prod` |
| `ts.serve.<port>` | Expose port via Tailscale Serve | `ts.serve.443=https:8080` |
| `ts.service` | Register as Tailscale Service backend | `ts.service=svc:my-api` |
| `ts.direct` | Enable direct machine serve | `ts.direct=true` (default) |

```bash
# Example: Container with custom hostname and tags
docker run -d --network my-tailnet \
  --label ts.hostname=my-api \
  --label ts.tags=tag:server \
  nginx

# Example: Expose HTTPS on port 443, forwarding to container port 8080
docker run -d --network my-tailnet \
  --label ts.hostname=web \
  --label ts.serve.443=https:8080 \
  my-web-app
```

### Setting Default Auth Key

Instead of passing the auth key with each network, set it as a plugin environment variable:

```bash
# Set default auth key (use :latest-arm64 for ARM systems)
docker plugin disable ghcr.io/aaomidi/tslink:latest-amd64
docker plugin set ghcr.io/aaomidi/tslink:latest-amd64 TS_AUTHKEY=tskey-auth-xxxxx
docker plugin enable ghcr.io/aaomidi/tslink:latest-amd64

# Now create networks without specifying the auth key
docker network create --driver ghcr.io/aaomidi/tslink:latest-amd64 my-tailnet
```

## Auth Key Types

| Key Type | Behavior |
|----------|----------|
| **Ephemeral key** | Nodes are automatically removed when the container stops |
| **Reusable key** | Nodes persist in your tailnet after container stops |
| **Pre-approved key** | Nodes don't require manual approval |

For most use cases, use an ephemeral, reusable, pre-approved key.

## Cleanup

```bash
# Remove network (stops all containers using it)
docker network rm my-tailnet

# Ephemeral nodes are automatically removed from Tailscale
# Non-ephemeral nodes remain in your admin console and need manual removal
```

## Troubleshooting

### View Debug Logs

Each container's Tailscale daemon writes logs to the host:

```bash
# List all endpoint logs
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  sh -c 'for d in /data/by-hostname/*/; do echo "=== $d ==="; cat "$d/debug.log" 2>/dev/null | tail -20; done'

# Check plugin logs (Linux)
journalctl -u docker -f | grep -i tailscale

# Check plugin logs (macOS/OrbStack)
docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
# Then: journalctl -u docker -f
```

### Container Won't Start

1. Check debug logs (above) for errors
2. Verify auth key is valid and not expired
3. Ensure the plugin is enabled: `docker plugin ls`

### Node Appears "Offline" in Tailscale Console

- Auth key may be expired - create a new one
- Container may have stopped - check `docker ps`
- Network issues - check debug logs for connection errors

### Internet Works but Can't Reach Tailnet

Check debug logs for:
- `Switching ipn state Starting -> Running` = connected successfully
- `Switching ipn state NeedsLogin` = auth key issue
- `network is unreachable` = veth setup failed

### Identity Reuse

Containers with the same hostname reuse the same Tailscale identity. If you need a fresh identity:

```bash
# Clear state for a specific hostname
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  rm -rf /data/by-hostname/<hostname>
```

## Known Limitations

- **macOS/Windows**: Only works with Docker in a Linux VM (OrbStack, Docker Desktop)
- **MagicDNS in container**: Containers can reach tailnet by IP; MagicDNS resolution requires additional DNS config

## Development

See [CLAUDE.md](CLAUDE.md) for development setup.

## License

MIT
