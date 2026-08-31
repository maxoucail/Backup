package keepawake

import (
	"log"
	"os/exec"
)

// Start holds a "caffeinate" child process alive for the duration of the
// backup - the same mechanism every "keep my Mac awake" utility uses.
// caffeinate ships with every macOS install since 10.8, so this needs no
// extra permission, dependency, or CGO.
//
//   - -i: prevent idle system sleep.
//   - -s: prevent idle system sleep specifically while on AC power (a
//     laptop running on battery can still sleep if the operator wants that -
//     this only stops the machine deciding on its own that a background
//     backup counts as "idle").
func Start() (stop func()) {
	cmd := exec.Command("caffeinate", "-i", "-s")
	if err := cmd.Start(); err != nil {
		log.Printf("keepawake: caffeinate indisponible, la mise en veille ne sera pas empêchée pendant la sauvegarde: %v", err)
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
