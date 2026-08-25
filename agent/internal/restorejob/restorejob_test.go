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
