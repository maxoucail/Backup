//go:build windows

package filestore

import "os"

// fileIdentity has no portable equivalent on Windows without extra
// syscalls, and the server is deployed on Debian - so hard-linked files
// are simply counted every time here. Only affects the reported total,
// never what's stored.
func fileIdentity(os.FileInfo) (uint64, uint64, bool) { return 0, 0, false }
