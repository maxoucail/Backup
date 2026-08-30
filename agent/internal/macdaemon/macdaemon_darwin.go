//go:build darwin

// Package macdaemon runs the agent as a root LaunchDaemon: starts at boot,
// before anyone logs in, and can only be stopped by an administrator
// (launchctl/launchd require root to unload a system daemon) - the macOS
// equivalent of the Windows Service in package svcmode.
//
// A LaunchDaemon has no GUI session of its own, so showing the setup
// wizard or a progress popup requires the documented "asuser" trick:
// launchctl asuser <uid> sudo -u <user> <command> runs the command inside
// the console user's own session bootstrap, as that user, so windows it
// opens actually appear on screen.
package macdaemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	Label     = "com.backupcenter.agent"
	plistPath = "/Library/LaunchDaemons/" + Label + ".plist"
)

func IsRoot() bool { return os.Geteuid() == 0 }

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
	<string>/var/log/backup-agent.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/backup-agent.log</string>
</dict>
</plist>
`

// Install writes the LaunchDaemon plist and loads it. Must run as root
// (the install script uses sudo for exactly this).
func Install(exePath string) error {
	if !IsRoot() {
		return fmt.Errorf("droits root requis (sudo) pour installer le daemon")
	}
	content := fmt.Sprintf(plistTemplate, Label, exePath)
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", "system", plistPath).Run() // ignore: may not be loaded yet
	if err := exec.Command("launchctl", "bootstrap", "system", plistPath).Run(); err != nil {
		return exec.Command("launchctl", "load", "-w", plistPath).Run()
	}
	return nil
}

func Uninstall() error {
	if !IsRoot() {
		return fmt.Errorf("droits root requis (sudo) pour désinstaller le daemon")
	}
	_ = exec.Command("launchctl", "bootout", "system", plistPath).Run()
	// The menu bar helper (backup-agent --menubar) is a separate process
	// living in the console user's own session (see
	// LaunchInConsoleSession) - stopping the daemon above doesn't touch
	// it. Left running, it would keep showing the icon for a good five
	// minutes (its own unreachable-service timeout) after the actual
	// service is already gone. Root can signal any user's process
	// directly; no session-crossing trick is needed just to kill one.
	_ = exec.Command("pkill", "-f", "backup-agent --menubar").Run()
	return os.Remove(plistPath)
}

// ConsoleUser returns the username and uid of whoever is logged into the
// physical console (Screen Sharing / SSH sessions don't count), or an
// error if nobody is.
func ConsoleUser() (username string, uid int, err error) {
	out, err := exec.Command("stat", "-f%Su", "/dev/console").Output()
	if err != nil {
		return "", 0, err
	}
	username = strings.TrimSpace(string(out))
	if username == "" || username == "root" {
		return "", 0, fmt.Errorf("aucun utilisateur connecté à la console")
	}
	idOut, err := exec.Command("id", "-u", username).Output()
	if err != nil {
		return "", 0, err
	}
	uid, err = strconv.Atoi(strings.TrimSpace(string(idOut)))
	if err != nil {
		return "", 0, err
	}
	return username, uid, nil
}

// ConsoleUserHomeDir returns the console user's real home directory,
// asking the directory service for it rather than assuming the standard
// /Users/<name> layout - that assumption breaks for a directory-bound or
// relocated account, and a wrong home here means backing up (or restoring
// into) the wrong place entirely. Falls back to /Users/<name> only if
// dscl can't answer, which is the right guess when there's nothing better.
func ConsoleUserHomeDir() (string, error) {
	username, _, err := ConsoleUser()
	if err != nil {
		return "", err
	}
	if home := dsclHomeDir(username); home != "" {
		return home, nil
	}
	return filepath.Join("/Users", username), nil
}

// dsclHomeDir reads NFSHomeDirectory from the local directory service.
// Returns "" on any doubt - a caller with a bad path is worse than a
// caller that falls back to the conventional one.
func dsclHomeDir(username string) string {
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+username, "NFSHomeDirectory").Output()
	if err != nil {
		return ""
	}
	// Output is "NFSHomeDirectory: /path/to/home".
	_, value, found := strings.Cut(string(out), ":")
	if !found {
		return ""
	}
	home := strings.TrimSpace(value)
	if home == "" || home == "/var/empty" || !filepath.IsAbs(home) {
		return ""
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		return ""
	}
	return home
}

// LaunchInConsoleSession runs exePath with args as the console user,
// inside their GUI session, so anything it opens (a browser tab) is
// actually visible.
func LaunchInConsoleSession(exePath string, args []string) error {
	username, uid, err := ConsoleUser()
	if err != nil {
		return err
	}
	cmdArgs := append([]string{"asuser", strconv.Itoa(uid), "sudo", "-u", username, exePath}, args...)
	return exec.Command("launchctl", cmdArgs...).Start()
}
