package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaomidi/tslink/pkg/logger"
)

const authKeyHashFile = "authkey.sha256"

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
// Removes tailscaled.state, tailscaled.sock, debug.log, and authkey hash.
func WipeState(stateDir string) error {
	logger.Info("Wiping state directory: %s", stateDir)

	files := []string{
		filepath.Join(stateDir, "tailscaled.state"),
		filepath.Join(stateDir, "tailscaled.sock"),
		filepath.Join(stateDir, "debug.log"),
		filepath.Join(stateDir, authKeyHashFile),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove %s: %v", f, err)
		}
	}

	return nil
}

// hashAuthKey returns the SHA256 hash of an auth key.
func hashAuthKey(authKey string) string {
	h := sha256.Sum256([]byte(authKey))
	return hex.EncodeToString(h[:])
}

// SaveAuthKeyHash stores the SHA256 hash of the auth key in the state directory.
func SaveAuthKeyHash(stateDir, authKey string) error {
	hashPath := filepath.Join(stateDir, authKeyHashFile)
	hash := hashAuthKey(authKey)
	return os.WriteFile(hashPath, []byte(hash), 0600)
}

// CheckAuthKeyMatch checks if the provided auth key matches the stored hash.
// Returns true if they match or if no hash is stored (new state).
// Returns false if they don't match (auth key changed).
func CheckAuthKeyMatch(stateDir, authKey string) bool {
	hashPath := filepath.Join(stateDir, authKeyHashFile)
	stored, err := os.ReadFile(hashPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No stored hash - this is a new state or old state without hash
			return true
		}
		logger.Warn("Failed to read auth key hash: %v", err)
		return true // Assume match on error to avoid breaking existing setups
	}

	currentHash := hashAuthKey(authKey)
	match := strings.TrimSpace(string(stored)) == currentHash
	if !match {
		logger.Info("Auth key changed - state will be wiped for fresh registration")
	}
	return match
}
