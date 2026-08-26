//go:build !windows

package knownfolders

import (
	"errors"
	"path/filepath"

	"backup-agent/internal/userctx"
)

// UserSID is a no-op on this platform; it exists only so main.go can set
// it unconditionally without per-OS build tags of its own.
var UserSID string

var errEmptyHome = errors.New("répertoire utilisateur vide")

// Resolve on macOS/Linux: these platforms don't have Windows-style folder
// redirection for the handful of folders this agent watches by default, so
// the conventional <home>/<name> path is already correct. Returns "" if
// the current user's home can't be determined at all; see ResolveErr for
// why a low-stakes caller (the backup-time folder scan, which just skips
// what it can't find) can use this, and why a caller where getting it
// wrong is dangerous (restore) must use ResolveErr instead.
func Resolve(name string) string {
	p, err := ResolveErr(name)
	if err != nil {
		return ""
	}
	return p
}

// ResolveErr is like Resolve but reports failure instead of silently
// falling back to os.UserHomeDir(). That fallback matters here: running
// as a root LaunchDaemon, userctx.HomeDir is overridden to resolve the
// actual console user's home, and it can legitimately fail (nobody logged
// in at the console, mid-login/logout). os.UserHomeDir() would then
// resolve root's own home (/var/root) - a real, valid path that is not
// where any real user will ever look. Restoring a file there isn't a
// safe degradation, it's data quietly vanishing into a directory nobody
// will check; the caller needs to know resolution failed so it can fall
// back to somewhere honest (or skip the file and say why) instead.
func ResolveErr(name string) (string, error) {
	home, err := userctx.HomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errEmptyHome
	}
	return filepath.Join(home, name), nil
}
