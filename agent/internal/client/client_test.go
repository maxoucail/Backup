package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// uploadClient must let connections be reused (keep-alives on - the
// zero-value default, but worth asserting so a future edit doesn't
// silently reintroduce DisableKeepAlives) and must be able to hold onto
// enough idle ones for every concurrent upload worker to keep its own
// warm between files - otherwise most of them still pay for a fresh TCP
// handshake (and its RTT, worse over a routed/VPN path) per file despite
// keep-alives nominally being on, since Go's default MaxIdleConnsPerHost
// of 2 would evict the rest immediately. maxKnownUploadConcurrency
// mirrors backupjob.uploadConcurrency (can't import it directly:
// backupjob already imports this package) - bump both together if that
// value changes.
const maxKnownUploadConcurrency = 8

func TestUploadClientReusesConnectionsAcrossAllWorkers(t *testing.T) {
	tr, ok := uploadClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("uploadClient.Transport = %T, attendu *http.Transport", uploadClient.Transport)
	}
	if tr.DisableKeepAlives {
		t.Fatal("uploadClient ne doit plus désactiver les keep-alives : le vrai problème était sendfile, pas la réutilisation de connexion")
	}
	if tr.MaxIdleConnsPerHost < maxKnownUploadConcurrency {
		t.Fatalf("MaxIdleConnsPerHost = %d, attendu au moins %d pour que chaque worker garde sa connexion au chaud",
			tr.MaxIdleConnsPerHost, maxKnownUploadConcurrency)
	}
}

// This is the exact failure reported from a real Mac: uploads reliably
// failed with errors literally prefixed "sendfile: ..." (broken pipe,
// socket is not connected) - Go's own internal/poll naming the syscall it
// used, not this agent's error text. net.TCPConn.ReadFrom recognizes an
// *os.File source via a type assertion and hands it to the kernel's
// sendfile() for a zero-copy send; that recognition is exactly what
// noReaderFrom must defeat, since Darwin's sendfile() has long-standing
// kernel bugs under concurrent use that Linux and Windows don't share -
// the same agent binary, same upload code, fails only on macOS. Checked
// here at the one point that actually matters: type-asserting the
// wrapped reader back to *os.File must fail, exactly what stops Go's
// runtime from choosing that path, while Read must still delegate
// correctly to the real file underneath.
func TestNoReaderFromHidesTheFileTypeFromSendfileDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	var wrapped io.Reader = noReaderFrom{f}
	if _, ok := wrapped.(*os.File); ok {
		t.Fatal("noReaderFrom laisse passer le type *os.File - le contournement du bug sendfile de macOS ne fonctionnerait pas")
	}

	data, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("contenu lu = %q, attendu %q (le wrapper doit rester un simple passe-plat pour Read)", data, "hello")
	}
}

// The exact bug reported from a real device with 35000+ files: the server
// rewrites this device's whole file index before answering /finish (see
// filestore.ConfirmUpdates), which on a slow NAS took longer than the
// previous 30-second client timeout - the agent gave up and logged the
// run as failed while the server went on to actually mark it successful.
// A generous timeout must never be the reason a genuinely completed
// backup gets reported as an error.
func TestFinishSnapshotTimeoutSurvivesASlowServerResponse(t *testing.T) {
	if finishSnapshotTimeout < 5*time.Minute {
		t.Fatalf("finishSnapshotTimeout = %v, trop court pour un gros index réécrit sur un NAS lent", finishSnapshotTimeout)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands in for a slow ConfirmUpdates on a real NAS - short enough
		// to keep the test fast, but well past the old 30-second timeout's
		// failure mode if that regressed.
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":"true"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "dev1", "secret")
	if err := c.FinishSnapshot(context.Background(), "snap1", "success", "", 0); err != nil {
		t.Fatalf("FinishSnapshot: %v (une réponse tardive mais réelle ne doit jamais être prise pour un échec)", err)
	}
}
