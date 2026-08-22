package autostart

import "os/exec"

const taskName = "BackupAgent"

// Ensure registers the agent to start automatically when the current user
// logs on, via Task Scheduler. Per-user (not a Windows Service) so it runs
// in the interactive session and can show the progress popup - a service
// running as SYSTEM cannot draw on the user's desktop on modern Windows.
func Ensure(exePath string) error {
	tr := `"` + exePath + `"`
	cmd := exec.Command("schtasks", "/Create", "/TN", taskName, "/TR", tr, "/SC", "ONLOGON", "/RL", "LIMITED", "/F")
	return cmd.Run()
}

func Remove() error {
	return exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").Run()
}
