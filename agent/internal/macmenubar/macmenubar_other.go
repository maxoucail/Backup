//go:build !darwin

package macmenubar

import "errors"

// Run exists only so cmd/backup-agent, a single file shared by every
// platform, can reference this package unconditionally; the darwin-only
// call site never actually reaches this on another OS.
func Run(controlBase string) error {
	return errors.New("macmenubar: non pris en charge sur cette plateforme")
}
