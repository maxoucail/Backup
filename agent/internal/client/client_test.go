package client

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// UploadFile must never reuse a pooled keep-alive connection across
// separate uploads. This is the exact shape of a real field failure: a
// large backup with several concurrent uploads, one of which dies
// mid-transfer (a network blip, a server restart). If the next upload on
// that worker reused the same pooled connection, it can land on one
// net/http hasn't fully recognized as dead yet - and a perfectly normal
// request reads back as a nonsensical error (a stray 405) that has
// nothing to do with it. Checked directly on uploadClient's configuration
// rather than by observing connection reuse over a real socket: Go's
// http.Response.Body.Close() opportunistically drains and reuses a
// connection for a small enough response regardless of this setting, so a
// behavioral test against a local httptest server doesn't reliably tell
// the two configurations apart - the setting itself is what actually
// governs behavior against a real, larger, unpredictable network path.
func TestUploadClientDisablesKeepAlives(t *testing.T) {
	tr, ok := uploadClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("uploadClient.Transport = %T, attendu *http.Transport", uploadClient.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Fatal("uploadClient doit désactiver les keep-alives (DisableKeepAlives = true)")
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
