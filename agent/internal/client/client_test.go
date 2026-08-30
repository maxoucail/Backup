package client

import (
	"net/http"
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
