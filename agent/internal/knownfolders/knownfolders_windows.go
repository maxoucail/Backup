//go:build windows

// Package knownfolders resolves where Desktop/Downloads/Documents/Pictures
// actually live for the current user. On Windows these are frequently
// moved off C: (a second internal drive, an external one) via "Location"
// in the folder's Properties dialog, which just rewrites a registry value
// - the folder's *name* stays the same but its real path doesn't follow
// the profile directory anymore. Reading that registry value is the only
// reliable way to find the real path; assuming <home>\Desktop would
// silently back up an empty (or wrong) folder for anyone who's redirected
// theirs.
package knownfolders

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"backup-agent/internal/userctx"
)

// UserSID, when set, makes Resolve read HKEY_USERS\<UserSID>\... instead of
// HKEY_CURRENT_USER. Set by main.go when running as a LocalSystem service:
// HKEY_CURRENT_USER inside that process is LocalSystem's own hive, not the
// console user's, so it would silently resolve nothing useful there.
var UserSID string

// registryName maps our folder names to the value name under
// User Shell Folders. Downloads has no classic name - only the CLSID form.
var registryName = map[string]string{
	"Desktop":   "Desktop",
	"Documents": "Personal",
	"Pictures":  "My Pictures",
	"Downloads": "{374DE290-123F-4565-9164-39C4925E467B}",
}

var procExpandEnvironmentStringsW = syscall.NewLazyDLL("kernel32.dll").NewProc("ExpandEnvironmentStringsW")

func expandEnv(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	src, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return s
	}
	// First call to get the required buffer size, second to fill it - the
	// standard two-pass pattern for this API.
	n, _, _ := procExpandEnvironmentStringsW.Call(uintptr(unsafe.Pointer(src)), 0, 0)
	if n == 0 {
		return s
	}
	buf := make([]uint16, n)
	procExpandEnvironmentStringsW.Call(uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&buf[0])), n)
	return syscall.UTF16ToString(buf)
}

// Resolve returns the real, current path of a well-known folder ("Desktop",
// "Downloads", "Documents", "Pictures"), following any redirection to
// another drive. Falls back to <home>\<name> if the registry lookup fails
// for any reason (fresh/unusual profile, restricted permissions, ...).
func Resolve(name string) string {
	home, err := userctx.HomeDir()
	if err != nil {
		home, _ = os.UserHomeDir()
	}
	fallback := filepath.Join(home, name)

	valueName, ok := registryName[name]
	if !ok {
		return fallback
	}

	root := registry.CURRENT_USER
	subKey := `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	if UserSID != "" {
		root = registry.USERS
		subKey = UserSID + `\` + subKey
	}

	k, err := registry.OpenKey(root, subKey, registry.QUERY_VALUE)
	if err != nil {
		return fallback
	}
	defer k.Close()

	raw, _, err := k.GetStringValue(valueName)
	if err != nil || raw == "" {
		return fallback
	}
	resolved := filepath.Clean(expandEnv(raw))
	if resolved == "" || resolved == "." {
		return fallback
	}
	return resolved
}
