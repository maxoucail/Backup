// Package restorejob reconstructs a snapshot's files back onto disk,
// downloading only each distinct chunk once even if it's shared by
// several files.
//
// Files are put back by *logical* location: a file backed up from
// Downloads goes into this machine's Downloads folder, re-resolved once
// at the start of the restore (registry lookup on Windows) and reused for
// every file, so a folder redirected to another drive - here, or on the
// replacement machine a snapshot was moved to - still restores to the
// right place. See destinationFor for the full ordering, including the
// fallbacks for paths that never belonged to a well-known folder, and
// scanner.ResolveKnownFolders for why resolution happens once up front
// rather than per file.
package restorejob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-agent/internal/client"
	"backup-agent/internal/config"
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
	// Destinations are the distinct directories files were actually
	// written into, so the operator is told where the restore landed
	// instead of having to guess. "Restored 42 files" with no destination
	// is indistinguishable, from the outside, from a restore that wrote
	// into a folder nobody can see.
	Destinations []string
	// UsedFallbackPaths is true when live resolution of the user's folders
	// failed and the restore used the paths remembered from the last
	// successful backup. Worth surfacing: the files are in the right place,
	// but the machine is in a state (nobody logged in) the operator may
	// want to know about.
	UsedFallbackPaths bool
}

// Locations supplies where a restore may write. It's resolved once, up
// front, by the caller rather than per file - see scanner.ResolveKnownFolders.
type Locations struct {
	Home           string
	KnownFolders   map[string]string
	FromLastBackup bool
}

// ResolveLocations works out where this machine's user folders are, trying
// live resolution first and falling back to the paths remembered from the
// last successful backup.
//
// The fallback is the difference between a restore that works and one that
// disappears: resolving the logged-on user's profile from a service needs a
// usable session at that instant, and a restore triggered from the panel
// routinely arrives when there isn't one (lock screen, nobody logged in,
// RDP session still connecting). Without the fallback that case ends in
// the service account's own profile - a real directory the user will never
// look in - or in nothing at all.
func ResolveLocations(cfg *config.Config) (*Locations, error) {
	live := scanner.ResolveKnownFolders()
	home, homeErr := userctx.HomeDir()
	if homeErr != nil || home == "" || !filepath.IsAbs(home) || knownfolders.IsServiceProfilePath(home) {
		home = ""
	}

	// Live resolution is authoritative when it produced anything usable.
	if len(live) > 0 || home != "" {
		return &Locations{Home: home, KnownFolders: live}, nil
	}

	if cfg != nil && (len(cfg.LastKnownFolders) > 0 || cfg.LastKnownHome != "") {
		log.Print("restore: résolution des dossiers utilisateur impossible maintenant, utilisation des chemins de la dernière sauvegarde réussie")
		folders := make(map[string]string, len(cfg.LastKnownFolders))
		for name, p := range cfg.LastKnownFolders {
			// Cached paths get the same scrutiny as freshly resolved ones:
			// an agent build older than the service-profile guard could
			// have recorded SYSTEM's folders here, and replaying those
			// would recreate the very failure this fixes.
			if p == "" || !filepath.IsAbs(p) {
				continue
			}
			if knownfolders.IsServiceProfilePath(p) {
				log.Printf("restore: chemin mémorisé %q ignoré (profil d'un compte de service)", p)
				continue
			}
			folders[name] = p
		}
		cachedHome := cfg.LastKnownHome
		if knownfolders.IsServiceProfilePath(cachedHome) {
			cachedHome = ""
		}
		if len(folders) == 0 && cachedHome == "" {
			return nil, fmt.Errorf("impossible de déterminer les dossiers de l'utilisateur sur ce poste " +
				"(aucune session ouverte, et les chemins mémorisés sont inutilisables) : " +
				"ouvrez une session sur la machine, lancez une sauvegarde, puis relancez la restauration")
		}
		return &Locations{Home: cachedHome, KnownFolders: folders, FromLastBackup: true}, nil
	}

	return nil, fmt.Errorf("impossible de déterminer les dossiers de l'utilisateur sur ce poste " +
		"(aucune session ouverte et aucune sauvegarde précédente pour s'y référer) : " +
		"ouvrez une session sur la machine puis relancez la restauration")
}

func Run(ctx context.Context, c *client.Client, snapshotID string, loc *Locations, onProgress ProgressFunc) (*Result, error) {
	if onProgress == nil {
		onProgress = func(Progress) {}
	}
	if loc == nil {
		return nil, fmt.Errorf("emplacements de restauration non résolus")
	}
	onProgress(Progress{Phase: "fetching_manifest"})

	manifest, err := c.GetManifest(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("récupération du manifeste: %w", err)
	}
	home := loc.Home
	resolvedFolders := loc.KnownFolders

	var totalBytes int64
	for _, f := range manifest.Files {
		totalBytes += f.Size
	}

	var restoredBytes int64
	var restoredFiles int
	var skippedFiles []string
	var fatalErr error
	destinations := make(map[string]bool)
	var mu sync.Mutex
	start := time.Now()

	fileCh := make(chan protocol.ManifestFile)
	var wg sync.WaitGroup
	for w := 0; w < restoreConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				dest, err := restoreFile(ctx, c, home, resolvedFolders, f)
				if err == nil {
					mu.Lock()
					destinations[filepath.Dir(dest)] = true
					mu.Unlock()
				}
				if err != nil {
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
				// Snapshot the counters under the same lock that updates
				// them: reading restoredFiles again outside it (for the
				// progress message) is a data race, and with several
				// restore workers it can report a count that never existed.
				done, total, doneFiles := restoredBytes, totalBytes, restoredFiles
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
					Phase: "restoring", FileCount: doneFiles, TotalFiles: len(manifest.Files),
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

	dests := make([]string, 0, len(destinations))
	for d := range destinations {
		dests = append(dests, d)
	}
	sort.Strings(dests)
	log.Printf("restore: %d fichier(s) restauré(s) dans: %s", restoredFiles, strings.Join(dests, ", "))

	return &Result{
		FileCount: restoredFiles, Bytes: restoredBytes, SkippedFiles: skippedFiles,
		Destinations: dests, UsedFallbackPaths: loc.FromLastBackup,
	}, nil
}

// TopDestinations condenses the per-file destination directories into the
// handful of top-level folders worth showing an operator, so a restore of
// a deep tree reports "…\Downloads" rather than fifty subdirectories of it.
func (r *Result) TopDestinations(limit int) []string {
	roots := make([]string, 0, len(r.Destinations))
	for _, d := range r.Destinations {
		covered := false
		for _, existing := range roots {
			if d == existing || strings.HasPrefix(d, existing+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, d)
		}
	}
	if limit > 0 && len(roots) > limit {
		roots = roots[:limit]
	}
	return roots
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
//
// Returns an error if even the Desktop can't be resolved for this
// restore - at that point there's no honest place left to put the file,
// and the caller must report the file as failed rather than guess.
func fallbackDest(resolved map[string]string, f protocol.ManifestFile) (string, error) {
	desktop, ok := resolved["Desktop"]
	if !ok {
		return "", fmt.Errorf("impossible de déterminer le dossier de l'utilisateur (aucune session interactive détectée)")
	}
	relPath := filepath.FromSlash(f.Path)
	if strings.Contains(relPath, ":") {
		relPath = strings.ReplaceAll(relPath, ":", "")
	}
	return filepath.Join(desktop, relPath), nil
}

// destinationFor decides where a manifest entry lands on this machine, in
// order of preference:
//
//  1. Its well-known folder, from resolved (see scanner.ResolveKnownFolders)
//     - "Downloads/x.pdf" goes into this user's real Downloads, wherever
//     Windows says that is right now. This is the case that covers
//     essentially every backed-up file, and it deliberately ignores where
//     the file physically sat when it was backed up: that machine's
//     layout is not this machine's layout.
//  2. Its recorded original absolute path, when the entry has one and that
//     location is creatable - a configured path outside home, restored on
//     the same machine with the same drive still attached.
//  3. Home-relative, for an ordinary path that named no known folder -
//     only when home is itself a genuine absolute path, so a home
//     resolution that silently came back empty or relative can't produce
//     a destination that quietly writes next to the process instead of
//     into the user's files.
//  4. The Desktop fallback above.
//
// Returns an error only when none of the four produced anything usable -
// which the caller reports as this one file failing, rather than silently
// writing it somewhere the user will never find it.
func destinationFor(home string, resolved map[string]string, f protocol.ManifestFile) (string, error) {
	if dest, ok := scanner.KnownFolderDest(resolved, f.Path); ok {
		return dest, nil
	}
	if f.AbsPath != "" {
		if err := os.MkdirAll(filepath.Dir(f.AbsPath), 0o755); err == nil {
			return f.AbsPath, nil
		}
	}
	if !scanner.IsOutsideHome(f.Path) && !strings.Contains(f.Path, ":") && home != "" && filepath.IsAbs(home) {
		return filepath.Join(home, filepath.FromSlash(f.Path)), nil
	}
	return fallbackDest(resolved, f)
}

// restoreFile writes one manifest entry to disk and returns where it
// actually landed, so the caller can report real destinations rather than
// assumed ones.
func restoreFile(ctx context.Context, c *client.Client, home string, resolved map[string]string, f protocol.ManifestFile) (string, error) {
	dest, err := destinationFor(home, resolved, f)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}

	tmp := dest + ".restoring"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}

	var written int64
	for _, hash := range f.Chunks {
		r, err := c.DownloadChunk(ctx, hash)
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return "", err
		}
		n, copyErr := io.Copy(out, r)
		r.Close()
		written += n
		if copyErr != nil {
			out.Close()
			os.Remove(tmp)
			return "", copyErr
		}
	}
	// Flush to the filesystem before the rename: without it a crash or
	// power loss right after this point can leave a correctly-named file
	// with unwritten (zero) contents - the worst possible outcome for a
	// restore, since it looks like it worked.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// The manifest records the size the file had when backed up; a
	// mismatch means the reassembled content is not the file that was
	// backed up, so refuse to present it as a successful restore.
	if written != f.Size {
		os.Remove(tmp)
		return "", fmt.Errorf("taille restaurée incohérente (%d octets au lieu de %d)", written, f.Size)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	// Confirm the bytes really are at the destination. A restore that
	// reports success without the file being there is precisely the
	// failure this whole path exists to prevent.
	if info, err := os.Stat(dest); err != nil {
		return "", fmt.Errorf("fichier restauré introuvable après écriture: %w", err)
	} else if info.Size() != f.Size {
		return "", fmt.Errorf("fichier restauré incomplet (%d octets au lieu de %d)", info.Size(), f.Size)
	}
	modTime := time.Unix(f.ModTime, 0)
	_ = os.Chtimes(dest, modTime, modTime)
	return dest, nil
}
