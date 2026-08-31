//go:build !windows && !darwin

package keepawake

// Start is a no-op on platforms this agent doesn't target as a desktop
// (or where the caller is a test running on the developer's own Linux
// machine, which was never going to sleep mid-test anyway).
func Start() (stop func()) {
	return func() {}
}
