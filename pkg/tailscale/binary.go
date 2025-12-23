package tailscale

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"

	"github.com/aaomidi/tslink/pkg/logger"
)

const (
	stableAPIURL  = "https://pkgs.tailscale.com/stable/?mode=json"
	stableBaseURL = "https://pkgs.tailscale.com/stable/"
	binCacheDir   = "/data/tailscale-bin"
)

var (
	binaryMu      sync.Mutex
	cachedBinDir  string
	cachedVersion string
)

// stableRelease represents the JSON response from Tailscale's stable API.
type stableRelease struct {
	Tarballs        map[string]string `json:"Tarballs"`
	TarballsVersion string            `json:"TarballsVersion"`
}

// getArch returns the architecture string used by Tailscale downloads.
func getArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	case "amd64":
		return "amd64"
	default:
		return runtime.GOARCH
	}
}

// FetchLatestVersion queries the Tailscale API for the latest stable version.
func FetchLatestVersion() (string, error) {
	logger.Info("Fetching latest Tailscale version from %s", stableAPIURL)

	resp, err := http.Get(stableAPIURL)
	if err != nil {
		return "", fmt.Errorf("failed to fetch version API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version API returned status %d", resp.StatusCode)
	}

	var release stableRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("failed to parse version API response: %w", err)
	}

	// Verify the architecture is available
	arch := getArch()
	if _, ok := release.Tarballs[arch]; !ok {
		return "", fmt.Errorf("no Tailscale release found for architecture %s", arch)
	}

	logger.Info("Latest Tailscale version for %s: %s", arch, release.TarballsVersion)
	return release.TarballsVersion, nil
}

// DownloadTailscale downloads and extracts Tailscale binaries for the given version.
// Returns the directory containing the binaries.
func DownloadTailscale(version string) (string, error) {
	arch := getArch()
	destDir := filepath.Join(binCacheDir, version)

	// Check if already downloaded
	tailscalePath := filepath.Join(destDir, "tailscale")
	tailscaledPath := filepath.Join(destDir, "tailscaled")
	if fileExists(tailscalePath) && fileExists(tailscaledPath) {
		logger.Info("Tailscale %s already cached at %s", version, destDir)
		return destDir, nil
	}

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Download tarball
	url := fmt.Sprintf("%stailscale_%s_%s.tgz", stableBaseURL, version, arch)
	logger.Info("Downloading Tailscale from %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download Tailscale: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("tailscale version %s not found for %s", version, arch)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Extract tarball
	if err := extractTailscaleTarball(resp.Body, destDir); err != nil {
		// Clean up partial download
		os.RemoveAll(destDir)
		return "", fmt.Errorf("failed to extract Tailscale: %w", err)
	}

	// Verify binaries exist
	if !fileExists(tailscalePath) || !fileExists(tailscaledPath) {
		os.RemoveAll(destDir)
		return "", fmt.Errorf("extraction succeeded but binaries not found")
	}

	logger.Info("Tailscale %s installed to %s", version, destDir)
	return destDir, nil
}

// extractTailscaleTarball extracts tailscale and tailscaled from a .tgz archive.
func extractTailscaleTarball(r io.Reader, destDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	extracted := 0

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar: %w", err)
		}

		// Only extract tailscale and tailscaled binaries
		name := filepath.Base(header.Name)
		if name != "tailscale" && name != "tailscaled" {
			continue
		}

		// Skip directories
		if header.Typeflag == tar.TypeDir {
			continue
		}

		outPath := filepath.Join(destDir, name)
		outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create %s: %w", name, err)
		}

		if _, err := io.Copy(outFile, tr); err != nil {
			outFile.Close()
			return fmt.Errorf("failed to write %s: %w", name, err)
		}
		outFile.Close()

		extracted++
		logger.Debug("Extracted %s", name)
	}

	if extracted < 2 {
		return fmt.Errorf("expected 2 binaries, extracted %d", extracted)
	}

	return nil
}

// EnsureBinaries ensures Tailscale binaries are available.
// Returns paths to tailscale and tailscaled binaries.
//
// If customPath is set, it uses binaries from that directory.
// Otherwise, it downloads the specified version (or "latest").
func EnsureBinaries(requestedVersion, customPath string) (tailscale, tailscaled string, err error) {
	binaryMu.Lock()
	defer binaryMu.Unlock()

	// 1. Check for user-provided path override
	if customPath != "" {
		ts := filepath.Join(customPath, "tailscale")
		tsd := filepath.Join(customPath, "tailscaled")

		if !fileExists(ts) {
			return "", "", fmt.Errorf("tailscale binary not found at %s", ts)
		}
		if !fileExists(tsd) {
			return "", "", fmt.Errorf("tailscaled binary not found at %s", tsd)
		}

		logger.Info("Using custom Tailscale binaries from %s", customPath)
		return ts, tsd, nil
	}

	// 2. Resolve "latest" to actual version
	version := requestedVersion
	if version == "" || strings.ToLower(version) == "latest" {
		// Check if we already resolved latest in this session
		if cachedVersion != "" && cachedBinDir != "" {
			if fileExists(filepath.Join(cachedBinDir, "tailscale")) {
				logger.Debug("Using cached Tailscale %s", cachedVersion)
				return filepath.Join(cachedBinDir, "tailscale"),
					filepath.Join(cachedBinDir, "tailscaled"), nil
			}
		}

		v, err := FetchLatestVersion()
		if err != nil {
			return "", "", fmt.Errorf("failed to fetch latest version: %w", err)
		}
		version = v
	}

	// 3. Download if needed (or use cache)
	binDir, err := DownloadTailscale(version)
	if err != nil {
		return "", "", err
	}

	// Cache for future calls in this session
	cachedVersion = version
	cachedBinDir = binDir

	return filepath.Join(binDir, "tailscale"),
		filepath.Join(binDir, "tailscaled"), nil
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// MinServiceVersion is the minimum Tailscale version required for Services.
const MinServiceVersion = "1.86.0"

// GetInstalledVersion returns the version of the tailscale binary.
func GetInstalledVersion(tailscaleBin string) (string, error) {
	cmd := exec.Command(tailscaleBin, "version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get tailscale version: %w", err)
	}

	// Parse first line - format is just the version number like "1.86.0"
	lines := strings.Split(string(output), "\n")
	if len(lines) > 0 {
		version := strings.TrimSpace(lines[0])
		// Handle potential "v" prefix
		version = strings.TrimPrefix(version, "v")
		return version, nil
	}

	return "", fmt.Errorf("could not parse tailscale version output")
}

// CheckVersionForServices verifies the Tailscale version supports Services.
// Returns nil if version is sufficient, error otherwise.
func CheckVersionForServices(versionStr string) error {
	if versionStr == "" {
		return fmt.Errorf("unknown Tailscale version, cannot verify Services support")
	}

	// Normalize version string (remove 'v' prefix if present)
	versionStr = strings.TrimPrefix(versionStr, "v")

	current, err := semver.NewVersion(versionStr)
	if err != nil {
		return fmt.Errorf("failed to parse Tailscale version %s: %w", versionStr, err)
	}

	minimum, _ := semver.NewVersion(MinServiceVersion)

	if current.LessThan(minimum) {
		return fmt.Errorf("tailscale %s does not support services (minimum: %s)",
			versionStr, MinServiceVersion)
	}

	return nil
}
