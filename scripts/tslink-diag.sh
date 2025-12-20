#!/bin/bash
# tslink-diag.sh - Run diagnostics on tslink Docker network plugin
#
# Usage:
#   ./tslink-diag.sh           # Run diagnostics
#   ./tslink-diag.sh --json    # Output as JSON
#   ./tslink-diag.sh --watch   # Watch mode (refresh every 5s)

set -e

DATA_DIR="/var/lib/docker-plugins/tailscale"
PLUGIN_IMAGE="ghcr.io/aaomidi/tslink:latest"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_header() {
    echo -e "${GREEN}=== tslink Diagnostics ===${NC}"
    echo "Time: $(date)"
    echo ""
}

check_plugin_status() {
    echo -e "${YELLOW}Plugin Status:${NC}"
    if docker plugin ls 2>/dev/null | grep -q "tslink"; then
        ENABLED=$(docker plugin ls --format '{{.Enabled}}' --filter name=tslink 2>/dev/null || echo "unknown")
        if [ "$ENABLED" = "true" ]; then
            echo -e "  Plugin: ${GREEN}enabled${NC}"
        else
            echo -e "  Plugin: ${RED}disabled${NC}"
        fi
    else
        echo -e "  Plugin: ${RED}not installed${NC}"
        return 1
    fi
    echo ""
}

check_networks() {
    echo -e "${YELLOW}Networks using tslink:${NC}"
    NETWORKS=$(docker network ls --filter driver=ghcr.io/aaomidi/tslink:latest --format '{{.Name}}' 2>/dev/null || true)
    if [ -z "$NETWORKS" ]; then
        echo "  (none)"
    else
        echo "$NETWORKS" | while read net; do
            CONTAINERS=$(docker network inspect "$net" --format '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null || true)
            echo "  $net: ${CONTAINERS:-no containers}"
        done
    fi
    echo ""
}

run_diag() {
    echo -e "${YELLOW}Endpoint Status:${NC}"
    # Run the diag command inside the plugin's data directory
    docker run --rm \
        -v "$DATA_DIR:/data:ro" \
        alpine sh -c '
            echo ""
            for dir in /data/*/; do
                [ -d "$dir" ] || continue
                name=$(basename "$dir")

                # Skip cache directories
                [ "$name" = "tailscale-bin" ] && continue

                id="${name:0:12}"
                socket_exists="no"
                state_exists="no"

                [ -S "${dir}tailscaled.sock" ] && socket_exists="yes"
                [ -f "${dir}tailscaled.state" ] && state_exists="yes"

                echo "  Endpoint: $id"
                echo "    Socket: $socket_exists"
                echo "    State:  $state_exists"

                if [ -f "${dir}debug.log" ]; then
                    echo "    Debug log:"
                    cat "${dir}debug.log" | sed "s/^/      /"
                fi
                echo ""
            done

            if [ ! -d "/data" ] || [ -z "$(ls -A /data 2>/dev/null)" ]; then
                echo "  No endpoints found"
            fi
        '
}

show_recent_logs() {
    echo -e "${YELLOW}Recent Plugin Logs:${NC}"
    # Try to get Docker daemon logs (works on Linux)
    if command -v journalctl &> /dev/null; then
        journalctl -u docker --since "5 minutes ago" 2>/dev/null | \
            grep -E "tailscale|tslink|Endpoint|Join|Leave" | \
            tail -20 | sed 's/^/  /' || echo "  (could not read logs)"
    else
        echo "  (journalctl not available - check Docker Desktop logs)"
    fi
    echo ""
}

# Main
case "${1:-}" in
    --watch)
        while true; do
            clear
            print_header
            check_plugin_status
            check_networks
            run_diag
            echo "Refreshing in 5s... (Ctrl+C to exit)"
            sleep 5
        done
        ;;
    --json)
        docker run --rm \
            -v "$DATA_DIR:/data:ro" \
            alpine sh -c '
                echo "{"
                echo "  \"timestamp\": \"$(date -Iseconds)\","
                echo "  \"endpoints\": ["
                first=true
                for dir in /data/*/; do
                    [ -d "$dir" ] || continue
                    name=$(basename "$dir")
                    [ "$name" = "tailscale-bin" ] && continue

                    [ "$first" = "false" ] && echo ","
                    first=false

                    socket_exists="false"
                    state_exists="false"
                    [ -S "${dir}tailscaled.sock" ] && socket_exists="true"
                    [ -f "${dir}tailscaled.state" ] && state_exists="true"

                    echo "    {"
                    echo "      \"id\": \"$name\","
                    echo "      \"socket_exists\": $socket_exists,"
                    echo "      \"state_exists\": $state_exists"
                    echo -n "    }"
                done
                echo ""
                echo "  ]"
                echo "}"
            '
        ;;
    *)
        print_header
        check_plugin_status || exit 1
        check_networks
        run_diag
        ;;
esac
