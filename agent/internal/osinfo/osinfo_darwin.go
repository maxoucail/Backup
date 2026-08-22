package osinfo

import (
	"os/exec"
	"strings"
)

func Version() string {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return "macOS " + strings.TrimSpace(string(out))
}
