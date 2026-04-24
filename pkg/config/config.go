package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultPath is where we try to read config from.
	DefaultPath = "/etc/etcd-walker/config.json"
)

// Config describes what can be set in JSON config.
type Config struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Protocol string `json:"protocol"` // "v2", "v3", "auto"
	Debug    bool   `json:"debug"`
	Username string `json:"username"`
	Password string `json:"password"`

	// TLS options (v3 only)
	TLSEnabled    bool   `json:"tls_enabled"`
	TLSCAFile     string `json:"tls_ca_file"`
	TLSCertFile   string `json:"tls_cert_file"`
	TLSKeyFile    string `json:"tls_key_file"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`

	// TimeoutSeconds for etcd operations (0 = default 5s)
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Load tries to read and unmarshal config from the given path.
// If the file does not exist, it returns (nil, nil).
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}

	finfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if finfo.IsDir() {
		return nil, fmt.Errorf("config path %s is a directory, expected file", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", filepath.Base(path), err)
	}
	return &cfg, nil
}
