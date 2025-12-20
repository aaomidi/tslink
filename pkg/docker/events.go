package docker

import (
	"context"
	"strings"
	"sync"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/aaomidi/tslink/pkg/core"
	"github.com/aaomidi/tslink/pkg/logger"
)

// ContainerCache caches container info for quick lookup during Join().
type ContainerCache struct {
	mu sync.RWMutex
	// Map EndpointID -> ContainerInfo
	byEndpoint map[string]*core.ContainerInfo
}

// NewContainerCache creates a new container cache.
func NewContainerCache() *ContainerCache {
	return &ContainerCache{
		byEndpoint: make(map[string]*core.ContainerInfo),
	}
}

// Store stores container info keyed by endpoint ID.
func (c *ContainerCache) Store(endpointID string, info *core.ContainerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byEndpoint[endpointID] = info
	logger.Info(
		"ContainerCache: stored info for endpoint %s (hostname=%s service=%s endpoints=%d direct=%v)",
		endpointID[:12],
		info.Hostname,
		info.Service,
		len(info.Endpoints),
		info.Direct,
	)
}

// GetByEndpoint retrieves container info by endpoint ID.
func (c *ContainerCache) GetByEndpoint(endpointID string) (*core.ContainerInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.byEndpoint[endpointID]
	return info, ok
}

// Delete removes container info by endpoint ID.
func (c *ContainerCache) Delete(endpointID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byEndpoint, endpointID)
}

// ContainerInfoCallback is called when container info is stored in the cache.
// The driver uses this to trigger Tailscale setup.
type ContainerInfoCallback func(endpointID string, info *core.ContainerInfo)

// WatchEvents watches Docker events and caches container info.
// When container info is stored, the callback is invoked to trigger Tailscale setup.
// The context controls the lifecycle - when cancelled, the watcher stops.
func WatchEvents(ctx context.Context, cache *ContainerCache, networkDriverName string, onInfo ContainerInfoCallback) {
	logger.Info("Starting Docker event watcher for network driver: %s", networkDriverName)

	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		logger.Error("Failed to create Docker client for event watching: %v", err)
		return
	}
	defer cli.Close()

	logger.Debug("Docker client created, starting event stream")

	// Watch for network events
	filterArgs := dockerclient.Filters{}.
		Add("type", "network").
		Add("event", "connect")

	result := cli.Events(ctx, dockerclient.EventsListOptions{
		Filters: filterArgs,
	})

	logger.Debug("Event stream started, waiting for events...")

	for {
		select {
		case <-ctx.Done():
			logger.Info("Event watcher shutting down")
			return

		case err := <-result.Err:
			if err != nil {
				// Check if context is cancelled before reconnecting
				if ctx.Err() != nil {
					logger.Info("Event watcher context cancelled, stopping")
					return
				}

				logger.Warn("Event stream error: %v, reconnecting in 1s...", err)

				// Backoff before reconnecting to avoid tight loop
				select {
				case <-ctx.Done():
					logger.Info("Event watcher context cancelled during backoff")
					return
				case <-time.After(1 * time.Second):
				}

				// Reconnect after backoff
				result = cli.Events(ctx, dockerclient.EventsListOptions{
					Filters: filterArgs,
				})
			}

		case msg := <-result.Messages:
			// Log all network events for debugging
			networkType := msg.Actor.Attributes["type"]
			logger.Debug("Event received: type=%s driver=%s (want=%s)", msg.Action, networkType, networkDriverName)

			// Only process events for our network driver
			if networkType != networkDriverName {
				continue
			}

			containerID := msg.Actor.Attributes["container"]
			networkName := msg.Actor.Attributes["name"]

			if containerID == "" {
				continue
			}

			logger.Info("Event: network connect - container=%s network=%s", containerID[:12], networkName)

			// Inspect container to get labels and endpoint ID
			// This is safe - we're not in a callback
			info, err := cli.ContainerInspect(ctx, containerID, dockerclient.ContainerInspectOptions{})
			if err != nil {
				logger.Error("Failed to inspect container %s: %v", containerID[:12], err)
				continue
			}

			// Find the endpoint ID for this network
			if info.Container.NetworkSettings == nil || info.Container.NetworkSettings.Networks == nil {
				logger.Warn("Container %s has no network settings", containerID[:12])
				continue
			}

			netSettings, ok := info.Container.NetworkSettings.Networks[networkName]
			if !ok {
				logger.Warn("Container %s not found in network %s", containerID[:12], networkName)
				continue
			}

			endpointID := netSettings.EndpointID
			if endpointID == "" {
				logger.Warn("Container %s has no endpoint ID for network %s", containerID[:12], networkName)
				continue
			}

			logger.Debug("Container %s has endpoint ID %s", containerID[:12], endpointID[:12])

			// Parse container info
			name := strings.TrimPrefix(info.Container.Name, "/")

			labels := make(map[string]string)
			if info.Container.Config != nil && info.Container.Config.Labels != nil {
				labels = info.Container.Config.Labels
			}

			containerInfo := parseContainerInfo(name, labels)

			// Store in cache
			cache.Store(endpointID, containerInfo)

			// Trigger callback to start Tailscale setup
			if onInfo != nil {
				onInfo(endpointID, containerInfo)
			}
		}
	}
}

// parseContainerInfo extracts ts.* labels into structured ContainerInfo.
func parseContainerInfo(name string, labels map[string]string) *core.ContainerInfo {
	info := &core.ContainerInfo{
		Name:   name,
		Labels: labels,
		Direct: true, // Default: direct serve enabled
	}

	// ts.hostname - override Tailscale hostname
	if v, ok := labels["ts.hostname"]; ok && v != "" {
		info.Hostname = v
	} else {
		info.Hostname = name
	}

	// ts.tags - comma-separated ACL tags (e.g., "tag:web,tag:prod")
	if v, ok := labels["ts.tags"]; ok && v != "" {
		tags := strings.Split(v, ",")
		for i, t := range tags {
			tags[i] = strings.TrimSpace(t)
		}
		info.Tags = tags
	}

	// ts.service - service name (e.g., "svc:hello-world")
	if v, ok := labels["ts.service"]; ok && v != "" {
		info.Service = v
	}

	// ts.direct - enable/disable direct machine serve (default: true)
	// Set ts.direct=false to disable direct serve (only use service backend)
	if v, ok := labels["ts.direct"]; ok {
		info.Direct = v != "false" && v != "0" && v != "no"
	}

	// Parse serve endpoints from ts.serve.<port> labels
	info.Endpoints = parseServeEndpoints(labels)

	return info
}

// parseServeEndpoints parses ts.serve.<port> labels into ServeEndpoint structs.
// Format: ts.serve.<external-port> = <proto>:<target>[/<path>]
// Examples:
//   - ts.serve.443=https:8080       → HTTPS on 443 → localhost:8080
//   - ts.serve.80=http:3000/api     → HTTP on 80 → localhost:3000 at /api
//   - ts.serve.5432=tcp             → TCP on 5432 → localhost:5432 (same port)
func parseServeEndpoints(labels map[string]string) []core.ServeEndpoint {
	var endpoints []core.ServeEndpoint

	for key, value := range labels {
		if !strings.HasPrefix(key, "ts.serve.") {
			continue
		}

		// Extract external port from key
		externalPort := strings.TrimPrefix(key, "ts.serve.")
		if externalPort == "" {
			continue
		}

		endpoint := parseServeValue(externalPort, value)
		if endpoint != nil {
			endpoints = append(endpoints, *endpoint)
		}
	}

	return endpoints
}

// parseServeValue parses a serve label value.
// Format: <proto>[:<target>][/<path>]
// Examples:
//   - "https:8080"      → proto=https, target=8080
//   - "http:3000/api"   → proto=http, target=3000, path=/api
//   - "tcp"             → proto=tcp, target=<same as external port>
//   - "tcp:5432"        → proto=tcp, target=5432
func parseServeValue(externalPort, value string) *core.ServeEndpoint {
	if value == "" {
		return nil
	}

	endpoint := &core.ServeEndpoint{
		Port:   externalPort,
		Target: externalPort, // Default: same as external
	}

	// Split by colon to get proto and target
	parts := strings.SplitN(value, ":", 2)
	endpoint.Proto = strings.ToLower(parts[0])

	// Validate protocol
	switch endpoint.Proto {
	case "http", "https", "tcp", "tls-terminated-tcp", "tun":
		// Valid
	default:
		logger.Debug("parseServeValue: unknown protocol %q for port %s", endpoint.Proto, externalPort)
		return nil
	}

	if len(parts) > 1 {
		targetAndPath := parts[1]

		// Check for path (L7 only)
		if idx := strings.Index(targetAndPath, "/"); idx != -1 {
			endpoint.Target = targetAndPath[:idx]
			endpoint.Path = targetAndPath[idx:] // Keep the leading /
		} else {
			endpoint.Target = targetAndPath
		}
	}

	// Default target to external port if empty
	if endpoint.Target == "" {
		endpoint.Target = externalPort
	}

	return endpoint
}
