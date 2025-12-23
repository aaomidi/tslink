# Examples

## docker-compose.yml

Basic example: a single web server with HTTPS via Tailscale Serve.

```bash
export TS_AUTHKEY=tskey-auth-xxx
docker-compose up -d
```

Access at `https://web-server.<your-tailnet>.ts.net`

## docker-compose-services.yml

Load-balanced backends using [Tailscale Services](https://tailscale.com/kb/1438/services).

**Setup:**
1. Create a service named `my-app` in the [Tailscale admin console](https://login.tailscale.com/admin/services)
2. Use a tag-based auth key (required for services)

```bash
export TS_AUTHKEY=tskey-auth-xxx
docker-compose -f docker-compose-services.yml up -d

# Scale to multiple backends
docker-compose -f docker-compose-services.yml up -d --scale backend=3
```

Access via the TailVIP shown in the admin console.
