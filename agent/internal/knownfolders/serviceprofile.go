package knownfolders

import (
	"path/filepath"
	"strings"
)

// serviceProfileMarkers identify the built-in Windows service accounts'
// own profile directories.
//
// A user folder must never resolve inside one. Those are the paths a
// wrong-hive registry read produces - a LocalSystem service reading
// HKEY_CURRENT_USER gets SYSTEM's own folders - and they are invisible to
// the logged-in user, often not even browsable without elevation. A
// restore that writes there reports success while the files effectively
// disappear, which is the exact failure this guard exists to stop.
var serviceProfileMarkers = []string{
	`\config\systemprofile`,
	`\serviceprofiles\localservice`,
	`\serviceprofiles\networkservice`,
}

// IsServiceProfilePath reports whether p lives inside a service account's
// profile.
//
// Deliberately not build-tagged to Windows: these are path *patterns*, not
// OS behaviour, and a genuine Unix path can't accidentally match one.
// Keeping it cross-platform means the restore path can apply the same
// refusal everywhere - including to paths cached by an older agent build
// that recorded them before this guard existed - and that the guard is
// testable off Windows.
func IsServiceProfilePath(p string) bool {
	if p == "" {
		return false
	}
	normalised := strings.ToLower(strings.ReplaceAll(filepath.ToSlash(p), "/", `\`))
	for _, marker := range serviceProfileMarkers {
		if strings.Contains(normalised, marker) {
			return true
		}
	}
	return false
}
