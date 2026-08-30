//go:build windows

package filestore

import "fmt"

// freeBytes has no portable equivalent here without extra syscalls
// (GetDiskFreeSpaceExW), and the server is deployed on Debian - so this
// just reports that the figure isn't available rather than guessing.
func freeBytes(root string) (int64, error) {
	return 0, fmt.Errorf("espace disque disponible non pris en charge sur cette plateforme")
}
