//go:build !windows

package filestore

import "syscall"

// freeBytes reports how much space remains on the filesystem backing
// root - a single statfs() call, not a walk, so it stays cheap enough to
// call on a fast cadence (see scheduler.refreshStorageFree).
func freeBytes(root string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return 0, err
	}
	// Bavail, not Bfree: space available to an unprivileged user, which is
	// what actually limits how much more this store can hold. Bfree also
	// counts space the kernel reserves for root, which a backup running as
	// its own system user could never actually use.
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
