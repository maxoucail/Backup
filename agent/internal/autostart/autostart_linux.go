package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

// Ensure installs an XDG autostart entry. Linux desktops aren't a primary
// packaging target for this project (Windows/macOS are), but the agent is
// plain Go and runs fine there too, which is also what makes local
// development and testing of the agent possible without Windows/macOS
// hardware.
func Ensure(exePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "autostart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Backup Agent
Exec=%s
X-GNOME-Autostart-enabled=true
`, exePath)
	return os.WriteFile(filepath.Join(dir, "backup-agent.desktop"), []byte(content), 0o644)
}

func Remove() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(home, ".config", "autostart", "backup-agent.desktop"))
}
