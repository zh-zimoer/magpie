package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// restPerModelCandidate is one model on a subscription account, on a
// provider with rest-per-model as asked.
func restPerModelCandidate(model string, perModel bool) candidate {
	return candidate{
		p: provider.Provider{ID: "codex", RestPerModel: perModel,
			Account: &provider.Account{Agent: "codex", User: "me@example.com"}},
		model: model, rest: "codex@me@example.com",
	}
}

// forgetRests starts and leaves nobody resting, for a test of rests alone.
func forgetRests(t *testing.T) {
	t.Helper()
	clear := func() {
		restingUntil.Lock()
		restingUntil.m, restingUntil.note = map[string]time.Time{}, map[string]Rest{}
		restingUntil.Unlock()
		routed.Lock()
		routed.failures = map[string]int{}
		routed.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// A capability failure on a provider set rest-per-model rests that model
// on the account alone (bulkhead): the account's other models are still
// asked. A 401, a quota and an overload rest as they ever did: the first
// two the account, the last the model already.
func TestRestPerModelNarrowsCapabilityFailures(t *testing.T) {
	forgetRests(t)
	s := &Server{}
	rate := []byte(`{"error":{"message":"Too many requests","type":"rate_limit_error"}}`)
	boom := []byte(`{"error":{"message":"An error occurred while processing your request.","type":"server_error"}}`)

	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"429", http.StatusTooManyRequests, rate},
		{"500", http.StatusInternalServerError, boom},
		{"503", http.StatusServiceUnavailable, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forgetRests(t)
			c := restPerModelCandidate("gpt-6-astra", true)
			if r := s.restAfter(c, tc.status, http.Header{}, tc.body); r.Key != c.restID() || r.Model != c.model {
				t.Fatalf("rests %s (model %q), want the model %s", r.Key, r.Model, c.restID())
			}
			if _, ok := restOf("codex@me@example.com"); ok {
				t.Fatal("the account rests whole for one model's failure")
			}
		})
	}

	t.Run("401 stays the account's", func(t *testing.T) {
		forgetRests(t)
		c := restPerModelCandidate("gpt-6-astra", true)
		r := s.restAfter(c, http.StatusUnauthorized, http.Header{},
			[]byte(`{"error":{"message":"Your authentication token has expired.","code":"token_expired"}}`))
		if r.Key != c.restKey() {
			t.Fatalf("a 401 rests %s, want the account %s", r.Key, c.restKey())
		}
	})
	t.Run("a usage limit stays the account's", func(t *testing.T) {
		forgetRests(t)
		c := restPerModelCandidate("gpt-6-astra", true)
		r := s.restAfter(c, http.StatusTooManyRequests, http.Header{},
			[]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
		if r.Why != failQuota || r.Key != c.restKey() {
			t.Fatalf("a usage limit rests %s as %s, want the account as %s", r.Key, r.Why, failQuota)
		}
	})
	t.Run("off by default", func(t *testing.T) {
		forgetRests(t)
		c := restPerModelCandidate("gpt-6-astra", false)
		if r := s.restAfter(c, http.StatusTooManyRequests, http.Header{}, rate); r.Key != c.restKey() {
			t.Fatalf("a plain provider rests %s, want the account %s as before", r.Key, c.restKey())
		}
	})
}

// Two of one account's models failing capability failures within
// bulkheadWindow merges the rests to the account (escalateRests): a key
// truly turned away says so by failing everywhere. An overload never
// merges: two models' 529s are one capacity moment, not a refusal.
func TestRestPerModelEscalatesOnSecondModel(t *testing.T) {
	forgetRests(t)
	s := &Server{}
	rate := []byte(`{"error":{"message":"Too many requests","type":"rate_limit_error"}}`)

	c1 := restPerModelCandidate("gpt-6-astra", true)
	if r := s.restAfter(c1, http.StatusTooManyRequests, http.Header{}, rate); r.Key != c1.restID() {
		t.Fatalf("the first model rests %s, want %s", r.Key, c1.restID())
	}
	if _, ok := restOf("codex@me@example.com"); ok {
		t.Fatal("one model's 429 escalated to the account already")
	}

	c2 := restPerModelCandidate("gpt-6-sol", true)
	r := s.restAfter(c2, http.StatusTooManyRequests, http.Header{}, rate)
	if r.Key != "codex@me@example.com" {
		t.Fatalf("the second model's 429 rests %s, want the account", r.Key)
	}
	esc, ok := restOf("codex@me@example.com")
	if !ok || esc.Model != "" {
		t.Fatalf("the account's rest: %+v", esc)
	}
	// the models' own rests are gone, merged into the account's
	if _, ok := restOf(c1.restID()); ok {
		t.Fatal("the first model still rests alone after the merge")
	}
	if _, ok := restOf(c2.restID()); ok {
		t.Fatal("the second model still rests alone after the merge")
	}
}

func TestRestPerModelOverloadsNeverEscalate(t *testing.T) {
	forgetRests(t)
	s := &Server{}
	over := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`)

	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-5.6-sol"} {
		c := restPerModelCandidate(model, true)
		if r := s.restAfter(c, 529, http.Header{}, over); r.Key != c.restID() {
			t.Fatalf("%s rests %s for an overload, want its model", model, r.Key)
		}
	}
	if _, ok := restOf("codex@me@example.com"); ok {
		t.Fatal("three models' overloads escalated to the account: an overload says nothing of it")
	}
}

// A hard window the vendor named (x-retry-metadata: NO_MORE_RETRY) holds
// the overload's rest to it, up to overloadHardRestMost: resting a minute
// asks again into a sure refusal, a dozen times over the episode.
func TestOverloadNoMoreRetryHoldsTheWindow(t *testing.T) {
	forgetRests(t)
	s := &Server{}
	c := restPerModelCandidate("gpt-6-astra", false) // the switch is the bulkhead's; this is every provider's

	// The ChatGPT backend puts retry-after inside the error body's
	// headers, not the HTTP headers — so this test sends NO HTTP
	// Retry-After, the shape production actually has. A past version of
	// this test set the HTTP header and so passed while the real path
	// (retryAfterInBody) was never exercised.
	nmr := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY","retry-after":"301","retry-after-ms":"301000"},"message":"Our servers are currently overloaded. Please try again later."}}`)

	r := s.restAfter(c, 529, http.Header{}, nmr)
	if r.By != "retry-after" {
		t.Fatalf("rests by %s, want retry-after", r.By)
	}
	// 301s is over the 3-minute cap, so it caps at overloadHardRestMost.
	if d := time.Until(r.Until); d < overloadHardRestMost-time.Second || d > overloadHardRestMost+time.Second {
		t.Fatalf("a 301s NO_MORE_RETRY window rests %v, want capped at %v", d.Round(time.Second), overloadHardRestMost)
	}

	// a window the vendor named under the cap is held to as named
	forgetRests(t)
	short := []byte(`{"error":{"code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY","retry-after":"90"},"message":"overloaded"}}`)
	r2 := s.restAfter(c, 529, http.Header{}, short)
	if d := time.Until(r2.Until); d < 88*time.Second || d > 92*time.Second {
		t.Fatalf("a 90s NO_MORE_RETRY window rests %v, want ~90s", d.Round(time.Second))
	}

	// retry-after-ms alone is read too
	forgetRests(t)
	msonly := []byte(`{"error":{"code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY","retry-after-ms":"120000"},"message":"overloaded"}}`)
	r3 := s.restAfter(c, 529, http.Header{}, msonly)
	if d := time.Until(r3.Until); d < 118*time.Second || d > 122*time.Second {
		t.Fatalf("a 120000ms NO_MORE_RETRY window rests %v, want ~2m", d.Round(time.Second))
	}

	// NO_MORE_RETRY with no window named at all: the vendor said stop but
	// not for how long — hold to the cap rather than the 30s cooldown
	forgetRests(t)
	nowin := []byte(`{"error":{"code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY"},"message":"overloaded"}}`)
	r4 := s.restAfter(c, 529, http.Header{}, nowin)
	if d := time.Until(r4.Until); d < overloadHardRestMost-time.Second || d > overloadHardRestMost+time.Second {
		t.Fatalf("a NO_MORE_RETRY with no window rests %v, want the cap %v", d.Round(time.Second), overloadHardRestMost)
	}

	// a plain overload, no NO_MORE_RETRY: unchanged — the 30s cooldown,
	// even though its body names a 6s retry-after (that 6s must NOT
	// shorten the cooldown and cause more wall-hitting)
	forgetRests(t)
	plain := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"retry-after":"6"},"message":"Our servers are currently overloaded. Please try again later."}}`)
	r5 := s.restAfter(c, 529, http.Header{}, plain)
	if r5.By != "cooldown" || time.Until(r5.Until) > overloadRest+time.Second || time.Until(r5.Until) < overloadRest-time.Second {
		t.Fatalf("a plain 6s overload rests %v by %s, want the %v cooldown", time.Until(r5.Until).Round(time.Second), r5.By, overloadRest)
	}

	// an HTTP-header Retry-After still works, as any vendor's does
	forgetRests(t)
	h := http.Header{}
	h.Set("Retry-After", "600")
	plainBig := []byte(`{"error":{"code":"server_is_overloaded","message":"overloaded"}}`)
	r6 := s.restAfter(c, 529, h, plainBig)
	if r6.By != "retry-after" || time.Until(r6.Until) > overloadRestMost+time.Second {
		t.Fatalf("an HTTP Retry-After 600 rests %v by %s, want capped at %v", time.Until(r6.Until).Round(time.Second), r6.By, overloadRestMost)
	}
}

// keptAgain, the retry that keeps a conversation on the key its vendor
// cached it with, doesn't retry into a hard window the vendor named
// (x-retry-metadata: NO_MORE_RETRY): every ask inside the window is
// refused alike, so it moves at once. A plain overload (retry-after of
// seconds) is still asked once more, as before.
func TestKeptAgainSkipsNoMoreRetry(t *testing.T) {
	aff := &Affinity{Kept: true, Last: "codex"}
	c := candidate{rest: "codex"}
	hw := func(status int, header http.Header, body string) *holdWriter {
		h := &holdWriter{header: header, status: status}
		h.held.WriteString(body)
		return h
	}
	nmr := `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY","retry-after":"305"},"message":"Our servers are currently overloaded. Please try again later."}}`
	plain := `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`

	// a hard window: not asked again
	h1 := hw(529, http.Header{}, nmr)
	if d, ok := keptAgain(aff, c, h1, false); ok {
		t.Fatalf("keptAgain retried a NO_MORE_RETRY overload (wait %v): it should move at once", d)
	}
	// the header spelling, too
	hh := http.Header{}
	hh.Set("X-Retry-Metadata", "NO_MORE_RETRY")
	h2 := hw(529, hh, `{"error":{"code":"server_is_overloaded","message":"overloaded"}}`)
	if d, ok := keptAgain(aff, c, h2, false); ok {
		t.Fatalf("keptAgain retried a header NO_MORE_RETRY (wait %v)", d)
	}
	// a plain overload: asked again, as before
	h3 := hw(529, http.Header{}, plain)
	if _, ok := keptAgain(aff, c, h3, false); !ok {
		t.Fatal("keptAgain didn't retry a plain overload: it should, as before")
	}
	// and a 6s blip, still asked again
	h4 := http.Header{}
	h4.Set("Retry-After", "6")
	h5 := hw(529, h4, plain)
	if _, ok := keptAgain(aff, c, h5, false); !ok {
		t.Fatal("keptAgain didn't retry a 6s overload: it should")
	}
}
