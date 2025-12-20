package tailscale

import (
	"os"
	"path/filepath"

	"github.com/aaomidi/tslink/pkg/logger"
)

// GetHostnameStateDir returns the state directory path for a hostname.
// The directory structure is: <dataDir>/by-hostname/<hostname>/.
func GetHostnameStateDir(dataDir, hostname string) string {
	return filepath.Join(dataDir, "by-hostname", hostname)
}

// StateExists checks if valid Tailscale state exists in the given directory.
// Returns true if tailscaled.state file exists and is non-empty.
func StateExists(stateDir string) bool {
	statePath := filepath.Join(stateDir, "tailscaled.state")
	info, err := os.Stat(statePath)
	if err != nil {
		return false
	}
	return info.Size() > 0
}

// WipeState removes all state files from the directory to allow fresh registration.
// Removes tailscaled.state, tailscaled.sock, and debug.log.
func WipeState(stateDir string) error {
	logger.Info("Wiping state directory: %s", stateDir)

	files := []string{
		filepath.Join(stateDir, "tailscaled.state"),
		filepath.Join(stateDir, "tailscaled.sock"),
		filepath.Join(stateDir, "debug.log"),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove %s: %v", f, err)
		}
	}

	return nil
}
