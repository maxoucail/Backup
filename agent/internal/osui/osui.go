// Package osui provides the two small cross-platform, CGO-free ways the
// agent reaches the user's screen: opening a local URL in the default
// browser (used for both the first-run setup wizard and the live
// backup/restore progress view) and firing a native "heads up" OS
// notification. Deliberately not a custom native GUI toolkit - shelling
// out to the OS's own opener and notifier is far more reliable across
// Windows/macOS/Linux than bundling a GUI stack.
package osui

import (
	"os/exec"
	"runtime"
)

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		// rundll32 avoids the quoting pitfalls of "cmd /c start".
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// Notify fires a best-effort native OS notification. Failures are not
// fatal - the browser popup this always accompanies is the primary UI.
func Notify(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		script := `display notification ` + quoteAppleScript(message) + ` with title ` + quoteAppleScript(title)
		_ = exec.Command("osascript", "-e", script).Start()
	case "windows":
		// msg.exe has shipped with every Windows desktop release since XP;
		// it pops a small system dialog in the current user's session,
		// which a background process cannot do with a normal message box.
		_ = exec.Command("msg.exe", "*", "/TIME:10", title+": "+message).Start()
	default:
		_ = exec.Command("notify-send", title, message).Start()
	}
}

func quoteAppleScript(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}
