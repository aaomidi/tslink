package core

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/aaomidi/tslink/pkg/logger"
	"github.com/aaomidi/tslink/pkg/netutil"
	"github.com/aaomidi/tslink/pkg/tailscale"
)

// ServeEndpoint represents a single Tailscale serve configuration.
// Configured via labels like: ts.serve.443=https:8080/api.
type ServeEndpoint struct {
	Proto  string // http, https, tcp, tls-terminated-tcp, tun
	Port   string // External port Tailscale exposes
	Target string // Container port or address to forward to
	Path   string // L7 only - path prefix (e.g., "/api")
}

// Endpoint represents a container endpoint with Tailscale connectivity.
type Endpoint struct {
	mu sync.RWMutex // Protects concurrent access to mutable fields

	ID          string
	Network     *Network
	Hostname    string
	Tags        []string        // ACL tags for tailscale up --advertise-tags
	Service     string          // Service name (e.g., "svc:hello-world")
	Endpoints   []ServeEndpoint // Serve endpoints (replaces ServePort)
	Direct      bool            // Enable direct machine serve (default: true)
	TailscaleIP string
	VethName    string
	StateDir    string
	TSVersion   string // Tailscale version to use
	TSPath      string // Optional custom Tailscale binary path
	SandboxKey  string // Container's network namespace path, stored during Join
	DataDir     string // Base data directory for state

	supervisor       *tailscale.DaemonSupervisor
	tailscaleStarted bool // Whether Tailscale setup has been completed
}

// ContainerInfo holds information extracted from Docker container inspection.
type ContainerInfo struct {
	Name      string            // Container name (without leading /)
	Labels    map[string]string // All container labels
	Hostname  string            // Parsed ts.hostname label or container name
	Tags      []string          // Parsed ts.tags label (comma-separated)
	Service   string            // Parsed ts.service label (e.g., "svc:hello-world")
	Endpoints []ServeEndpoint   // Parsed ts.serve.<port> labels
	Direct    bool              // Enable direct machine serve (default: true, set ts.direct=false to disable)
}

// NewEndpoint creates a new endpoint with the given configuration.
func NewEndpoint(id string, net *Network, opts EndpointOptions, cfg *Config) (*Endpoint, error) {
	hostname := opts.Hostname
	if hostname == "" {
		// Use short endpoint ID as default hostname (will be updated when container info arrives)
		hostname = id[:12]
	}

	// Use hostname-based state directory for Tailscale identity reuse
	// Same hostname = same Tailscale node, enabling state reuse on container restart
	stateDir := filepath.Join(cfg.DataDir, "by-hostname", hostname)

	return &Endpoint{
		ID:        id,
		Network:   net,
		Hostname:  hostname,
		Direct:    true, // Default to direct serve enabled
		StateDir:  stateDir,
		DataDir:   cfg.DataDir,
		TSVersion: cfg.TSVersion,
		TSPath:    cfg.TSPath,
	}, nil
}

// generateVethIPs generates unique IP addresses for a veth pair based on endpoint ID.
// Uses 10.200.0.0/16 range, with each endpoint getting a /30 subnet.
// Returns (hostIP, containerIP).
func generateVethIPs(endpointID string) (string, string) {
	// Use first 4 bytes of endpoint ID to generate a unique subnet
	// Each /30 has 4 IPs: network, host, container, broadcast
	// So we can have 65536/4 = 16384 unique subnets
	var hash uint16
	for i := 0; i < len(endpointID) && i < 8; i++ {
		hash = hash*31 + uint16(endpointID[i])
	}

	// Ensure we don't use .0 or .255 subnets
	subnetNum := (hash % 16380) + 1 // 1 to 16380
	baseIP := subnetNum * 4         // Each /30 uses 4 IPs

	// 10.200.x.y where x.y comes from baseIP
	thirdOctet := baseIP / 256
	fourthOctet := baseIP % 256

	hostIP := fmt.Sprintf("10.200.%d.%d", thirdOctet, fourthOctet+1)
	containerIP := fmt.Sprintf("10.200.%d.%d", thirdOctet, fourthOctet+2)

	return hostIP, containerIP
}

// Join is called when a container joins the network.
// It sets up basic networking (veth, IPs, routing, NAT) and stores the sandbox key.
// Tailscale setup is deferred until container info is available via Docker events.
// This allows Join() to return quickly while Tailscale is configured asynchronously.
func (e *Endpoint) Join(sandboxKey string) (*network.JoinResponse, error) {
	// NOTE: We intentionally do NOT hold the lock during network operations.
	// Network syscalls can block for seconds, which would block all other endpoint operations.
	// We only lock briefly to read/write state.

	logger.Info("Endpoint %s joining with sandbox %s", e.ID, sandboxKey)

	// Generate unique IPs for this endpoint's veth pair (uses only e.ID which is immutable)
	hostVethIP, containerVethIP := generateVethIPs(e.ID)
	logger.Info("Using veth IPs: host=%s container=%s", hostVethIP, containerVethIP)

	// Create veth pair (blocking network operation - no lock held)
	vethHost, vethContainer, err := netutil.CreateVethPair(e.ID[:8])
	if err != nil {
		return nil, fmt.Errorf("failed to create veth pair: %w", err)
	}

	logger.Info("Created veth pair: host=%s container=%s", vethHost, vethContainer)

	// Move container end of veth into container's network namespace
	if err := netutil.MoveToNetNS(vethContainer, sandboxKey); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to move veth to container ns: %w", err)
	}

	// Set up host side of veth with IP
	if err := netutil.SetupHostRouting(vethHost, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up host routing: %w", err)
	}

	// Set up container side with IP, bring it up, and add default route
	if err := netutil.SetupInterfaceInNS(sandboxKey, vethContainer, ""); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container interface: %w", err)
	}

	if err := netutil.SetupContainerRouting(sandboxKey, vethContainer, containerVethIP, hostVethIP); err != nil {
		if cleanupErr := netutil.DeleteVeth(vethHost); cleanupErr != nil {
			logger.Warn("failed to cleanup veth %s after error: %v", vethHost, cleanupErr)
		}
		return nil, fmt.Errorf("failed to set up container routing: %w", err)
	}

	// Set up NAT/MASQUERADE for internet access
	if err := netutil.SetupNAT(vethHost); err != nil {
		logger.Info("Warning: failed to set up NAT: %v", err)
		// Continue anyway - tailscaled might still work via DERP
	}

	// Now lock briefly to store results
	e.mu.Lock()
	e.SandboxKey = sandboxKey
	e.VethName = vethHost
	e.mu.Unlock()

	logger.Info(
		"Network setup complete for endpoint %s, Tailscale will be configured when container info arrives",
		e.ID[:12],
	)

	// Return immediately - Tailscale setup will be triggered by Docker event
	return &network.JoinResponse{
		DisableGatewayService: true,
	}, nil
}

// StartTailscale starts the Tailscale daemon with the provided container info.
// This is called by the event handler when container info becomes available.
// It uses the correct hostname and state directory for identity reuse.
func (e *Endpoint) StartTailscale(info *ContainerInfo) error {
	// NOTE: We intentionally do NOT hold the lock during blocking operations.
	// Binary download can take seconds, supervisor startup can take 90s, WaitForIP can take 60s.
	// We only lock briefly to check/update state.

	// Quick check if already started (avoids duplicate work)
	e.mu.Lock()
	if e.tailscaleStarted {
		e.mu.Unlock()
		logger.Debug("Tailscale already started for endpoint %s", e.ID[:12])
		return nil
	}
	if e.supervisor != nil {
		e.mu.Unlock()
		logger.Debug("Tailscale startup already in progress for endpoint %s", e.ID[:12])
		return nil
	}
	sandboxKey := e.SandboxKey
	if sandboxKey == "" {
		e.mu.Unlock()
		return fmt.Errorf("no sandbox key set - Join() must be called first")
	}

	// Copy immutable config needed for supervisor creation
	endpointID := e.ID
	dataDir := e.DataDir
	tsVersion := e.TSVersion
	tsPath := e.TSPath
	authKey := e.Network.AuthKey
	e.mu.Unlock()

	// Compute state directory from hostname
	stateDir := filepath.Join(dataDir, "by-hostname", info.Hostname)

	logger.Info("StartTailscale: endpoint=%s hostname=%s service=%s endpoints=%d direct=%v",
		endpointID[:12], info.Hostname, info.Service, len(info.Endpoints), info.Direct)

	// Validate service configuration
	if info.Service != "" && len(info.Endpoints) == 0 {
		return fmt.Errorf("ts.service requires at least one ts.serve.<port> endpoint")
	}

	// Ensure Tailscale binaries are available (may download - blocking!)
	tailscaleBin, tailscaledBin, err := tailscale.EnsureBinaries(tsVersion, tsPath)
	if err != nil {
		return fmt.Errorf("failed to ensure Tailscale binaries: %w", err)
	}

	// Version check only when Services are used
	if info.Service != "" {
		version, err := tailscale.GetInstalledVersion(tailscaleBin)
		if err != nil {
			logger.Info("Warning: could not determine Tailscale version: %v", err)
		} else if err := tailscale.CheckVersionForServices(version); err != nil {
			return err
		}
	}

	// Convert core.ServeEndpoint to tailscale.ServeEndpoint
	tsEndpoints := make([]tailscale.ServeEndpoint, len(info.Endpoints))
	for i, ep := range info.Endpoints {
		tsEndpoints[i] = tailscale.ServeEndpoint{
			Proto:  ep.Proto,
			Port:   ep.Port,
			Target: ep.Target,
			Path:   ep.Path,
		}
	}

	// Create supervisor (handles daemon lifecycle with auto-recovery)
	supervisor := tailscale.NewDaemonSupervisor(tailscale.DaemonConfig{
		EndpointID:    endpointID,
		StateDir:      stateDir,
		Hostname:      info.Hostname,
		AuthKey:       authKey,
		NetNSPath:     sandboxKey,
		TailscaleBin:  tailscaleBin,
		TailscaledBin: tailscaledBin,
		Tags:          info.Tags,
		Service:       info.Service,
		Endpoints:     tsEndpoints,
		Direct:        info.Direct,
	})

	// Start supervisor (blocks until initial daemon startup succeeds or fails - up to 90s!)
	if err := supervisor.Start(); err != nil {
		return fmt.Errorf("failed to start tailscale supervisor: %w", err)
	}

	// Wait for Tailscale to connect and get IP (can take up to 60s!)
	status, err := supervisor.WaitForIP()
	if err != nil {
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warn("failed to stop supervisor after WaitForIP error: %v", stopErr)
		}
		return fmt.Errorf("failed to get Tailscale IP: %w", err)
	}

	// Now lock briefly to store results
	e.mu.Lock()
	// Double-check we didn't race with another caller
	if e.tailscaleStarted {
		e.mu.Unlock()
		if stopErr := supervisor.Stop(); stopErr != nil {
			logger.Warn("failed to stop duplicate supervisor: %v", stopErr)
		}
		logger.Warn("Tailscale was started by another goroutine for endpoint %s", endpointID[:12])
		return nil
	}
	e.supervisor = supervisor
	e.TailscaleIP = status.IP
	e.Hostname = info.Hostname
	e.Tags = info.Tags
	e.Service = info.Service
	e.Endpoints = info.Endpoints
	e.Direct = info.Direct
	e.StateDir = stateDir
	e.tailscaleStarted = true
	e.mu.Unlock()

	logger.Info("Endpoint %s got Tailscale IP: %s (hostname=%s)", endpointID[:12], status.IP, info.Hostname)
	return nil
}

// IsTailscaleStarted returns whether Tailscale has been started for this endpoint.
func (e *Endpoint) IsTailscaleStarted() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tailscaleStarted
}

// GetSandboxKey returns the sandbox key (container netns path) safely.
func (e *Endpoint) GetSandboxKey() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.SandboxKey
}

// SetSandboxKey sets the sandbox key (used during recovery when Join wasn't called).
func (e *Endpoint) SetSandboxKey(sandboxKey string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.SandboxKey = sandboxKey
}

// GetInfo returns TailscaleIP and Hostname safely for status queries.
func (e *Endpoint) GetInfo() (tailscaleIP, hostname string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.TailscaleIP, e.Hostname
}

// Leave is called when a container leaves the network.
// It stops the Tailscale supervisor and cleans up networking resources.
func (e *Endpoint) Leave() error {
	e.mu.Lock()
	vethName := e.VethName
	supervisor := e.supervisor
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()

	logger.Info("Endpoint %s leaving", e.ID)

	// Stop the Tailscale supervisor first
	if supervisor != nil {
		if err := supervisor.Stop(); err != nil {
			logger.Warn("Failed to stop supervisor during Leave: %v", err)
		}
	}

	// Clean up NAT rules for this veth
	if vethName != "" {
		if err := netutil.CleanupNAT(vethName); err != nil {
			logger.Info("Warning: failed to cleanup NAT for %s: %v", vethName, err)
		}
	}

	// Clean up veth (this also removes the container-side interface)
	if vethName != "" {
		if err := netutil.DeleteVeth(vethName); err != nil {
			logger.Info("Warning: failed to delete veth %s: %v", vethName, err)
		}
	}

	return nil
}

// Stop stops the endpoint and cleans up resources.
func (e *Endpoint) Stop() error {
	e.mu.Lock()
	supervisor := e.supervisor
	e.supervisor = nil
	e.tailscaleStarted = false
	e.mu.Unlock()

	logger.Info("Stopping endpoint %s", e.ID)

	if supervisor != nil {
		if err := supervisor.Stop(); err != nil {
			logger.Info("Warning: failed to stop supervisor: %v", err)
		}
	}

	return nil
}

// ApplyHostnameChange changes the hostname of a running Tailscale instance.
func (e *Endpoint) ApplyHostnameChange(newHostname string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.supervisor == nil {
		return fmt.Errorf("no supervisor running")
	}

	logger.Info("ApplyHostnameChange: changing hostname from %s to %s", e.Hostname, newHostname)

	if err := e.supervisor.SetHostname(newHostname); err != nil {
		return fmt.Errorf("failed to set hostname: %w", err)
	}

	e.Hostname = newHostname
	return nil
}

// ApplyServiceConfig configures Tailscale service endpoints after initial startup.
// This is called when container info is obtained from cache after Join().
func (e *Endpoint) ApplyServiceConfig(info *ContainerInfo) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.supervisor == nil {
		return fmt.Errorf("no supervisor running")
	}

	if info.Service == "" {
		return nil // No service to configure
	}

	logger.Info("ApplyServiceConfig: configuring service %s with %d endpoints", info.Service, len(info.Endpoints))

	// Convert core.ServeEndpoint to tailscale.ServeEndpoint
	tsEndpoints := make([]tailscale.ServeEndpoint, len(info.Endpoints))
	for i, ep := range info.Endpoints {
		tsEndpoints[i] = tailscale.ServeEndpoint{
			Proto:  ep.Proto,
			Port:   ep.Port,
			Target: ep.Target,
			Path:   ep.Path,
		}
	}

	if err := e.supervisor.ConfigureServeEndpoints(info.Service, tsEndpoints, info.Tags, info.Direct); err != nil {
		return fmt.Errorf("failed to configure service: %w", err)
	}

	e.Service = info.Service
	e.Tags = info.Tags
	e.Endpoints = info.Endpoints
	e.Direct = info.Direct
	return nil
}
