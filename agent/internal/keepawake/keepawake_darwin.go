package keepawake

import (
	"log"
	"os/exec"
)

// caffeinatePath is hardcoded rather than left to $PATH lookup: this
// agent runs as a root LaunchDaemon, which - unlike a normal user shell -
// has no guarantee of a populated PATH environment variable at all. This
// is exactly what silently defeated this whole package on a real machine:
// exec.Command("caffeinate", ...) failed to find it, Start() logged that
// and fell back to its no-op, and the backup went right on being
// interruptible by idle sleep despite the agent being fully up to date.
// caffeinate has lived at this exact path since its introduction in Mac
// OS X 10.8 and remains there through the current macOS releases.
const caffeinatePath = "/usr/bin/caffeinate"

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
	cmd := exec.Command(caffeinatePath, "-i", "-s")
	if err := cmd.Start(); err != nil {
		log.Printf("keepawake: caffeinate indisponible (%s), la mise en veille ne sera pas empêchée pendant la sauvegarde: %v", caffeinatePath, err)
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
