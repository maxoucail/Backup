// Package scanner enumerates the files a backup run should consider:
// either the operator's default set of well-known user folders (Desktop,
// Downloads, Documents, Pictures - present under the user's home directory
// on Windows, macOS and Linux alike) or a custom list pushed from the
// panel.
package scanner

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

var defaultFolderNames = []string{"Desktop", "Downloads", "Documents", "Pictures"}

// DefaultRoots returns the standard per-user folders to back up.
// Non-existent folders (e.g. a fresh account with no Pictures folder yet)
// are silently skipped rather than treated as an error.
func DefaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var roots []string
	for _, name := range defaultFolderNames {
		p := filepath.Join(home, name)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// ResolveRoots turns the operator-configured path list (which may contain
// bare folder names like "Desktop" relative to the home directory, or full
// absolute paths for something like an external project folder) into
// absolute paths. An empty list means "use the defaults".
func ResolveRoots(configured []string) []string {
	if len(configured) == 0 {
		return DefaultRoots()
	}
	home, _ := os.UserHomeDir()
	var roots []string
	for _, p := range configured {
		if filepath.IsAbs(p) {
			roots = append(roots, filepath.Clean(p))
			continue
		}
		roots = append(roots, filepath.Join(home, p))
	}
	return roots
}

type FileEntry struct {
	AbsPath string
	RelPath string // relative to the user's home directory, forward-slash separated
	Size    int64
	ModTime int64 // unix seconds
}

// Walk enumerates every regular file under the given roots. Errors on
// individual entries (permission denied, a file vanishing mid-walk) are
// logged and skipped rather than aborting the whole backup - a single
// locked file should never fail an otherwise-successful run.
func Walk(roots []string) []FileEntry {
	home, _ := os.UserHomeDir()
	seen := make(map[string]bool)
	var out []FileEntry

	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				log.Printf("scanner: skipping %s: %v", p, err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil // skip symlinks, sockets, devices, etc.
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				return nil
			}
			if seen[abs] {
				return nil // overlapping configured roots
			}
			seen[abs] = true

			info, err := d.Info()
			if err != nil {
				log.Printf("scanner: stat failed for %s: %v", p, err)
				return nil
			}

			rel := abs
			if home != "" {
				if r, err := filepath.Rel(home, abs); err == nil && !isOutside(r) {
					rel = r
				}
			}

			out = append(out, FileEntry{
				AbsPath: abs,
				RelPath: filepath.ToSlash(rel),
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
			})
			return nil
		})
	}
	return out
}

func isOutside(rel string) bool {
	return len(rel) >= 2 && rel[0] == '.' && rel[1] == '.'
}
