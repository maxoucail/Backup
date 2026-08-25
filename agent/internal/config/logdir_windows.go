//go:build windows

package config

import (
	"os"
	"path/filepath"
)

// ServiceLogDir is where the Windows Service writes its log file. It must
// not be the per-user Dir() this package otherwise uses for config: the
// service runs as LocalSystem, so os.UserConfigDir() there resolves to the
// SYSTEM account's own hidden profile - not anywhere an administrator
// troubleshooting the agent would ever think to look. ProgramData is the
// standard, world-readable location Windows services use for exactly this.
func ServiceLogDir() (string, error) {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	dir := filepath.Join(base, "BackupAgent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
