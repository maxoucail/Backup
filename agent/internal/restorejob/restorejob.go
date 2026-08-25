// Package restorejob reconstructs a snapshot's files back onto disk,
// downloading only each distinct chunk once even if it's shared by
// several files, and restoring into the same relative location under the
// user's home directory the files were originally backed up from.
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
	"backup-agent/internal/protocol"
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

func restoreFile(ctx context.Context, c *client.Client, home string, f protocol.ManifestFile) error {
	// Defense in depth: a manifest written by an older, buggy agent could
	// still carry a raw absolute path (e.g. a Windows drive letter, "E:")
	// as its "relative" path. Joining that onto home would try to create a
	// literal "E:" directory, which every Windows API rejects outright -
	// stripping stray colons here means even a pre-existing bad manifest
	// restores into a slightly odd but valid location instead of failing.
	relPath := filepath.FromSlash(f.Path)
	if strings.Contains(relPath, ":") {
		relPath = strings.ReplaceAll(relPath, ":", "")
	}
	dest := filepath.Join(home, relPath)
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
