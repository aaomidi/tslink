package docker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/aaomidi/tslink/pkg/core"
	"github.com/aaomidi/tslink/pkg/logger"
)

// Driver implements the Docker network plugin interface.
type Driver struct {
	mu        sync.RWMutex
	networks  map[string]*core.Network
	endpoints map[string]*core.Endpoint
	config    *core.Config
	cache     *ContainerCache // Pre-cached container info from Docker events

	// Docker client for container inspection (used for recovery)
	docker *dockerclient.Client

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NetworkDriverName is the name used to identify our network driver.
const NetworkDriverName = "ghcr.io/aaomidi/tslink:latest"

// watchdogInterval is the interval at which the watchdog checks for orphaned endpoints.
const watchdogInterval = 60 * time.Second

// NewDriver creates a new Docker network driver.
func NewDriver() (*Driver, error) {
	cfg, err := core.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Create Docker client for container inspection and recovery
	docker, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	cache := NewContainerCache()
	ctx, cancel := context.WithCancel(context.Background())

	d := &Driver{
		networks:  make(map[string]*core.Network),
		endpoints: make(map[string]*core.Endpoint),
		config:    cfg,
		cache:     cache,
		docker:    docker,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Start event watcher in background (tracked by waitgroup)
	// Pass callback to trigger Tailscale setup when container info arrives
	d.wg.Go(func() {
		WatchEvents(ctx, cache, NetworkDriverName, d.onContainerInfo)
	})

	// Recover orphaned endpoints from previous plugin instance (host reboot, plugin restart)
	// Run in background so plugin starts accepting requests immediately
	d.wg.Go(func() {
		// Small delay to let Docker daemon finish startup
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if err := d.RecoverEndpoints(ctx); err != nil {
			logger.Error("Initial endpoint recovery failed: %v", err)
		}
	})

	// Start watchdog for continuous reconciliation
	d.wg.Go(func() {
		d.runWatchdog(ctx)
	})

	return d, nil
}

// onContainerInfo is called by the event watcher when container info is stored.
// It triggers Tailscale setup for the endpoint with the correct hostname.
func (d *Driver) onContainerInfo(endpointID string, info *core.ContainerInfo) {
	// NOTE: We must not hold driver.mu while calling endpoint methods.
	// Lock ordering: never hold driver.mu when acquiring endpoint.mu to avoid deadlock.

	// Get endpoint reference under lock
	d.mu.RLock()
	endpoint, ok := d.endpoints[endpointID]
	d.mu.RUnlock()

	if !ok {
		logger.Debug("onContainerInfo: endpoint %s not found (may have already left)", endpointID[:12])
		return
	}

	// Call endpoint methods without holding driver.mu
	if endpoint.IsTailscaleStarted() {
		logger.Debug("onContainerInfo: Tailscale already started for endpoint %s", endpointID[:12])
		return
	}

	if endpoint.GetSandboxKey() == "" {
		logger.Debug("onContainerInfo: endpoint %s has no sandbox key (Join not called yet)", endpointID[:12])
		return
	}

	logger.Info(
		"onContainerInfo: triggering Tailscale setup for endpoint %s (hostname=%s)",
		endpointID[:12],
		info.Hostname,
	)

	// Start Tailscale in a goroutine so we don't block the event handler
	d.wg.Go(func() {
		if err := endpoint.StartTailscale(info); err != nil {
			logger.Error("onContainerInfo: failed to start Tailscale for endpoint %s: %v", endpointID[:12], err)
		}
	})
}

// GetCapabilities returns the capabilities of the driver.
func (d *Driver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	logger.Info("GetCapabilities called")
	return &network.CapabilitiesResponse{
		Scope:             "local",
		ConnectivityScope: "global", // Containers can reach the tailnet
	}, nil
}

// CreateNetwork creates a new network.
func (d *Driver) CreateNetwork(req *network.CreateNetworkRequest) error {
	logger.Info("CreateNetwork: %s", req.NetworkID)
	logger.Info("CreateNetwork raw: %+v", req)

	// Debug: log all options to understand Docker's format
	for k, v := range req.Options {
		logger.Info("  Option: %q = %v (type: %T)", k, v, v)
		// Check if value is a nested map
		if nested, ok := v.(map[string]any); ok {
			for nk, nv := range nested {
				logger.Info("    Nested: %q = %v", nk, nv)
			}
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	opts := core.ParseNetworkOptions(req.Options)
	logger.Info("Parsed opts: authkey=%q", opts.AuthKey)

	// Merge auth key from options or use default from config
	authKey := opts.AuthKey
	if authKey == "" {
		authKey = d.config.AuthKey
		logger.Info("Using config authkey: %q", authKey)
	}
	if authKey == "" {
		return fmt.Errorf("no Tailscale auth key provided: set TS_AUTHKEY env var or use --opt ts.authkey=xxx")
	}

	net := &core.Network{
		ID:      req.NetworkID,
		AuthKey: authKey,
	}

	d.networks[req.NetworkID] = net
	logger.Info("Created network %s", req.NetworkID)

	return nil
}

// AllocateNetwork is called during network creation (for multi-host networks).
func (d *Driver) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	logger.Info("AllocateNetwork: %s", req.NetworkID)
	return &network.AllocateNetworkResponse{}, nil
}

// DeleteNetwork deletes a network.
func (d *Driver) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	logger.Info("DeleteNetwork: %s", req.NetworkID)

	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.networks, req.NetworkID)
	return nil
}

// FreeNetwork is called during network deletion (for multi-host networks).
func (d *Driver) FreeNetwork(req *network.FreeNetworkRequest) error {
	logger.Info("FreeNetwork: %s", req.NetworkID)
	return nil
}

// CreateEndpoint creates a new endpoint for a container.
func (d *Driver) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	logger.Info("CreateEndpoint: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	d.mu.Lock()
	defer d.mu.Unlock()

	net, ok := d.networks[req.NetworkID]
	if !ok {
		return nil, fmt.Errorf("network %s not found", req.NetworkID)
	}

	// Parse endpoint options for hostname override
	opts := core.ParseEndpointOptions(req.Options)

	endpoint, err := core.NewEndpoint(req.EndpointID, net, opts, d.config)
	if err != nil {
		return nil, fmt.Errorf("failed to create endpoint: %w", err)
	}

	d.endpoints[req.EndpointID] = endpoint

	// The interface will be configured during Join
	return &network.CreateEndpointResponse{}, nil
}

// DeleteEndpoint deletes an endpoint.
func (d *Driver) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	logger.Info("DeleteEndpoint: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	d.mu.Lock()
	defer d.mu.Unlock()

	// Clean up cache entry
	d.cache.Delete(req.EndpointID)

	endpoint, ok := d.endpoints[req.EndpointID]
	if !ok {
		logger.Info("Endpoint %s not found, ignoring", req.EndpointID)
		return nil
	}

	if err := endpoint.Stop(); err != nil {
		logger.Info("Warning: failed to stop endpoint: %v", err)
	}

	delete(d.endpoints, req.EndpointID)
	return nil
}

// EndpointInfo returns information about an endpoint.
func (d *Driver) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	logger.Info("EndpointInfo: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	d.mu.RLock()
	defer d.mu.RUnlock()

	endpoint, ok := d.endpoints[req.EndpointID]
	if !ok {
		return nil, fmt.Errorf("endpoint %s not found", req.EndpointID)
	}

	// Use safe getter to avoid race with StartTailscale
	tailscaleIP, hostname := endpoint.GetInfo()

	return &network.InfoResponse{
		Value: map[string]string{
			"tailscale_ip": tailscaleIP,
			"hostname":     hostname,
		},
	}, nil
}

// Join is called when a container joins the network.
// It sets up basic networking and returns quickly.
// Tailscale setup is triggered asynchronously by the Docker event handler.
func (d *Driver) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	logger.Info("Join: network=%s endpoint=%s sandbox=%s", req.NetworkID, req.EndpointID, req.SandboxKey)

	// Get endpoint reference under lock, then release before calling endpoint methods
	d.mu.RLock()
	endpoint, ok := d.endpoints[req.EndpointID]
	d.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("endpoint %s not found", req.EndpointID)
	}

	// Set up basic networking (veth, IPs, routing, NAT)
	// This returns quickly - Tailscale setup is deferred until container info arrives via event
	// NOTE: driver.mu is NOT held here to avoid lock ordering issues with endpoint.mu
	joinResp, err := endpoint.Join(req.SandboxKey)
	if err != nil {
		return nil, fmt.Errorf("failed to join: %w", err)
	}

	// Check if container info is already in cache (rare but possible)
	// cache has its own internal lock, so this is safe
	if info, ok := d.cache.GetByEndpoint(req.EndpointID); ok {
		logger.Info("Join: container info already cached, triggering immediate Tailscale setup")
		d.wg.Go(func() {
			if err := endpoint.StartTailscale(info); err != nil {
				logger.Error("Join: failed to start Tailscale: %v", err)
			}
		})
	} else {
		logger.Info("Join: waiting for Docker event to trigger Tailscale setup for endpoint %s", req.EndpointID[:12])
	}

	return joinResp, nil
}

// Leave is called when a container leaves the network.
func (d *Driver) Leave(req *network.LeaveRequest) error {
	logger.Info("Leave: network=%s endpoint=%s", req.NetworkID, req.EndpointID)

	d.mu.Lock()
	defer d.mu.Unlock()

	endpoint, ok := d.endpoints[req.EndpointID]
	if !ok {
		logger.Info("Endpoint %s not found, ignoring", req.EndpointID)
		return nil
	}

	if err := endpoint.Leave(); err != nil {
		logger.Info("Warning: failed to leave: %v", err)
	}

	return nil
}

// DiscoverNew is called when a new node is discovered.
func (d *Driver) DiscoverNew(req *network.DiscoveryNotification) error {
	logger.Info("DiscoverNew: type=%d", req.DiscoveryType)
	return nil
}

// DiscoverDelete is called when a node is removed.
func (d *Driver) DiscoverDelete(req *network.DiscoveryNotification) error {
	logger.Info("DiscoverDelete: type=%d", req.DiscoveryType)
	return nil
}

// ProgramExternalConnectivity is called to program external connectivity.
func (d *Driver) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	logger.Info("ProgramExternalConnectivity: network=%s endpoint=%s", req.NetworkID, req.EndpointID)
	return nil
}

// RevokeExternalConnectivity is called to revoke external connectivity.
func (d *Driver) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	logger.Info("RevokeExternalConnectivity: network=%s endpoint=%s", req.NetworkID, req.EndpointID)
	return nil
}

// Shutdown gracefully stops the driver and all managed resources.
// It cancels the context (stopping the event watcher and any pending operations),
// stops all endpoints, and waits for goroutines to finish.
// The passed context controls how long to wait for graceful shutdown.
func (d *Driver) Shutdown(ctx context.Context) error {
	logger.Info("Driver shutdown initiated")

	// Cancel internal context to stop event watcher and pending operations
	d.cancel()

	// Stop all endpoints
	d.mu.Lock()
	endpoints := make([]*core.Endpoint, 0, len(d.endpoints))
	for _, ep := range d.endpoints {
		endpoints = append(endpoints, ep)
	}
	d.mu.Unlock()

	for _, ep := range endpoints {
		logger.Info("Stopping endpoint %s", ep.ID[:12])
		if err := ep.Stop(); err != nil {
			logger.Warn("Failed to stop endpoint %s: %v", ep.ID[:12], err)
		}
	}

	// Wait for all goroutines to finish (or context timeout)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("Driver shutdown complete")
	case <-ctx.Done():
		logger.Warn("Driver shutdown timed out, some goroutines may still be running")
		return ctx.Err()
	}

	// Close Docker client
	if d.docker != nil {
		d.docker.Close()
	}

	return nil
}

// runWatchdog periodically scans for orphaned endpoints and recovers them.
// This handles cases where containers restart after initial recovery.
func (d *Driver) runWatchdog(ctx context.Context) {
	// Wait before first check (let initial recovery complete)
	select {
	case <-ctx.Done():
		return
	case <-time.After(watchdogInterval):
	}

	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Watchdog shutting down")
			return
		case <-ticker.C:
			if err := d.RecoverEndpoints(ctx); err != nil {
				logger.Error("Watchdog recovery failed: %v", err)
			}
		}
	}
}

// RecoverEndpoints scans Docker for containers on tslink networks that are missing
// from the driver's in-memory state. This handles host reboot and plugin restart.
func (d *Driver) RecoverEndpoints(ctx context.Context) error {
	logger.Info("RecoverEndpoints: scanning for orphaned endpoints")

	// List all running containers
	containerList, err := d.docker.ContainerList(ctx, dockerclient.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	recovered := 0
	for _, container := range containerList.Items {
		// Check each network the container is attached to
		if container.NetworkSettings == nil {
			continue
		}

		for netName, netSettings := range container.NetworkSettings.Networks {
			if netSettings.NetworkID == "" || netSettings.EndpointID == "" {
				continue
			}

			// Check if this network uses our driver
			networkResult, err := d.docker.NetworkInspect(
				ctx,
				netSettings.NetworkID,
				dockerclient.NetworkInspectOptions{},
			)
			if err != nil {
				logger.Debug("RecoverEndpoints: failed to inspect network %s: %v", netSettings.NetworkID[:12], err)
				continue
			}

			if networkResult.Network.Driver != NetworkDriverName {
				continue
			}

			endpointID := netSettings.EndpointID

			// Check if we already have this endpoint
			d.mu.RLock()
			_, exists := d.endpoints[endpointID]
			d.mu.RUnlock()

			if exists {
				continue
			}

			// Found orphaned endpoint - recover it
			logger.Info("RecoverEndpoints: found orphaned endpoint %s for container %s on network %s",
				endpointID[:12], container.ID[:12], netName)

			if err := d.recoverEndpoint(ctx, container.ID, netName, netSettings.NetworkID, endpointID, networkResult); err != nil {
				logger.Error("RecoverEndpoints: failed to recover endpoint %s: %v", endpointID[:12], err)
				continue
			}
			recovered++
		}
	}

	if recovered > 0 {
		logger.Info("RecoverEndpoints: recovered %d orphaned endpoint(s)", recovered)
	} else {
		logger.Debug("RecoverEndpoints: no orphaned endpoints found")
	}

	return nil
}

// recoverEndpoint restores a single orphaned endpoint.
// It recreates the network and endpoint in driver state, then triggers Tailscale setup.
func (d *Driver) recoverEndpoint(
	ctx context.Context,
	containerID, netName, networkID, endpointID string,
	networkResult dockerclient.NetworkInspectResult,
) error {
	// Get full container info
	containerInfo, err := d.docker.ContainerInspect(ctx, containerID, dockerclient.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// Verify container is attached to the network
	if _, ok := containerInfo.Container.NetworkSettings.Networks[netName]; !ok {
		return fmt.Errorf("container not attached to network %s", netName)
	}

	// Sandbox key might be empty if container isn't fully running
	sandboxKey := containerInfo.Container.NetworkSettings.SandboxKey
	if sandboxKey == "" {
		return fmt.Errorf("container has no sandbox key (not fully started?)")
	}

	// Ensure network exists in driver state
	d.mu.Lock()
	net, netExists := d.networks[networkID]
	if !netExists {
		// Recover network from Docker info
		authKey := d.config.AuthKey // Use default auth key
		// Try to get auth key from network options
		if opts, ok := networkResult.Network.Options["ts.authkey"]; ok {
			authKey = opts
		}
		if authKey == "" {
			d.mu.Unlock()
			return fmt.Errorf("no auth key available for network %s", networkID[:12])
		}

		net = &core.Network{
			ID:      networkID,
			AuthKey: authKey,
		}
		d.networks[networkID] = net
		logger.Info("recoverEndpoint: recovered network %s", networkID[:12])
	}
	d.mu.Unlock()

	// Parse container info for Tailscale config
	name := strings.TrimPrefix(containerInfo.Container.Name, "/")

	labels := make(map[string]string)
	if containerInfo.Container.Config != nil && containerInfo.Container.Config.Labels != nil {
		labels = containerInfo.Container.Config.Labels
	}

	tsInfo := parseContainerInfo(name, labels)

	// Create endpoint
	endpoint, err := core.NewEndpoint(endpointID, net, core.EndpointOptions{Hostname: tsInfo.Hostname}, d.config)
	if err != nil {
		return fmt.Errorf("failed to create endpoint: %w", err)
	}

	// Store endpoint
	d.mu.Lock()
	// Double-check someone else didn't create it
	if _, exists := d.endpoints[endpointID]; exists {
		d.mu.Unlock()
		logger.Debug("recoverEndpoint: endpoint %s already exists (race)", endpointID[:12])
		return nil
	}
	d.endpoints[endpointID] = endpoint
	d.mu.Unlock()

	// Set sandbox key on endpoint (normally done by Join)
	endpoint.SetSandboxKey(sandboxKey)

	// Store in cache for event handler
	d.cache.Store(endpointID, tsInfo)

	// Trigger Tailscale setup
	// Note: We don't recreate the veth pair - if networking is broken,
	// the container needs to be restarted anyway. We only recover Tailscale.
	logger.Info(
		"recoverEndpoint: triggering Tailscale setup for endpoint %s (hostname=%s)",
		endpointID[:12],
		tsInfo.Hostname,
	)

	d.wg.Go(func() {
		if err := endpoint.StartTailscale(tsInfo); err != nil {
			logger.Error("recoverEndpoint: failed to start Tailscale for endpoint %s: %v", endpointID[:12], err)
			// Remove failed endpoint from state
			d.mu.Lock()
			delete(d.endpoints, endpointID)
			d.mu.Unlock()
		}
	})

	return nil
}
