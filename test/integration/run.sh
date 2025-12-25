#!/usr/bin/env bash
# Integration test runner for tslink Docker network plugin
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Configuration
export PLUGIN_NAME="${PLUGIN_NAME:-ghcr.io/aaomidi/tslink}"
export PLUGIN_TAG="${PLUGIN_TAG:-test}"
export LOG_DIR="${LOG_DIR:-$REPO_ROOT/test-logs}"

# Generate random hostnames to avoid conflicts
RANDOM_SUFFIX=$(head -c 4 /dev/urandom | xxd -p)
export SERVER_HOSTNAME="tslink-srv-${RANDOM_SUFFIX}"
export CLIENT_HOSTNAME="tslink-cli-${RANDOM_SUFFIX}"
export STATIC_SERVICE="tslink-test.atlas-diminished.ts.net"
export TAILNET_SUFFIX="atlas-diminished.ts.net"
echo "Using hostnames: server=$SERVER_HOSTNAME client=$CLIENT_HOSTNAME"
echo "Static service: $STATIC_SERVICE"

# Validate required environment
if [ -z "${TS_AUTHKEY:-}" ]; then
    echo "ERROR: TS_AUTHKEY environment variable is required"
    exit 1
fi

# Export logs for artifact upload
export_logs() {
    echo "=== Exporting logs to $LOG_DIR ==="
    mkdir -p "$LOG_DIR"

    # Plugin logs
    sudo cp -r /var/lib/docker-plugins/tailscale/*.log "$LOG_DIR/" 2>/dev/null || true
    sudo find /var/lib/docker-plugins/tailscale -name "debug.log" -exec cp {} "$LOG_DIR/" \; 2>/dev/null || true

    # Plugin state (for debugging)
    docker plugin inspect "$PLUGIN_NAME:$PLUGIN_TAG" > "$LOG_DIR/plugin-inspect.json" 2>&1 || true

    # Container logs
    docker compose -f "$SCRIPT_DIR/docker-compose.yml" logs > "$LOG_DIR/compose.log" 2>&1 || true

    # Fix permissions so CI can read them
    sudo chown -R "$(id -u):$(id -g)" "$LOG_DIR" 2>/dev/null || true

    echo "Logs exported to $LOG_DIR"
    ls -la "$LOG_DIR" || true
}

# Cleanup function
cleanup() {
    local exit_code=$?

    # Always export logs
    export_logs

    echo "=== Cleaning up ==="
    docker compose -f "$SCRIPT_DIR/docker-compose.yml" down -v 2>/dev/null || true
    docker plugin disable "$PLUGIN_NAME:$PLUGIN_TAG" -f 2>/dev/null || true
    docker plugin rm "$PLUGIN_NAME:$PLUGIN_TAG" -f 2>/dev/null || true

    exit $exit_code
}
trap cleanup EXIT

# Build and install plugin
echo "=== Building plugin ==="
cd "$REPO_ROOT"
make docker-rootfs
docker plugin create "$PLUGIN_NAME:$PLUGIN_TAG" docker/

# Ensure state directory exists (required for plugin mounts)
echo "=== Creating state directory ==="
sudo mkdir -p /var/lib/docker-plugins/tailscale

# Ensure netns directory exists (primes Docker to create /var/run/docker/netns)
docker run --rm alpine echo "Priming netns directory..."

# Enable plugin
docker plugin enable "$PLUGIN_NAME:$PLUGIN_TAG"

# Run tests via docker compose (V2)
echo "=== Running integration tests ==="
docker compose -f "$SCRIPT_DIR/docker-compose.yml" up \
    --build \
    --abort-on-container-exit \
    --exit-code-from client

echo "=== Integration tests passed! ==="
