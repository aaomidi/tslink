package diag

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EndpointStatus represents the health status of a single endpoint.
type EndpointStatus struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"short_id"`
	StateDir     string    `json:"state_dir"`
	SocketExists bool      `json:"socket_exists"`
	StateExists  bool      `json:"state_exists"`
	TailscaleIP  string    `json:"tailscale_ip,omitempty"`
	Hostname     string    `json:"hostname,omitempty"`
	Online       bool      `json:"online"`
	BackendState string    `json:"backend_state,omitempty"`
	LastModified time.Time `json:"last_modified"`
	DebugLog     string    `json:"debug_log,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// DiagResult represents the overall diagnostic result.
type DiagResult struct {
	Timestamp     time.Time         `json:"timestamp"`
	DataDir       string            `json:"data_dir"`
	DataDirExists bool              `json:"data_dir_exists"`
	Endpoints     []*EndpointStatus `json:"endpoints"`
	Summary       Summary           `json:"summary"`
}

// Summary provides a quick overview of endpoint health.
type Summary struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Errors  int `json:"errors"`
}

// Run performs diagnostics on the tslink plugin state.
func Run(dataDir string, w io.Writer) error {
	result := &DiagResult{
		Timestamp: time.Now(),
		DataDir:   dataDir,
		Endpoints: make([]*EndpointStatus, 0),
	}

	info, err := os.Stat(dataDir)
	if err != nil {
		result.DataDirExists = false
		return outputResult(result, w)
	}
	result.DataDirExists = info.IsDir()

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("failed to read data directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip non-endpoint directories (like tailscale-bin cache)
		if entry.Name() == "tailscale-bin" {
			continue
		}

		status := checkEndpoint(dataDir, entry.Name())
		result.Endpoints = append(result.Endpoints, status)

		// Update summary
		result.Summary.Total++
		switch {
		case status.Error != "":
			result.Summary.Errors++
		case status.Online:
			result.Summary.Online++
		default:
			result.Summary.Offline++
		}
	}

	return outputResult(result, w)
}

// checkEndpoint checks the status of a single endpoint.
func checkEndpoint(dataDir, endpointID string) *EndpointStatus {
	stateDir := filepath.Join(dataDir, endpointID)
	status := &EndpointStatus{
		ID:       endpointID,
		ShortID:  truncateID(endpointID),
		StateDir: stateDir,
	}

	socketPath := filepath.Join(stateDir, "tailscaled.sock")
	if _, err := os.Stat(socketPath); err == nil {
		status.SocketExists = true
	}

	statePath := filepath.Join(stateDir, "tailscaled.state")
	if info, err := os.Stat(statePath); err == nil {
		status.StateExists = true
		status.LastModified = info.ModTime()
	}

	debugPath := filepath.Join(stateDir, "debug.log")
	if data, err := os.ReadFile(debugPath); err == nil {
		status.DebugLog = string(data)
	}

	// Try to get tailscale status if socket exists
	if status.SocketExists {
		tsStatus, err := getTailscaleStatus(socketPath)
		if err != nil {
			status.Error = err.Error()
		} else {
			status.TailscaleIP = tsStatus.IP
			status.Hostname = tsStatus.Hostname
			status.Online = tsStatus.Online
			status.BackendState = tsStatus.BackendState
		}
	}

	return status
}

type tailscaleStatusResult struct {
	IP           string
	Hostname     string
	Online       bool
	BackendState string
}

// getTailscaleStatus queries tailscale status via the socket.
func getTailscaleStatus(socketPath string) (*tailscaleStatusResult, error) {
	// Find tailscale binary - check common locations
	tailscaleBin := findTailscaleBinary()
	if tailscaleBin == "" {
		return nil, fmt.Errorf("tailscale binary not found")
	}

	cmd := exec.Command(tailscaleBin, "--socket="+socketPath, "status", "--json")
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
		BackendState string `json:"BackendState"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	status := &tailscaleStatusResult{
		Hostname:     result.Self.HostName,
		Online:       result.Self.Online,
		BackendState: result.BackendState,
	}

	if len(result.Self.TailscaleIPs) > 0 {
		status.IP = result.Self.TailscaleIPs[0]
	}

	return status, nil
}

// findTailscaleBinary looks for the tailscale binary in common locations.
func findTailscaleBinary() string {
	// Check in data directory first (downloaded binaries)
	entries, err := os.ReadDir("/data/tailscale-bin")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				binPath := filepath.Join("/data/tailscale-bin", e.Name(), "tailscale")
				if _, err := os.Stat(binPath); err == nil {
					return binPath
				}
			}
		}
	}

	paths := []string{
		"/usr/bin/tailscale",
		"/usr/local/bin/tailscale",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func outputResult(result *DiagResult, w io.Writer) error {
	fmt.Fprintf(w, "=== tslink Diagnostic Report ===\n")
	fmt.Fprintf(w, "Timestamp: %s\n", result.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(w, "Data Dir:  %s (exists: %v)\n\n", result.DataDir, result.DataDirExists)

	if !result.DataDirExists {
		fmt.Fprintf(w, "ERROR: Data directory does not exist\n")
		return nil
	}

	if len(result.Endpoints) == 0 {
		fmt.Fprintf(w, "No endpoints found.\n")
		return nil
	}

	fmt.Fprintf(w, "=== Summary ===\n")
	fmt.Fprintf(w, "Total: %d | Online: %d | Offline: %d | Errors: %d\n\n",
		result.Summary.Total, result.Summary.Online, result.Summary.Offline, result.Summary.Errors)

	fmt.Fprintf(w, "=== Endpoints ===\n")
	for _, ep := range result.Endpoints {
		fmt.Fprintf(w, "\n--- %s ---\n", ep.ShortID)
		fmt.Fprintf(w, "  Socket:    %v\n", ep.SocketExists)
		fmt.Fprintf(w, "  State:     %v\n", ep.StateExists)

		if ep.TailscaleIP != "" {
			fmt.Fprintf(w, "  IP:        %s\n", ep.TailscaleIP)
		}
		if ep.Hostname != "" {
			fmt.Fprintf(w, "  Hostname:  %s\n", ep.Hostname)
		}
		if ep.BackendState != "" {
			fmt.Fprintf(w, "  Backend:   %s\n", ep.BackendState)
		}
		fmt.Fprintf(w, "  Online:    %v\n", ep.Online)

		if ep.Error != "" {
			fmt.Fprintf(w, "  ERROR:     %s\n", ep.Error)
		}

		if ep.DebugLog != "" {
			fmt.Fprintf(w, "  Debug Log:\n")
			for line := range strings.SplitSeq(ep.DebugLog, "\n") {
				if line != "" {
					fmt.Fprintf(w, "    %s\n", line)
				}
			}
		}
	}

	return nil
}
