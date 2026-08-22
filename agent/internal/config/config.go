// Package config manages the agent's local, per-user configuration file:
// the paired server address and device identity, plus the last policy
// pushed from the panel. Stored per-user (not machine-wide) so the agent
// never needs elevated privileges to update it after enrollment.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	ServerURL       string   `json:"server_url"`
	DeviceID        string   `json:"device_id"`
	DeviceSecret    string   `json:"device_secret"`
	DeviceName      string   `json:"device_name"`
	IntervalMinutes int      `json:"interval_minutes"`
	RetentionCount  int      `json:"retention_count"`
	BackupPaths     []string `json:"backup_paths,omitempty"`
}

func (c *Config) Enrolled() bool {
	return c.ServerURL != "" && c.DeviceID != "" && c.DeviceSecret != ""
}

// Dir returns the per-user directory the agent stores its config and
// state cache in, creating it if necessary.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "BackupAgent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func Save(c *Config) error {
	p, err := path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ManifestCachePath is where the agent keeps a local copy of the last
// manifest it successfully uploaded, so routine backups can skip
// re-hashing unchanged files (matched by size+mtime) without round-tripping
// to the server first.
func ManifestCachePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "last_manifest.json"), nil
}
