package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A JSON reply magpie writes ends at the last byte of its document, with
// no trailing newline. Codex 0.167's models reader refuses a body with
// anything after the JSON — "failed to decode models response: Data at
// line 1 column 20460 (body: 20461 bytes)", every refresh, its list never
// shown — and the one byte over was the newline json.Encoder appends.
// curl, python and jq all tolerate trailing whitespace, so only a check of
// the raw bytes catches it.
func TestJSONReplyHasNoTrailingNewline(t *testing.T) {
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
		// an error body goes through writeJSON too: it must end at the
		// document as well, or a strict reader chokes on the 404 the same
		// way it did on the list
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
			if b[len(b)-1] == '\n' {
				t.Fatalf("%s: body ends in a newline (%d bytes, last %q); a strict JSON reader refuses it", c.path, len(b), b[len(b)-1:])
			}
			if !json.Valid(b) {
				t.Fatalf("%s: body isn't valid JSON: %q", c.path, b)
			}
		})
	}
}
