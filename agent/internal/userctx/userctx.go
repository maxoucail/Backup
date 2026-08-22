// Package userctx resolves "the user whose files we're backing up".
//
// For a normal per-user process this is trivial (os.UserHomeDir). It stops
// being trivial once the agent runs as a system service (Windows Service
// under LocalSystem, macOS LaunchDaemon under root): os.UserHomeDir() would
// then return the service account's own profile, not the console user's -
// backing that up would silently back up nothing useful. HomeDir is a
// package-level override point: platform-specific service bootstrap code
// (see cmd/backup-agent) replaces it at startup when running under a
// system account; everything else in the agent calls userctx.HomeDir()
// instead of os.UserHomeDir() directly.
package userctx

import "os"

var HomeDir = os.UserHomeDir
