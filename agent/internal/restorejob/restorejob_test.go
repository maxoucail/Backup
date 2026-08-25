package restorejob

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"backup-agent/internal/client"
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

	result, err := Run(t.Context(), c, "snap-test", nil)
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

	if _, err := Run(t.Context(), c, "snap-test", nil); err == nil {
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

	result, err := Run(t.Context(), c, "snap-test", nil)
	if err != nil {
		t.Fatalf("la restauration d'un ancien manifeste avec un chemin absolu a échoué: %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("fichiers restaurés = %d, attendu 1", result.FileCount)
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

	if _, err := Run(t.Context(), c, "snap-test", nil); err != nil {
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

	result, err := Run(t.Context(), c, "snap-test", nil)
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
