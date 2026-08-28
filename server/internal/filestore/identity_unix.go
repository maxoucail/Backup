//go:build !windows

package filestore

import (
	"os"
	"syscall"
)

// fileIdentity exposes the inode number and link count so UsedBytes can
// count a file shared between versions by hard links exactly once. Without
// it, a machine with several versions would appear to use a multiple of
// what it really occupies on the NAS.
func fileIdentity(info os.FileInfo) (ino uint64, nlink uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, false
	}
	return st.Ino, uint64(st.Nlink), true
}
