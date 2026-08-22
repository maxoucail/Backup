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

// ConsoleUserHomeDir assumes the standard /Users/<name> convention (true
// for local accounts, the overwhelming common case on a personal or
// small-business Mac). A directory-bound account with a nonstandard home
// path isn't handled - go/user's pure-Go lookup on darwin isn't reliable
// without cgo, and this keeps the dependency-free build.
func ConsoleUserHomeDir() (string, error) {
	username, _, err := ConsoleUser()
	if err != nil {
		return "", err
	}
	return filepath.Join("/Users", username), nil
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
