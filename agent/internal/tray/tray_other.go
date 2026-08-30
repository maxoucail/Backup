//go:build !windows

package tray

import "errors"

// Run is Windows-only: this package drives Shell_NotifyIconW directly.
// macOS has its own equivalent, internal/macmenubar, since the two
// platforms' native icon APIs have nothing in common to share here.
func Run(controlBase string) error {
	return errors.New("tray: uniquement disponible sur Windows")
}

// KillRunningHelper is a no-op here so cmd/backup-agent, a single file
// shared by every platform, can call it unconditionally; the
// Windows-only call site never actually reaches this on another OS.
func KillRunningHelper() {}
