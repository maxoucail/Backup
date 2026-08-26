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
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"backup-agent/internal/userctx"
)

// UserSID, when set, makes Resolve read HKEY_USERS\<UserSID>\... instead
// of HKEY_CURRENT_USER. Set by main.go when running as a LocalSystem
// service: HKEY_CURRENT_USER inside that process is LocalSystem's own
// hive, not the console user's, so it would silently resolve that account's
// folders instead - which is how restored files ended up in
// C:\Windows\System32\config\systemprofile where nobody could find them.
var UserSID string

// UserSIDFunc, when set, re-resolves the console user's SID on demand.
//
// A one-shot UserSID set at startup is not enough, and getting this wrong
// is exactly what sent restores into the void: the service starts at boot,
// when nobody is logged in yet, so the initial lookup fails and UserSID
// stays empty for the entire life of the service. Every later registry
// read then silently falls back to HKEY_CURRENT_USER - the SYSTEM hive -
// and returns SYSTEM's folder paths, no matter who logged in afterwards.
// Re-resolving per lookup means the first user to log in after boot is
// picked up straight away.
var UserSIDFunc func() (string, error)

// currentUserSID prefers a freshly resolved SID, falling back to whatever
// was captured at startup.
func currentUserSID() string {
	if UserSIDFunc != nil {
		if sid, err := UserSIDFunc(); err == nil && sid != "" {
			return sid
		}
	}
	return UserSID
}

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

var errEmptyHome = errors.New("répertoire utilisateur vide")

// Resolve returns the real, current path of a well-known folder ("Desktop",
// "Downloads", "Documents", "Pictures"), following any redirection to
// another drive. Returns "" if the current user's home can't be determined
// at all; see ResolveErr for why a low-stakes caller (the backup-time
// folder scan, which just skips what it can't find) can use this, and why
// a caller where getting it wrong is dangerous (restore) must use
// ResolveErr instead.
func Resolve(name string) string {
	p, err := ResolveErr(name)
	if err != nil {
		return ""
	}
	return p
}

// ResolveErr is like Resolve but reports failure instead of silently
// falling back to os.UserHomeDir(). That fallback matters here: running as
// a Windows Service under LocalSystem, userctx.HomeDir is overridden to
// resolve the actual console user's home via their session token, and
// that lookup can legitimately fail (nobody logged in at the console,
// mid-logon/logoff, a WTS query racing a session change). os.UserHomeDir()
// would then resolve LocalSystem's own profile
// (C:\Windows\System32\config\systemprofile) - a real, valid path that is
// not where any real user will ever look, and one most users can't even
// browse to. Restoring a file there isn't a safe degradation, it's data
// quietly vanishing into a directory nobody will check; the caller needs
// to know resolution failed so it can fall back to somewhere honest (or
// skip the file and say why) instead.
//
// The registry lookup itself still falls back to <home>\<name> when it
// fails (fresh/unusual profile, restricted permissions, no redirection
// configured) - that part is a reasonable default once home is known to
// be the *real* user's home, not a substitute for a wrong one.
func ResolveErr(name string) (string, error) {
	home, err := userctx.HomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errEmptyHome
	}
	fallback := filepath.Join(home, name)

	valueName, ok := registryName[name]
	if !ok {
		return fallback, nil
	}

	// Read the *user's* hive whenever we know which one it is. Falling
	// through to HKEY_CURRENT_USER inside a LocalSystem service reads
	// SYSTEM's own hive and yields SYSTEM's folders, so only do that when
	// there's genuinely no user SID - and even then, the guard below
	// catches the bad answer.
	root := registry.CURRENT_USER
	subKey := `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	if sid := currentUserSID(); sid != "" {
		root = registry.USERS
		subKey = sid + `\` + subKey
	}

	k, err := registry.OpenKey(root, subKey, registry.QUERY_VALUE)
	if err != nil {
		return fallback, nil
	}
	defer k.Close()

	raw, _, err := k.GetStringValue(valueName)
	if err != nil || raw == "" {
		return fallback, nil
	}
	resolved := filepath.Clean(expandEnv(raw))
	if resolved == "" || resolved == "." {
		return fallback, nil
	}
	// Last line of defence against a wrong-hive read: the registry just
	// told us this user's folder lives inside a service account's profile.
	// It doesn't - that answer came from SYSTEM's hive - and honouring it
	// is precisely what makes restored files vanish somewhere the user
	// can't see. The user's own profile is the trustworthy answer here.
	if IsServiceProfilePath(resolved) && !IsServiceProfilePath(home) {
		return fallback, nil
	}
	return resolved, nil
}
