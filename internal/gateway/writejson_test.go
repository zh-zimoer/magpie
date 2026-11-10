package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A JSON reply magpie writes names its length and ends at the last byte of
// its document. Both matter to Codex 0.167, whose reader is reqwest:
//
//   - No Content-Length. json.NewEncoder(w) streams, so with no length
//     named Go answers a "Connection: close" request close-delimited — the
//     body ends where the connection does. reqwest reads the list as a
//     stream and takes that close for a broken stream: "stream
//     disconnected before completion: failed to decode models response",
//     every refresh, its list never shown. A turn answered the same way
//     reaches the agent as "EOF while parsing a value at line 1 column 0".
//     curl, python and jq read to EOF and tolerate it, so the server
//     looked healthy from every angle but the client's. writeJSON now
//     marshals first and sends Content-Length, which ends the body
//     unambiguously for every reader.
//   - No trailing newline. json.Encoder appends one after the JSON; a
//     strict reader refuses a body with any byte after the document.
func TestJSONReplyNamesItsLengthAndEndsAtItsDocument(t *testing.T) {
	codexSignedIn(t)
	if err := provider.Save(provider.Provider{ID: "codex", Models: []string{"gpt-5.6-sol"}}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		path string
		code int
	}{
		{"list", "/v1/models", 200},
		// an error body goes through writeJSON too: it names its length
		// and ends at its document as the list does
		{"unknown model", "/v1/models/no-such-model", 404},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
			if rec.Code != c.code {
				t.Fatalf("%s: %d, want %d (%s)", c.path, rec.Code, c.code, rec.Body.String())
			}
			b := rec.Body.Bytes()
			if len(b) == 0 {
				t.Fatalf("%s: empty body", c.path)
			}
			// the length is named, so the body isn't close-delimited and a
			// streaming reader knows where it ends without the connection
			// closing
			cl := rec.Header().Get("Content-Length")
			if cl == "" {
				t.Fatalf("%s: no Content-Length; a close-delimited body reads as a broken stream to reqwest", c.path)
			}
			if n, err := strconv.Atoi(cl); err != nil || n != len(b) {
				t.Fatalf("%s: Content-Length %q, body %d bytes", c.path, cl, len(b))
			}
			if b[len(b)-1] == '\n' {
				t.Fatalf("%s: body ends in a newline (%d bytes); a strict JSON reader refuses it", c.path, len(b))
			}
			if !json.Valid(b) {
				t.Fatalf("%s: body isn't valid JSON: %q", c.path, b)
			}
		})
	}
}
