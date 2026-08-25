//go:build !windows

package config

// ServiceLogDir is where a privileged background agent writes its log
// file. Only Windows needs a location different from the per-user Dir():
// macOS's LaunchDaemon already captures stdout/stderr machine-wide via
// launchd (see internal/macdaemon's /var/log/backup-agent.log), and Linux
// isn't installed as a privileged system service in this project at all.
func ServiceLogDir() (string, error) {
	return Dir()
}
