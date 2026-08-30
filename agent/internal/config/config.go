// Package config manages the agent's local, per-user configuration file:
// the paired server address and device identity, plus the last policy
// pushed from the panel. Stored per-user (not machine-wide) so the agent
// never needs elevated privileges to update it after enrollment.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"backup-agent/internal/userctx"
)

type Config struct {
	ServerURL       string   `json:"server_url"`
	DeviceID        string   `json:"device_id"`
	DeviceSecret    string   `json:"device_secret"`
	DeviceName      string   `json:"device_name"`
	IntervalMinutes int      `json:"interval_minutes"`
	RetentionCount  int      `json:"retention_count"`
	BackupPaths     []string `json:"backup_paths,omitempty"`

	// NextScheduledAt is when the next routine scheduled backup is due.
	// Compared against the current time at startup to detect a backup that
	// was missed because the machine was off - see the reschedule wizard.
	NextScheduledAt *time.Time `json:"next_scheduled_at,omitempty"`
	// PendingCatchUpAt is a one-off makeup time the user picked after a
	// missed backup was detected. Cleared once that catch-up run happens.
	PendingCatchUpAt *time.Time `json:"pending_catch_up_at,omitempty"`
	// CatchUpNotifiedT15/T5 track whether the T-15min/T-5min heads-up for
	// PendingCatchUpAt has already fired, so a restart doesn't repeat them.
	CatchUpNotifiedT15 bool `json:"catch_up_notified_t15,omitempty"`
	CatchUpNotifiedT5  bool `json:"catch_up_notified_t5,omitempty"`
}

func (c *Config) Enrolled() bool {
	return c.ServerURL != "" && c.DeviceID != "" && c.DeviceSecret != ""
}

// Dir returns the per-user directory the agent stores its config and
// state cache in, creating it if necessary.
//
// Built from userctx.HomeDir() rather than os.UserConfigDir(): the latter
// reads $HOME/%AppData% straight from the process environment, which a
// privileged system service (Windows Service under LocalSystem, macOS
// LaunchDaemon under root) doesn't have set to anything useful - on macOS
// it's simply unset, and os.UserConfigDir() hard-fails with "$HOME is not
// defined" instead of falling back to anything. userctx.HomeDir is exactly
// the override point built for this: main.go points it at the console
// user's real home when running as a service, so this resolves to the
// same place either way.
func Dir() (string, error) {
	home, err := userctx.HomeDir()
	if err != nil {
		return "", err
	}
	var base string
	switch runtime.GOOS {
	case "windows":
		base = filepath.Join(home, "AppData", "Roaming")
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support")
	default:
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

// Clear wipes the local device identity so the agent starts fresh through
// the setup wizard on next use - used when the server reports this device
// is no longer recognized (decommissioned).
func Clear() error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Left over from the pre-plain-files protocol; removed here so a
	// decommissioned machine doesn't keep a stale index around forever.
	if dir, err := Dir(); err == nil {
		_ = os.Remove(filepath.Join(dir, "last_manifest.json"))
	}
	return nil
}
