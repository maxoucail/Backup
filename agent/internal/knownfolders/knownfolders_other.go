//go:build !windows

package knownfolders

import (
	"os"
	"path/filepath"

	"backup-agent/internal/userctx"
)

// UserSID is a no-op on this platform; it exists only so main.go can set
// it unconditionally without per-OS build tags of its own.
var UserSID string

// Resolve on macOS/Linux: these platforms don't have Windows-style folder
// redirection for the handful of folders this agent watches by default, so
// the conventional <home>/<name> path is already correct.
func Resolve(name string) string {
	home, err := userctx.HomeDir()
	if err != nil {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, name)
}
