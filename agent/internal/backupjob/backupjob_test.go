package backupjob

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"backup-agent/internal/client"
	"backup-agent/internal/protocol"
	"backup-agent/internal/scanner"
)

// fakeServer stands in for the backup server, letting a test decide which
// chunk uploads fail - the situation a real agent hits when a file is
// edited or locked between hashing and upload.
type fakeServer struct {
	rejectContaining string // uploads of chunks for files whose bytes contain this are refused
	manifest         *protocol.Manifest
	finishStatus     string
}

func (f *fakeServer) start(t *testing.T) *client.Client {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/agent/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"snapshot_id": "snap-test"})
	})
	mux.HandleFunc("POST /api/agent/snapshots/{id}/check-chunks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hashes []string `json:"hashes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Server has nothing yet: every chunk needs uploading.
		_ = json.NewEncoder(w).Encode(map[string][]string{"missing": req.Hashes})
	})
	mux.HandleFunc("PUT /api/agent/chunks/{hash}", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		if f.rejectContaining != "" && strings.Contains(string(body[:n]), f.rejectContaining) {
			// Mirrors the real server refusing content that doesn't match
			// the announced hash, i.e. a file that changed under us.
			http.Error(w, "hash mismatch", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"bytes_written": n})
	})
	mux.HandleFunc("POST /api/agent/snapshots/{id}/manifest", func(w http.ResponseWriter, r *http.Request) {
		var m protocol.Manifest
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.manifest = &m
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})
	mux.HandleFunc("POST /api/agent/snapshots/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.finishStatus = req.Status
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "dev1", "secret")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// The whole point of a backup agent is that it runs on a machine someone
// is actively using. One file being edited, locked or deleted mid-run must
// cost that one file - not the backup of everything else.
func TestBackupSkipsUnreadableFileAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "rapport.txt", "contenu stable")
	writeFile(t, dir, "boite-mail.pst", "FICHIER-VERROUILLE en cours de modification")
	writeFile(t, dir, "photo.jpg", "autre contenu stable")

	srv := &fakeServer{rejectContaining: "FICHIER-VERROUILLE"}
	c := srv.start(t)

	// Isolate the manifest cache from the developer's real one.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	res, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, 16*1024*1024, nil)
	if err != nil {
		t.Fatalf("la sauvegarde a échoué alors qu'un seul fichier posait problème: %v", err)
	}

	if len(res.SkippedFiles) != 1 {
		t.Fatalf("fichiers ignorés = %v, attendu exactement le fichier verrouillé", res.SkippedFiles)
	}
	if !strings.Contains(res.SkippedFiles[0], "boite-mail.pst") {
		t.Fatalf("fichier ignoré = %q, attendu boite-mail.pst", res.SkippedFiles[0])
	}
	if res.FileCount != 2 {
		t.Fatalf("fichiers sauvegardés = %d, attendu 2", res.FileCount)
	}
	if srv.finishStatus != protocol.SnapshotStatusSuccess {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusSuccess)
	}

	// The manifest must not reference the file whose data never landed,
	// otherwise a restore would fail on it.
	for _, f := range srv.manifest.Files {
		if strings.Contains(f.Path, "boite-mail.pst") {
			t.Fatal("le manifeste référence un fichier dont les données n'ont pas été envoyées")
		}
	}
}

// The panel's live progress bar depends on every progress callback after
// snapshot creation carrying the snapshot ID: the server only persists a
// progress update when it can attach it to a row (see ws.Hub.handleIncoming),
// so a callback missing SnapshotID is silently dropped rather than shown.
// Without it, the panel shows nothing until the manifest and finish calls
// land at the very end - the whole backup looking stuck at 0% until it
// suddenly completes.
func TestProgressCallbacksCarryTheSnapshotID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "un peu de contenu")
	writeFile(t, dir, "b.txt", "un peu plus de contenu pour forcer un vrai passage upload")

	srv := &fakeServer{}
	c := srv.start(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var mu sync.Mutex
	var calls []Progress
	onProgress := func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, p)
	}

	if _, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, 16*1024*1024, onProgress); err != nil {
		t.Fatalf("la sauvegarde a échoué: %v", err)
	}

	sawPostCreation := false
	for _, p := range calls {
		if p.Phase == "scanning" {
			continue // before CreateSnapshot: there's genuinely no ID yet
		}
		sawPostCreation = true
		if p.SnapshotID == "" {
			t.Fatalf("callback de phase %q sans SnapshotID: le serveur ne pourra pas l'afficher en direct", p.Phase)
		}
	}
	if !sawPostCreation {
		t.Fatal("aucun callback de progression après la création de la sauvegarde: le test ne prouve rien")
	}
}

// If nothing at all could be uploaded, reporting success would be a lie -
// the snapshot would restore to an empty set.
func TestBackupFailsWhenNoFileCouldBeSaved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "TOUT-ECHOUE un")
	writeFile(t, dir, "b.txt", "TOUT-ECHOUE deux")

	srv := &fakeServer{rejectContaining: "TOUT-ECHOUE"}
	c := srv.start(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, 16*1024*1024, nil); err == nil {
		t.Fatal("une sauvegarde dont aucun fichier n'a pu être envoyé doit échouer")
	}
	if srv.finishStatus != protocol.SnapshotStatusFailed {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusFailed)
	}
}
