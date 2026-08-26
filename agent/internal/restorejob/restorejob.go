// Package restorejob reconstructs a snapshot's files back onto disk,
// downloading only each distinct chunk once even if it's shared by
// several files.
//
// Files are put back by *logical* location: a file backed up from
// Downloads goes into this machine's Downloads folder, re-resolved at
// restore time (registry lookup on Windows), so a folder redirected to
// another drive - here, or on the replacement machine a snapshot was
// moved to - still restores to the right place. See destinationFor for
// the full ordering, including the fallbacks for paths that never
// belonged to a well-known folder.
package restorejob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"backup-agent/internal/client"
	"backup-agent/internal/knownfolders"
	"backup-agent/internal/protocol"
	"backup-agent/internal/scanner"
	"backup-agent/internal/userctx"
)

const restoreConcurrency = 4

type Progress struct {
	Phase         string // fetching_manifest / restoring
	FileCount     int
	TotalFiles    int
	LogicalBytes  int64
	RestoredBytes int64
	Percent       float64
	EtaSeconds    int64
}

type ProgressFunc func(Progress)

type Result struct {
	FileCount int
	Bytes     int64
	// SkippedFiles are files that couldn't be restored (missing chunk,
	// disk error writing that one file...) while the rest of the snapshot
	// came back fine. A restore is a live, usable filesystem the moment it
	// finishes - failing the whole thing over one bad file would throw
	// away every other file that restored correctly for no reason.
	SkippedFiles []string
}

func Run(ctx context.Context, c *client.Client, snapshotID string, onProgress ProgressFunc) (*Result, error) {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	onProgress(Progress{Phase: "fetching_manifest"})

	manifest, err := c.GetManifest(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("récupération du manifeste: %w", err)
	}
	home, err := userctx.HomeDir()
	if err != nil {
		return nil, fmt.Errorf("répertoire utilisateur introuvable: %w", err)
	}

	var totalBytes int64
	for _, f := range manifest.Files {
		totalBytes += f.Size
	}

	var restoredBytes int64
	var restoredFiles int
	var skippedFiles []string
	var fatalErr error
	var mu sync.Mutex
	start := time.Now()

	fileCh := make(chan protocol.ManifestFile)
	var wg sync.WaitGroup
	for w := 0; w < restoreConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				if err := restoreFile(ctx, c, home, f); err != nil {
					// Credentials revoked mid-restore: every remaining
					// chunk fetch would fail too, so stop rather than
					// grind through the whole manifest for nothing.
					if errors.Is(err, client.ErrUnauthorized) {
						mu.Lock()
						if fatalErr == nil {
							fatalErr = err
						}
						mu.Unlock()
						continue
					}
					log.Printf("restore: fichier ignoré %s: %v", f.Path, err)
					mu.Lock()
					skippedFiles = append(skippedFiles, f.Path)
					mu.Unlock()
					continue
				}
				mu.Lock()
				restoredBytes += f.Size
				restoredFiles++
				done, total := restoredBytes, totalBytes
				mu.Unlock()

				elapsed := time.Since(start).Seconds()
				var eta int64
				if elapsed > 0.5 && done > 0 && total > done {
					rate := float64(done) / elapsed
					if rate > 0 {
						eta = int64(float64(total-done) / rate)
					}
				}
				pct := 100.0
				if total > 0 {
					pct = 100 * float64(done) / float64(total)
				}
				onProgress(Progress{
					Phase: "restoring", FileCount: restoredFiles, TotalFiles: len(manifest.Files),
					LogicalBytes: total, RestoredBytes: done, Percent: pct, EtaSeconds: eta,
				})
			}
		}()
	}

loop:
	for _, f := range manifest.Files {
		select {
		case fileCh <- f:
		case <-ctx.Done():
			break loop
		}
	}
	close(fileCh)
	wg.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if fatalErr != nil {
		return nil, fatalErr
	}
	if restoredFiles == 0 && len(skippedFiles) > 0 {
		return nil, fmt.Errorf("aucun fichier n'a pu être restauré (%d en échec)", len(skippedFiles))
	}
	return &Result{FileCount: restoredFiles, Bytes: restoredBytes, SkippedFiles: skippedFiles}, nil
}

// fallbackDest is the last-resort destination for a file that has no
// logical folder to go back to and no usable original location - an
// operator-configured path outside home whose drive isn't present on this
// machine. Landing it deep inside the profile would make it easy to lose
// track of; a clearly-named folder on the Desktop, mirroring the original
// location as a subfolder path (e.g.
// Desktop/_outside/E/Projets/rapport.docx), is somewhere the user will
// actually see it and can file away by hand.
//
// Also defends against a manifest written by an older, buggier agent that
// could carry a raw absolute path (e.g. a Windows drive letter, "E:") as
// its "relative" path outright: joining that onto anything would try to
// create a literal "E:" directory, which every Windows API rejects.
func fallbackDest(f protocol.ManifestFile) string {
	relPath := filepath.FromSlash(f.Path)
	if strings.Contains(relPath, ":") {
		relPath = strings.ReplaceAll(relPath, ":", "")
	}
	return filepath.Join(knownfolders.Resolve("Desktop"), relPath)
}

// destinationFor decides where a manifest entry lands on this machine, in
// order of preference:
//
//  1. Its well-known folder, re-resolved here and now - "Downloads/x.pdf"
//     goes into this user's real Downloads, wherever Windows says that is.
//     This is the case that covers essentially every backed-up file, and
//     it deliberately ignores where the file physically sat when it was
//     backed up: that machine's layout is not this machine's layout.
//  2. Its recorded original absolute path, when the entry has one and that
//     location is creatable - a configured path outside home, restored on
//     the same machine with the same drive still attached.
//  3. Home-relative, for an ordinary path that named no known folder.
//  4. The Desktop fallback above.
func destinationFor(home string, f protocol.ManifestFile) string {
	if dest, ok := scanner.KnownFolderDest(f.Path); ok {
		return dest
	}
	if f.AbsPath != "" {
		if err := os.MkdirAll(filepath.Dir(f.AbsPath), 0o755); err == nil {
			return f.AbsPath
		}
	}
	if scanner.IsOutsideHome(f.Path) || strings.Contains(f.Path, ":") {
		return fallbackDest(f)
	}
	return filepath.Join(home, filepath.FromSlash(f.Path))
}

func restoreFile(ctx context.Context, c *client.Client, home string, f protocol.ManifestFile) error {
	dest := destinationFor(home, f)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	tmp := dest + ".restoring"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	for _, hash := range f.Chunks {
		r, err := c.DownloadChunk(ctx, hash)
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		_, copyErr := io.Copy(out, r)
		r.Close()
		if copyErr != nil {
			out.Close()
			os.Remove(tmp)
			return copyErr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	modTime := time.Unix(f.ModTime, 0)
	_ = os.Chtimes(dest, modTime, modTime)
	return nil
}
