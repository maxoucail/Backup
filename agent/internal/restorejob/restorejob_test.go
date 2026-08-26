package restorejob

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"backup-agent/internal/client"
	"backup-agent/internal/config"
	"backup-agent/internal/protocol"
)

// fakeServer stands in for the backup server: a fixed manifest, and one
// chunk hash that's simply missing (mirrors a corrupted/GC'd chunk on a
// real server) so a test can force exactly one file to fail.
type fakeServer struct {
	manifest      *protocol.Manifest
	missingChunks map[string]bool
}

func (f *fakeServer) start(t *testing.T) *client.Client {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/agent/snapshots/{id}/manifest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.manifest)
	})
	mux.HandleFunc("GET /api/agent/chunks/{hash}", func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if f.missingChunks[hash] {
			http.Error(w, "chunk introuvable", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("contenu-" + hash))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "dev1", "secret")
}

func manifestFile(path, hash string) protocol.ManifestFile {
	return protocol.ManifestFile{Path: path, Size: int64(len("contenu-" + hash)), Chunks: []string{hash}}
}

// liveLocations resolves against the test's fake HOME, standing in for a
// machine where a user session is available.
func liveLocations(t *testing.T) *Locations {
	t.Helper()
	loc, err := ResolveLocations(nil)
	if err != nil {
		t.Fatalf("résolution des emplacements: %v", err)
	}
	return loc
}

// One file whose chunk is gone from the server must not sink the restore
// of everything else - exactly the bug reported: a restore that aborted
// entirely over a single bad entry, discarding files that had already come
// back fine.
func TestRestorePartialFailureStillRestoresTheRest(t *testing.T) {
	home := t.TempDir()
	srv := &fakeServer{
		manifest: &protocol.Manifest{Files: []protocol.ManifestFile{
			manifestFile("Documents/bon.txt", "hash-ok"),
			manifestFile("Documents/casse.txt", "hash-missing"),
		}},
		missingChunks: map[string]bool{"hash-missing": true},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	result, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil)
	if err != nil {
		t.Fatalf("la restauration a échoué alors qu'un seul fichier posait problème: %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("fichiers restaurés = %d, attendu 1", result.FileCount)
	}
	if len(result.SkippedFiles) != 1 || result.SkippedFiles[0] != "Documents/casse.txt" {
		t.Fatalf("fichiers ignorés = %v, attendu [Documents/casse.txt]", result.SkippedFiles)
	}
	if _, err := os.Stat(filepath.Join(home, "Documents", "bon.txt")); err != nil {
		t.Fatalf("le fichier qui a réussi doit être présent sur disque: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "Documents", "casse.txt")); err == nil {
		t.Fatal("le fichier en échec ne doit pas exister (pas de contenu partiel/corrompu)")
	}
}

// If literally nothing could be restored, reporting success would be a
// lie - there'd be nothing to show for it.
func TestRestoreFailsWhenNothingCouldBeRestored(t *testing.T) {
	home := t.TempDir()
	srv := &fakeServer{
		manifest: &protocol.Manifest{Files: []protocol.ManifestFile{
			manifestFile("a.txt", "hash-a"),
			manifestFile("b.txt", "hash-b"),
		}},
		missingChunks: map[string]bool{"hash-a": true, "hash-b": true},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil); err == nil {
		t.Fatal("une restauration dont aucun fichier n'a pu revenir doit échouer")
	}
}

// A manifest written by an older, buggy agent could carry a raw absolute
// path (a Windows drive letter) as its "relative" path instead of a
// properly namespaced one. Restoring it must not try to create a literal
// "E:" directory - it should land somewhere sane instead of erroring out.
func TestRestoreSanitizesLegacyColonInPath(t *testing.T) {
	home := t.TempDir()
	srv := &fakeServer{
		manifest: &protocol.Manifest{Files: []protocol.ManifestFile{
			manifestFile(`E:/$RECYCLE.BIN/S-1-5-21-0/desktop.ini`, "hash-legacy"),
		}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	result, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil)
	if err != nil {
		t.Fatalf("la restauration d'un ancien manifeste avec un chemin absolu a échoué: %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("fichiers restaurés = %d, attendu 1", result.FileCount)
	}
}

// The headline behaviour: a file backed up from a well-known folder goes
// back into that folder as it exists on the restoring machine, even when
// the manifest also carries an AbsPath pointing somewhere else entirely
// (the folder's location on the machine that took the backup). Restoring
// to that stale physical path - or to a _outside fallback - is exactly
// what users hit as "I restored and my file didn't come back".
func TestRestorePutsKnownFolderFileBackInThatFolder(t *testing.T) {
	home := t.TempDir()
	staleLocation := filepath.Join(t.TempDir(), "Downloads") // "E:\Downloads" on the old machine

	f := manifestFile("Downloads/facture.pdf", "hash-known")
	f.AbsPath = filepath.Join(staleLocation, "facture.pdf")

	srv := &fakeServer{
		manifest:      &protocol.Manifest{Files: []protocol.ManifestFile{f}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil); err != nil {
		t.Fatalf("restauration échouée: %v", err)
	}

	want := filepath.Join(home, "Downloads", "facture.pdf")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("le fichier doit revenir dans le dossier Téléchargements de cet utilisateur (%s): %v", want, err)
	}
	if _, err := os.Stat(f.AbsPath); err == nil {
		t.Fatal("le fichier ne doit pas être écrit à l'emplacement physique de l'ancienne machine")
	}
}

// A file backed up from outside home (a manually configured root on
// another drive) must come back at its real original location on a
// same-machine restore, not get silently relocated under home every time -
// that's what an operator restoring right after deleting the file expects
// to see, and where AbsPath exists precisely so restore can deliver it.
func TestRestoreUsesOriginalAbsPathWhenAvailable(t *testing.T) {
	home := t.TempDir()
	otherDrive := t.TempDir() // stands in for a separate drive, e.g. "E:\"
	originalPath := filepath.Join(otherDrive, "Projets", "rapport.docx")

	f := manifestFile("_outside/E/Projets/rapport.docx", "hash-abs")
	f.AbsPath = originalPath

	srv := &fakeServer{
		manifest:      &protocol.Manifest{Files: []protocol.ManifestFile{f}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil); err != nil {
		t.Fatalf("restauration échouée: %v", err)
	}

	if _, err := os.Stat(originalPath); err != nil {
		t.Fatalf("le fichier doit être restauré à son emplacement d'origine %s: %v", originalPath, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Desktop", "_outside", "E", "Projets", "rapport.docx")); err == nil {
		t.Fatal("le fichier ne doit pas être dupliqué/relocalisé sur le Bureau alors que son emplacement d'origine est disponible")
	}
}

// When the original drive genuinely isn't available (a different machine,
// or the drive is gone), restore must still succeed by falling back to a
// clearly-visible folder on the Desktop instead of losing the file or
// burying it deep in the profile. Blocking the original path with a plain
// file (not a permission error, which a root-run test wouldn't hit) makes
// MkdirAll fail for a structural reason that holds regardless of
// privileges.
func TestRestoreFallsBackWhenAbsPathUnavailable(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := manifestFile("_outside/E/Projets/rapport.docx", "hash-abs2")
	f.AbsPath = filepath.Join(blocker, "Projets", "rapport.docx")

	srv := &fakeServer{
		manifest:      &protocol.Manifest{Files: []protocol.ManifestFile{f}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	result, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil)
	if err != nil {
		t.Fatalf("restauration échouée alors qu'un repli était possible: %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("fichiers restaurés = %d, attendu 1", result.FileCount)
	}
	if _, err := os.Stat(filepath.Join(home, "Desktop", "_outside", "E", "Projets", "rapport.docx")); err != nil {
		t.Fatalf("le fichier doit atterrir dans le repli sur le Bureau: %v", err)
	}
}

// The scenario behind "it restores into the void": a restore triggered
// from the panel while nobody is logged in at the machine. Live
// resolution of the user's folders yields nothing, and the paths
// remembered from the last successful backup must take over so the files
// still land in the real user's folders.
func TestRestoreUsesLastKnownFoldersWhenNobodyIsLoggedIn(t *testing.T) {
	realUserHome := t.TempDir()

	// No live resolution available at all - what a service sees with no
	// usable session.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	loc, err := ResolveLocations(&config.Config{
		LastKnownHome: realUserHome,
		LastKnownFolders: map[string]string{
			"Downloads": filepath.Join(realUserHome, "Downloads"),
			"Desktop":   filepath.Join(realUserHome, "Desktop"),
		},
	})
	if err != nil {
		t.Fatalf("les chemins de la dernière sauvegarde doivent servir de repli: %v", err)
	}
	if !loc.FromLastBackup {
		t.Fatal("le repli doit être signalé comme tel pour pouvoir l'indiquer à l'opérateur")
	}

	srv := &fakeServer{
		manifest: &protocol.Manifest{Files: []protocol.ManifestFile{
			manifestFile("Downloads/facture.pdf", "hash-fallback"),
		}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)

	result, err := Run(t.Context(), c, "snap-test", loc, nil)
	if err != nil {
		t.Fatalf("restauration échouée: %v", err)
	}
	want := filepath.Join(realUserHome, "Downloads", "facture.pdf")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("le fichier doit atterrir dans les Téléchargements du vrai utilisateur (%s): %v", want, err)
	}
	if !result.UsedFallbackPaths {
		t.Fatal("le résultat doit indiquer que les chemins de secours ont été utilisés")
	}
}

// With no session AND no previous backup to fall back on, there is no
// honest destination. Refusing outright - with an actionable message - is
// the only correct outcome: writing the files anywhere else is what made
// them disappear in the first place.
func TestResolveLocationsFailsLoudlyWithNothingToGoOn(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	if _, err := ResolveLocations(&config.Config{}); err == nil {
		t.Fatal("sans session ni sauvegarde précédente, la restauration doit échouer explicitement")
	} else if !strings.Contains(err.Error(), "session") {
		t.Fatalf("le message doit expliquer quoi faire, obtenu: %v", err)
	}
}

// A restore must be able to say where it put things. "42 files restored"
// with no destination is exactly as useless as the silent failure.
func TestRestoreReportsWhereFilesLanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	srv := &fakeServer{
		manifest: &protocol.Manifest{Files: []protocol.ManifestFile{
			manifestFile("Downloads/a.pdf", "hash-r1"),
			manifestFile("Downloads/sous-dossier/b.pdf", "hash-r2"),
		}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)

	result, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil)
	if err != nil {
		t.Fatalf("restauration échouée: %v", err)
	}
	if len(result.Destinations) == 0 {
		t.Fatal("le résultat doit indiquer où les fichiers ont été écrits")
	}
	// The nested directory must collapse into its parent for reporting.
	tops := result.TopDestinations(4)
	wantRoot := filepath.Join(home, "Downloads")
	if len(tops) != 1 || tops[0] != wantRoot {
		t.Fatalf("destinations résumées = %v, attendu [%s]", tops, wantRoot)
	}
}

// A truncated or mismatched reassembly must never be presented as a
// restored file: silently leaving a wrong-sized file in place of the real
// one is worse than reporting the failure.
func TestRestoreRejectsFileWithWrongSize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	f := manifestFile("Documents/tronque.txt", "hash-short")
	f.Size += 100 // manifest claims more bytes than the chunk will deliver

	srv := &fakeServer{
		manifest:      &protocol.Manifest{Files: []protocol.ManifestFile{f}},
		missingChunks: map[string]bool{},
	}
	c := srv.start(t)

	if _, err := Run(t.Context(), c, "snap-test", liveLocations(t), nil); err == nil {
		t.Fatal("un fichier reconstitué à la mauvaise taille ne doit pas compter comme restauré")
	}
	if _, err := os.Stat(filepath.Join(home, "Documents", "tronque.txt")); err == nil {
		t.Fatal("aucun fichier incomplet ne doit rester sur le disque")
	}
}
