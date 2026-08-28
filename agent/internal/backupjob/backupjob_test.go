package backupjob

import (
	"context"
	"encoding/json"
	"io"
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

// fakeServer stands in for the backup server: it records what the agent
// announced and what it actually uploaded, and can decide which uploads
// fail - the situation a real agent hits when a file is edited or locked
// mid-run.
type fakeServer struct {
	rejectContaining string // uploads whose bytes contain this are refused

	mu           sync.Mutex
	announced    []protocol.FileInfo
	uploaded     map[string]string // relative path -> content
	finishStatus string

	// neededFilter, when set, decides what the plan asks for. Nil means
	// "everything", as on a first backup.
	neededFilter func(protocol.FileInfo) bool
}

func (f *fakeServer) start(t *testing.T) *client.Client {
	t.Helper()
	f.uploaded = map[string]string{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/agent/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"snapshot_id": "snap-test"})
	})
	mux.HandleFunc("POST /api/agent/snapshots/{id}/plan", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Files []protocol.FileInfo `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.announced = req.Files
		f.mu.Unlock()

		needed := []string{}
		for _, file := range req.Files {
			if f.neededFilter == nil || f.neededFilter(file) {
				needed = append(needed, file.Path)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"needed": needed, "destination": "/mnt/nas/backups/PC-Test"})
	})
	mux.HandleFunc("PUT /api/agent/files", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if f.rejectContaining != "" && strings.Contains(string(body), f.rejectContaining) {
			http.Error(w, "écriture refusée", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.uploaded[r.URL.Query().Get("path")] = string(body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"bytes_written": len(body)})
	})
	mux.HandleFunc("POST /api/agent/snapshots/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.finishStatus = req.Status
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "dev1", "secret")
}

func (f *fakeServer) uploadedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.uploaded {
		out = append(out, p)
	}
	return out
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// The point of the whole rewrite: a backup must cost only what changed.
// If the agent re-uploads files the server already has, a machine with a
// large photo library re-sends gigabytes every single run.
func TestOnlyTheFilesTheServerAsksForAreUploaded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ancien.txt", "inchangé depuis la dernière sauvegarde")
	writeFile(t, dir, "nouveau.txt", "ce fichier vient d'être créé")

	srv := &fakeServer{
		neededFilter: func(f protocol.FileInfo) bool { return strings.HasSuffix(f.Path, "nouveau.txt") },
	}
	c := srv.start(t)

	res, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, nil)
	if err != nil {
		t.Fatalf("la sauvegarde a échoué: %v", err)
	}

	// Everything is announced - that's how the server knows what the
	// machine holds, and what it no longer holds.
	if len(srv.announced) != 2 {
		t.Fatalf("fichiers annoncés = %d, attendu 2", len(srv.announced))
	}
	paths := srv.uploadedPaths()
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "nouveau.txt") {
		t.Fatalf("fichiers envoyés = %v, attendu uniquement nouveau.txt", paths)
	}
	if res.FileCount != 2 {
		t.Fatalf("fichiers sauvegardés = %d, attendu 2 (le total protégé, pas seulement l'envoi)", res.FileCount)
	}
	if srv.finishStatus != protocol.SnapshotStatusSuccess {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusSuccess)
	}
}

// A machine where nothing changed must still report a successful backup:
// its copy on the NAS is genuinely up to date, and reporting a failure
// would train an operator to ignore failures.
func TestBackupSucceedsWhenServerNeedsNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "stable.txt", "rien n'a bougé")

	srv := &fakeServer{neededFilter: func(protocol.FileInfo) bool { return false }}
	c := srv.start(t)

	res, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, nil)
	if err != nil {
		t.Fatalf("une sauvegarde sans rien à envoyer doit réussir: %v", err)
	}
	if res.UploadedBytes != 0 {
		t.Fatalf("octets envoyés = %d, attendu 0", res.UploadedBytes)
	}
	if len(srv.uploadedPaths()) != 0 {
		t.Fatalf("aucun envoi attendu, obtenu %v", srv.uploadedPaths())
	}
	if srv.finishStatus != protocol.SnapshotStatusSuccess {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusSuccess)
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

	res, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, nil)
	if err != nil {
		t.Fatalf("la sauvegarde a échoué alors qu'un seul fichier posait problème: %v", err)
	}

	if len(res.SkippedFiles) != 1 {
		t.Fatalf("fichiers ignorés = %v, attendu exactement le fichier verrouillé", res.SkippedFiles)
	}
	if !strings.Contains(res.SkippedFiles[0], "boite-mail.pst") {
		t.Fatalf("fichier ignoré = %q, attendu boite-mail.pst", res.SkippedFiles[0])
	}
	if len(srv.uploadedPaths()) != 2 {
		t.Fatalf("fichiers envoyés = %v, attendu les 2 fichiers lisibles", srv.uploadedPaths())
	}
	if srv.finishStatus != protocol.SnapshotStatusSuccess {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusSuccess)
	}
}

// The panel's live progress bar depends on every progress callback after
// snapshot creation carrying the snapshot ID: the server only persists a
// progress update when it can attach it to a row (see ws.Hub.handleIncoming),
// so a callback missing SnapshotID is silently dropped rather than shown.
// Without it, the panel shows nothing until the finish call lands at the
// very end - the whole backup looking stuck at 0% until it suddenly
// completes.
func TestProgressCallbacksCarryTheSnapshotID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "un peu de contenu")
	writeFile(t, dir, "b.txt", "un peu plus de contenu pour forcer un vrai passage upload")

	srv := &fakeServer{}
	c := srv.start(t)

	var mu sync.Mutex
	var calls []Progress
	onProgress := func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, p)
	}

	if _, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, onProgress); err != nil {
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

// If nothing at all could be sent, reporting success would be a lie.
func TestBackupFailsWhenNoFileCouldBeSaved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "TOUT-ECHOUE un")
	writeFile(t, dir, "b.txt", "TOUT-ECHOUE deux")

	srv := &fakeServer{rejectContaining: "TOUT-ECHOUE"}
	c := srv.start(t)

	if _, err := Run(context.Background(), c, protocol.SnapshotKindManual, []scanner.Root{{Path: dir}}, nil); err == nil {
		t.Fatal("une sauvegarde dont aucun fichier n'a pu être envoyé doit échouer")
	}
	if srv.finishStatus != protocol.SnapshotStatusFailed {
		t.Fatalf("statut final = %q, attendu %q", srv.finishStatus, protocol.SnapshotStatusFailed)
	}
}
