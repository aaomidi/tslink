package tailscale

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// streamingWriter wraps output and logs each line as it arrives.
type streamingWriter struct {
	prefix string
	buf    bytes.Buffer
	mu     sync.Mutex
}

func (w *streamingWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err = w.buf.Write(p)
	if err != nil {
		return n, err
	}

	// Log complete lines as they arrive
	for {
		line, readErr := w.buf.ReadString('\n')
		if errors.Is(readErr, io.EOF) {
			// Put back incomplete line
			w.buf.WriteString(line)
			break
		}
		if readErr != nil {
			break
		}
		line = strings.TrimRight(line, "\n\r")
		if line != "" {
			logger.Debug("[%s] %s", w.prefix, line)
		}
	}
	return n, nil
}

func (w *streamingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// runCommandWithStreaming runs a command and streams its output to the logger.
func runCommandWithStreaming(ctx context.Context, prefix string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout := &streamingWriter{prefix: prefix + ":stdout"}
	stderr := &streamingWriter{prefix: prefix + ":stderr"}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	logger.Debug("[%s] Running: %s %v", prefix, name, args)

	err := cmd.Run()

	// Combine output for return
	output := stdout.String() + stderr.String()

	return output, err
}

// ServeEndpoint represents a single Tailscale serve configuration.
type ServeEndpoint struct {
	Proto  string // http, https, tcp, tls-terminated-tcp, tun
	Port   string // External port Tailscale exposes
	Target string // Container port or address to forward to
	Path   string // L7 only - path prefix (e.g., "/api")
}

// DaemonConfig holds configuration for a tailscaled instance.
type DaemonConfig struct {
	EndpointID    string
	StateDir      string
	Hostname      string
	AuthKey       string
	NetNSPath     string
	TailscaleBin  string          // Path to tailscale CLI binary
	TailscaledBin string          // Path to tailscaled daemon binary
	Tags          []string        // ACL tags for --advertise-tags
	Service       string          // Service name for tailscale serve (e.g., "svc:hello-world")
	Endpoints     []ServeEndpoint // Serve endpoints (L3/L4/L7)
	Direct        bool            // Enable direct machine serve (HTTPS on machine hostname)
}

// Status represents the status of a Tailscale connection.
type Status struct {
	IP       string
	Hostname string
	Online   bool
}

// Daemon manages a tailscaled process.
type Daemon struct {
	config     DaemonConfig
	cmd        *exec.Cmd
	socketPath string

	// Context for goroutine lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Mutex protects cmd and running state
	mu      sync.RWMutex
	running bool

	// Pipe references for cleanup (closing unblocks reader goroutines)
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
}

// NewDaemon creates a new Daemon instance.
func NewDaemon(cfg DaemonConfig) (*Daemon, error) {
	// Create state directory
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state dir: %w", err)
	}

	socketPath := filepath.Join(cfg.StateDir, "tailscaled.sock")

	// Clean stale socket from previous run (prevents "address in use" errors)
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		logger.Warn("Failed to remove stale socket %s: %v", socketPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Daemon{
		config:     cfg,
		socketPath: socketPath,
		ctx:        ctx,
		cancel:     cancel,
	}, nil
}

// Start starts the tailscaled process in the target network namespace.
func (d *Daemon) Start() error {
	logger.Info("Starting tailscaled for endpoint %s in netns %s", d.config.EndpointID, d.config.NetNSPath)

	statePath := filepath.Join(d.config.StateDir, "tailscaled.state")

	// Build tailscaled arguments
	// Use a real tun device (tailscale0) so containers can use Tailscale networking directly
	tailscaledArgs := []string{
		"--state=" + statePath,
		"--socket=" + d.socketPath,
		"--tun=tailscale0",
		"--statedir=" + d.config.StateDir,
	}

	// Use nsenter to run tailscaled in the container's network namespace
	nsenterArgs := []string{
		"--net=" + d.config.NetNSPath,
		"--",
		d.config.TailscaledBin,
	}
	nsenterArgs = append(nsenterArgs, tailscaledArgs...)

	logger.Debug("Running: nsenter %v", nsenterArgs)

	d.cmd = exec.CommandContext(d.ctx, "nsenter", nsenterArgs...)
	d.cmd.Env = append(os.Environ(), "TS_AUTHKEY="+d.config.AuthKey)

	// Set up streaming output - logs each line as it arrives
	stdoutPipe, err := d.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderrPipe, err := d.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Store pipe references for cleanup in Stop()
	d.mu.Lock()
	d.stdoutPipe = stdoutPipe
	d.stderrPipe = stderrPipe
	d.mu.Unlock()

	if err := d.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start tailscaled: %w", err)
	}

	d.mu.Lock()
	d.running = true
	d.mu.Unlock()

	logger.Info("tailscaled started with PID %d", d.cmd.Process.Pid)

	debugFile := filepath.Join(d.config.StateDir, "debug.log")

	// Stream stdout in background (exits when pipe is closed in Stop())
	d.wg.Go(func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			logger.Debug("[tailscaled:%s:stdout] %s", d.config.EndpointID[:8], scanner.Text())
		}
	})

	// Stream stderr in background (exits when pipe is closed in Stop())
	d.wg.Go(func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			logger.Debug("[tailscaled:%s:stderr] %s", d.config.EndpointID[:8], scanner.Text())
		}
	})

	// Write debug info to a file (cancellable via context)
	d.wg.Go(func() {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(2 * time.Second):
			debugInfo := fmt.Sprintf("PID: %d\nSocket: %s\nEndpoint: %s\n",
				d.cmd.Process.Pid, d.socketPath, d.config.EndpointID)
			if err := os.WriteFile(debugFile, []byte(debugInfo), 0644); err != nil {
				logger.Warn("Failed to write debug file: %v", err)
			}
		}
	})

	// Monitor for process exit (cancellable via context)
	d.wg.Go(func() {
		done := make(chan error, 1)
		go func() {
			done <- d.cmd.Wait()
		}()

		select {
		case <-d.ctx.Done():
			return
		case err := <-done:
			if err != nil {
				logger.Error("tailscaled exited with error: %v", err)
				debugInfo := fmt.Sprintf("EXITED WITH ERROR\nPID: %d\nError: %v\n",
					d.cmd.Process.Pid, err)
				if writeErr := os.WriteFile(debugFile, []byte(debugInfo), 0644); writeErr != nil {
					logger.Warn("Failed to write debug file: %v", writeErr)
				}
			}
		}
	})

	if err := d.waitForSocket(); err != nil {
		// Write timeout debug info
		debugInfo := fmt.Sprintf("SOCKET TIMEOUT\nSocket: %s\nPID: %d\n",
			d.socketPath, d.cmd.Process.Pid)
		if writeErr := os.WriteFile(debugFile, []byte(debugInfo), 0644); writeErr != nil {
			logger.Warn("Failed to write debug file: %v", writeErr)
		}

		if stopErr := d.Stop(); stopErr != nil {
			logger.Warn("Failed to stop daemon after socket timeout: %v", stopErr)
		}
		return fmt.Errorf("failed waiting for tailscaled socket: %w", err)
	}

	if err := d.bringUp(); err != nil {
		if stopErr := d.Stop(); stopErr != nil {
			logger.Warn("Failed to stop daemon after bringUp error: %v", stopErr)
		}
		return fmt.Errorf("failed to bring up tailscale: %w", err)
	}

	// Configure direct machine serve if enabled (HTTPS on machine hostname)
	logger.Info("Checking direct serve: Direct=%v Endpoints=%d", d.config.Direct, len(d.config.Endpoints))
	if d.config.Direct && len(d.config.Endpoints) > 0 {
		logger.Info("Configuring direct serve...")
		if err := d.configureDirectServe(); err != nil {
			if stopErr := d.Stop(); stopErr != nil {
				logger.Warn("Failed to stop daemon after direct serve error: %v", stopErr)
			}
			return fmt.Errorf("failed to configure direct serve: %w", err)
		}
		logger.Info("Direct serve configured successfully")
	}

	// Configure service backend if specified
	// Services is a beta feature - add delay to let control plane fully register the node
	logger.Info("Checking service: Service=%q", d.config.Service)
	if d.config.Service != "" {
		logger.Info("Waiting for control plane sync before configuring service backend...")
		time.Sleep(2 * time.Second)
		logger.Info("Configuring service backend...")
		if err := d.configureService(); err != nil {
			if stopErr := d.Stop(); stopErr != nil {
				logger.Warn("Failed to stop daemon after service config error: %v", stopErr)
			}
			return fmt.Errorf("failed to configure Tailscale service: %w", err)
		}
		logger.Info("Service backend configured successfully")
	}

	return nil
}

// waitForSocket waits for the tailscaled socket to be ready.
func (d *Daemon) waitForSocket() error {
	logger.Debug("Waiting for tailscaled socket at %s", d.socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for tailscaled socket")
		default:
			if _, err := os.Stat(d.socketPath); err == nil {
				logger.Info("tailscaled socket is ready")
				// Give it a bit more time to fully initialize
				time.Sleep(500 * time.Millisecond)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// bringUp runs "tailscale up" with retry logic for state reuse.
// First attempts with existing state, then wipes and retries on auth failures.
func (d *Daemon) bringUp() error {
	// First attempt with existing state
	err := d.tryBringUp()
	if err == nil {
		return nil
	}

	// Check if it's an auth/state error that warrants a retry
	if isStateError(err) {
		logger.Warn("Auth failed with existing state, wiping and retrying: %v", err)
		if err := WipeState(d.config.StateDir); err != nil {
			logger.Warn("Failed to wipe state: %v", err)
		}

		// Second attempt with fresh state
		logger.Info("Retrying tailscale up with fresh state...")
		return d.tryBringUp()
	}

	return err
}

// isStateError checks if the error indicates stale/invalid state that should trigger a retry.
func isStateError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Common auth/state errors that indicate we should wipe and retry
	return strings.Contains(errStr, "not logged in") ||
		strings.Contains(errStr, "key expired") ||
		strings.Contains(errStr, "node not found") ||
		strings.Contains(errStr, "node key mismatch") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "register request") ||
		strings.Contains(errStr, "invalid node key")
}

// tryBringUp runs "tailscale up" to connect to the network (single attempt).
func (d *Daemon) tryBringUp() error {
	logger.Info("Bringing up Tailscale for endpoint %s", d.config.EndpointID)

	// The tailscale CLI communicates with tailscaled via the socket.
	// Since the socket is on the host filesystem, we don't need nsenter.
	// Disable netfilter mode since our plugin handles routing via veth pairs and NAT
	args := []string{
		"--socket=" + d.socketPath,
		"up",
		"--hostname=" + d.config.Hostname,
		"--authkey=" + d.config.AuthKey,
		"--accept-routes",
		"--netfilter-mode=off",
	}

	// Add tags if configured (required for Services)
	if len(d.config.Tags) > 0 {
		tagsArg := strings.Join(d.config.Tags, ",")
		args = append(args, "--advertise-tags="+tagsArg)
	}

	// Log with redacted authkey
	redactedArgs := make([]string, len(args))
	for i, arg := range args {
		if strings.HasPrefix(arg, "--authkey=") {
			redactedArgs[i] = "--authkey=(redacted)"
		} else {
			redactedArgs[i] = arg
		}
	}
	logger.Debug("Running: %s %v", d.config.TailscaleBin, redactedArgs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("tailscale up failed with output: %s", string(output))
		return fmt.Errorf("tailscale up failed: %w (output: %s)", err, string(output))
	}

	logger.Info("tailscale up succeeded: %s", string(output))
	return nil
}

// configureService runs "tailscale serve" to register as a service backend.
// This should be called after bringUp() succeeds.
func (d *Daemon) configureService() error {
	if d.config.Service == "" {
		return nil // No service configured, skip
	}

	if len(d.config.Endpoints) == 0 {
		logger.Warn("Service %s configured but no endpoints defined", d.config.Service)
		return nil
	}

	logger.Info("Configuring Tailscale Service %s with %d endpoint(s) for %s",
		d.config.Service, len(d.config.Endpoints), d.config.EndpointID)

	// Configure each endpoint
	for i, ep := range d.config.Endpoints {
		if err := d.configureServeEndpoint(ep); err != nil {
			return fmt.Errorf("failed to configure endpoint %d (%s:%s): %w",
				i, ep.Proto, ep.Port, err)
		}
	}

	return nil
}

// configureDirectServe runs "tailscale serve" without --service flag to configure
// direct machine serve. This makes the container accessible via its MagicDNS hostname
// (e.g., https://hostname.tailnet.ts.net/) with automatic TLS certificates.
func (d *Daemon) configureDirectServe() error {
	if len(d.config.Endpoints) == 0 {
		return nil // No endpoints to configure
	}

	logger.Info("Configuring direct machine serve with %d endpoint(s) for %s",
		len(d.config.Endpoints), d.config.EndpointID)

	// Configure each endpoint for direct serve
	for i, ep := range d.config.Endpoints {
		if err := d.configureDirectServeEndpoint(ep); err != nil {
			return fmt.Errorf("failed to configure direct endpoint %d (%s:%s): %w",
				i, ep.Proto, ep.Port, err)
		}
	}

	return nil
}

// configureDirectServeEndpoint configures a single direct serve endpoint (without --service).
func (d *Daemon) configureDirectServeEndpoint(ep ServeEndpoint) error {
	logger.Debug("configureDirectServeEndpoint: proto=%s port=%s target=%s path=%s",
		ep.Proto, ep.Port, ep.Target, ep.Path)

	// Validate port is numeric
	if _, err := strconv.Atoi(ep.Port); err != nil {
		return fmt.Errorf("invalid external port %q: must be numeric", ep.Port)
	}
	if _, err := strconv.Atoi(ep.Target); err != nil {
		return fmt.Errorf("invalid target port %q: must be numeric", ep.Target)
	}

	// Build tailscale serve command based on protocol (no --service flag)
	var args []string

	switch ep.Proto {
	case "http", "https":
		// L7: tailscale serve --bg --http=80 localhost:8080 OR --https=443 localhost:8080
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--bg", // Run in background
			"--" + ep.Proto + "=" + ep.Port,
		}
		if ep.Path != "" {
			args = append(args, "--set-path="+ep.Path)
		}
		args = append(args, fmt.Sprintf("http://127.0.0.1:%s", ep.Target))

	case "tcp":
		// L4: tailscale serve --bg --tcp=5432 tcp://127.0.0.1:5432
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--bg", // Run in background
			"--tcp=" + ep.Port,
			fmt.Sprintf("tcp://127.0.0.1:%s", ep.Target),
		}

	case "tls-terminated-tcp":
		// L4 with TLS termination: tailscale serve --bg --tls-terminated-tcp=443 tcp://127.0.0.1:8080
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--bg", // Run in background
			"--tls-terminated-tcp=" + ep.Port,
			fmt.Sprintf("tcp://127.0.0.1:%s", ep.Target),
		}

	case "tun":
		// L3: not applicable for direct serve without service
		logger.Debug("Skipping L3 (tun) endpoint for direct serve - only supported with services")
		return nil

	default:
		return fmt.Errorf("unsupported protocol: %s", ep.Proto)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use streaming to see output as it arrives
	prefix := fmt.Sprintf("direct-serve:%s:%s", ep.Proto, ep.Port)
	output, err := runCommandWithStreaming(ctx, prefix, d.config.TailscaleBin, args...)

	if err != nil {
		logger.Error("tailscale serve (direct) failed: %v", err)
		return fmt.Errorf("tailscale serve (direct) failed: %w (output: %s)", err, output)
	}

	logger.Info("Direct serve endpoint %s:%s configured", ep.Proto, ep.Port)
	return nil
}

// configureServeEndpoint configures a single serve endpoint for a service backend.
// Supports L3 (tun), L4 (tcp, tls-terminated-tcp), and L7 (http, https).
func (d *Daemon) configureServeEndpoint(ep ServeEndpoint) error {
	logger.Debug("configureServeEndpoint: proto=%s port=%s target=%s path=%s service=%s",
		ep.Proto, ep.Port, ep.Target, ep.Path, d.config.Service)
	logger.Debug("Configuring serve endpoint: proto=%s port=%s target=%s path=%s",
		ep.Proto, ep.Port, ep.Target, ep.Path)

	// Validate port is numeric
	if _, err := strconv.Atoi(ep.Port); err != nil {
		return fmt.Errorf("invalid external port %q: must be numeric", ep.Port)
	}
	if _, err := strconv.Atoi(ep.Target); err != nil {
		return fmt.Errorf("invalid target port %q: must be numeric", ep.Target)
	}

	// Build tailscale serve command based on protocol
	var args []string

	switch ep.Proto {
	case "http", "https":
		// L7: tailscale serve --service=svc:name --https=443 127.0.0.1:8080
		// Per Tailscale docs, service mode auto-runs in background, target is just host:port
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--service=" + d.config.Service,
			"--" + ep.Proto + "=" + ep.Port,
		}
		if ep.Path != "" {
			args = append(args, "--set-path="+ep.Path)
		}
		args = append(args, fmt.Sprintf("127.0.0.1:%s", ep.Target))

	case "tcp":
		// L4: tailscale serve --service=svc:name --tcp=5432 tcp://127.0.0.1:5432
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--service=" + d.config.Service,
			"--tcp=" + ep.Port,
			fmt.Sprintf("tcp://127.0.0.1:%s", ep.Target),
		}

	case "tls-terminated-tcp":
		// L4 with TLS termination: tailscale serve --service=svc:name --tls-terminated-tcp=443 tcp://127.0.0.1:8080
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--service=" + d.config.Service,
			"--tls-terminated-tcp=" + ep.Port,
			fmt.Sprintf("tcp://127.0.0.1:%s", ep.Target),
		}

	case "tun":
		// L3: tailscale serve --service=svc:name --tun ...
		// Note: L3 requires additional iptables configuration
		args = []string{
			"--socket=" + d.socketPath,
			"serve",
			"--service=" + d.config.Service,
			"--tun",
		}
		logger.Warn("L3 (tun) endpoints require additional iptables configuration")

	default:
		return fmt.Errorf("unsupported protocol: %s", ep.Proto)
	}

	// Debug: write to file so we can trace execution
	debugPath := filepath.Join(d.config.StateDir, "serve-debug.log")
	debugMsg := fmt.Sprintf("Running: %s %v\n", d.config.TailscaleBin, args)
	if err := os.WriteFile(debugPath, []byte(debugMsg), 0644); err != nil {
		logger.Warn("Failed to write serve debug file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use streaming to see output as it arrives
	prefix := fmt.Sprintf("serve:%s:%s", ep.Proto, ep.Port)
	output, err := runCommandWithStreaming(ctx, prefix, d.config.TailscaleBin, args...)

	if err != nil {
		debugMsg = fmt.Sprintf("FAILED: %v\nOutput: %s\n", err, output)
		if writeErr := os.WriteFile(debugPath, []byte(debugMsg), 0644); writeErr != nil {
			logger.Warn("Failed to write serve debug file: %v", writeErr)
		}
		logger.Error("tailscale serve failed: %v", err)

		if strings.Contains(output, "service not found") || strings.Contains(output, "unknown service") {
			return fmt.Errorf("service %s not found: create it in Tailscale admin console first",
				d.config.Service)
		}
		if strings.Contains(output, "tagged") || strings.Contains(output, "tag") {
			return fmt.Errorf("tailscale serve failed: requires tagged auth key (output: %s)", output)
		}

		return fmt.Errorf("tailscale serve failed: %w (output: %s)", err, output)
	}

	// Check for approval pending (command succeeds but backend not active yet)
	if strings.Contains(output, "approval from an admin is required") {
		logger.Warn("Service backend registered but pending admin approval: %s", d.config.Service)
	}

	debugMsg = fmt.Sprintf("SUCCESS\nOutput: %s\n", output)
	if err := os.WriteFile(debugPath, []byte(debugMsg), 0644); err != nil {
		logger.Warn("Failed to write serve debug file: %v", err)
	}
	logger.Info("tailscale serve endpoint %s:%s configured", ep.Proto, ep.Port)
	return nil
}

// WaitForIP waits for Tailscale to get an IP address.
func (d *Daemon) WaitForIP() (*Status, error) {
	logger.Debug("Waiting for Tailscale IP for endpoint %s", d.config.EndpointID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for Tailscale IP")
		default:
			status, err := d.getStatus()
			if err == nil && status.IP != "" {
				return status, nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// getStatus gets the current Tailscale status.
func (d *Daemon) getStatus() (*Status, error) {
	args := []string{
		"--socket=" + d.socketPath,
		"status",
		"--json",
	}

	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}

	var result struct {
		Self struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
			HostName     string   `json:"HostName"`
			Online       bool     `json:"Online"`
		} `json:"Self"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	status := &Status{
		Hostname: result.Self.HostName,
		Online:   result.Self.Online,
	}

	if len(result.Self.TailscaleIPs) > 0 {
		status.IP = result.Self.TailscaleIPs[0]
	}

	return status, nil
}

// IsRunning checks if the tailscaled process is still running.
func (d *Daemon) IsRunning() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if !d.running || d.cmd == nil || d.cmd.Process == nil {
		return false
	}
	// Signal 0 checks if process exists without killing it
	err := d.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// Stop stops the tailscaled process.
func (d *Daemon) Stop() error {
	logger.Info("Stopping tailscaled for endpoint %s", d.config.EndpointID)

	// Mark as not running and get current state
	d.mu.Lock()
	d.running = false
	cmd := d.cmd
	stdoutPipe := d.stdoutPipe
	stderrPipe := d.stderrPipe
	d.stdoutPipe = nil
	d.stderrPipe = nil
	d.mu.Unlock()

	// Cancel context to signal all goroutines to stop
	if d.cancel != nil {
		d.cancel()
	}

	if cmd != nil && cmd.Process != nil {
		// Try graceful shutdown first
		args := []string{
			"--socket=" + d.socketPath,
			"down",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		downCmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
		_ = downCmd.Run() // Ignore errors

		// Kill the process
		if err := cmd.Process.Kill(); err != nil {
			logger.Warn("Failed to kill tailscaled: %v", err)
		}

		// Wait for process with timeout to avoid blocking forever
		waitDone := make(chan struct{})
		go func() {
			if err := cmd.Wait(); err != nil {
				logger.Debug("tailscaled process exited: %v", err)
			}
			close(waitDone)
		}()

		select {
		case <-waitDone:
			// Process exited cleanly
		case <-time.After(5 * time.Second):
			logger.Warn("Timeout waiting for tailscaled to exit")
		}
	}

	// Close pipes to unblock reader goroutines
	if stdoutPipe != nil {
		if err := stdoutPipe.Close(); err != nil {
			logger.Debug("Failed to close stdout pipe: %v", err)
		}
	}
	if stderrPipe != nil {
		if err := stderrPipe.Close(); err != nil {
			logger.Debug("Failed to close stderr pipe: %v", err)
		}
	}

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debug("All daemon goroutines stopped")
	case <-time.After(5 * time.Second):
		logger.Warn("Timeout waiting for daemon goroutines to stop")
		return fmt.Errorf("timeout waiting for daemon goroutines")
	}

	return nil
}

// SetHostname updates the Tailscale hostname for a running daemon.
func (d *Daemon) SetHostname(hostname string) error {
	logger.Info("Setting hostname to %s for endpoint %s", hostname, d.config.EndpointID)

	args := []string{
		"--socket=" + d.socketPath,
		"set",
		"--hostname=" + hostname,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale set --hostname failed: %w (output: %s)", err, string(output))
	}

	// Update internal config (protected by mutex)
	d.mu.Lock()
	d.config.Hostname = hostname
	d.mu.Unlock()

	// Brief wait for hostname change to propagate to Tailscale's internal state
	// This ensures subsequent serve commands use the new hostname
	time.Sleep(500 * time.Millisecond)

	logger.Info("Hostname updated successfully")
	return nil
}

// ConfigureServeEndpoints configures multiple Tailscale serve endpoints after startup.
// This is called when container info is obtained from cache after initial Join.
func (d *Daemon) ConfigureServeEndpoints(service string, endpoints []ServeEndpoint, tags []string, direct bool) error {
	logger.Info("Late-configuring serve endpoints: service=%s endpoints=%d direct=%v for %s",
		service, len(endpoints), direct, d.config.EndpointID)

	// Update tags if provided
	if len(tags) > 0 {
		tagsArg := strings.Join(tags, ",")
		args := []string{
			"--socket=" + d.socketPath,
			"set",
			"--advertise-tags=" + tagsArg,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, d.config.TailscaleBin, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			logger.Warn("Failed to set tags: %v (output: %s)", err, string(output))
		}
	}

	// Store config for endpoint configuration
	d.config.Service = service
	d.config.Endpoints = endpoints
	d.config.Direct = direct

	// Configure direct serve first (fast access via machine hostname)
	if direct && len(endpoints) > 0 {
		for i, ep := range endpoints {
			if err := d.configureDirectServeEndpoint(ep); err != nil {
				return fmt.Errorf("failed to configure direct endpoint %d (%s:%s): %w",
					i, ep.Proto, ep.Port, err)
			}
		}
		logger.Info("Late direct serve configuration completed")
	}

	// Configure service backend if specified
	if service != "" {
		for i, ep := range endpoints {
			if err := d.configureServeEndpoint(ep); err != nil {
				return fmt.Errorf("failed to configure service endpoint %d (%s:%s): %w",
					i, ep.Proto, ep.Port, err)
			}
		}
		logger.Info("Late service configuration completed")
	}

	return nil
}
