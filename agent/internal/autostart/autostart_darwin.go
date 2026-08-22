package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
)

const label = "com.backupcenter.agent"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, label+".plist"), nil
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s/Library/Logs/BackupAgent.log</string>
	<key>StandardErrorPath</key>
	<string>%s/Library/Logs/BackupAgent.log</string>
</dict>
</plist>
`

// Ensure installs and loads a per-user LaunchAgent so the agent starts at
// login, in the user's GUI session (required to show the progress popup).
func Ensure(exePath string) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	content := fmt.Sprintf(plistTemplate, label, exePath, home, home)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}

	u, err := user.Current()
	uid := "staff"
	if err == nil {
		uid = u.Uid
	}
	target := "gui/" + uid + "/" + label
	_ = exec.Command("launchctl", "bootout", "gui/"+uid, path).Run() // ignore: may not be loaded yet
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err != nil {
		// Older macOS releases: fall back to the legacy subcommand.
		return exec.Command("launchctl", "load", "-w", path).Run()
	}
	_ = exec.Command("launchctl", "enable", target).Run()
	return nil
}

func Remove() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	u, _ := user.Current()
	uid := "staff"
	if u != nil {
		uid = u.Uid
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uid, path).Run()
	return os.Remove(path)
}
