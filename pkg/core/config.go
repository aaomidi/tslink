package core

import (
	"os"
)

// Config holds the plugin configuration.
type Config struct {
	AuthKey   string
	DataDir   string
	TSVersion string // Tailscale version to download ("latest" or specific version)
	TSPath    string // Optional path to user-provided Tailscale binaries
}

// NetworkOptions holds options for network creation.
type NetworkOptions struct {
	AuthKey string
}

// EndpointOptions holds options for endpoint creation.
type EndpointOptions struct {
	Hostname string
}

// LoadConfig loads configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		DataDir:   os.Getenv("TS_DATA_DIR"),
		TSVersion: os.Getenv("TS_VERSION"),
		TSPath:    os.Getenv("TS_PATH"),
	}

	if cfg.DataDir == "" {
		cfg.DataDir = "/data"
	}

	if cfg.TSVersion == "" {
		cfg.TSVersion = "latest"
	}

	return cfg, nil
}

// GenericOptionsKey is the key Docker uses to pass driver options.
const GenericOptionsKey = "com.docker.network.generic"

// ParseNetworkOptions parses network creation options.
// Docker passes --opt values nested under "com.docker.network.generic".
func ParseNetworkOptions(opts map[string]any) NetworkOptions {
	options := NetworkOptions{}

	genericOpts := opts
	if nested, ok := opts[GenericOptionsKey]; ok {
		if nestedMap, ok := nested.(map[string]any); ok {
			genericOpts = nestedMap
		}
	}

	if v, ok := genericOpts["tslink.authkey"]; ok {
		if s, ok := v.(string); ok {
			options.AuthKey = s
		}
	}

	return options
}

// ParseEndpointOptions parses endpoint creation options.
func ParseEndpointOptions(opts map[string]any) EndpointOptions {
	options := EndpointOptions{}

	if v, ok := opts["tslink.hostname"]; ok {
		if s, ok := v.(string); ok {
			options.Hostname = s
		}
	}

	return options
}
