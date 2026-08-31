package client

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
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
