package osinfo

import (
	"os/exec"
	"strings"
)

func Version() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
