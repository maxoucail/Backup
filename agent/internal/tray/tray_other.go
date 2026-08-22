//go:build !windows

package tray

import "errors"

// Run is Windows-only for now: a native macOS menu bar item needs CGO
// (NSStatusBar), which this project deliberately avoids so every binary -
// including the agent itself - can be cross-compiled from plain Linux
// without a Mac. See docs/ARCHITECTURE.md.
func Run(controlBase string) error {
	return errors.New("tray: uniquement disponible sur Windows pour le moment")
}
