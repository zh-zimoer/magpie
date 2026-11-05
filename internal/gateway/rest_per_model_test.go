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
	over := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","headers":{"x-retry-metadata":"NO_MORE_RETRY","retry-after":"301"},"message":"Our servers are currently overloaded. Please try again later."}}`)

	h := http.Header{}
	h.Set("Retry-After", "301")
	r := s.restAfter(c, 529, h, over)
	if r.By != "retry-after" {
		t.Fatalf("rests by %s, want retry-after", r.By)
	}
	if d := time.Until(r.Until); d < overloadHardRestMost-time.Second || d > overloadHardRestMost+time.Second {
		t.Fatalf("rests %v, want the window capped at %v", d.Round(time.Second), overloadHardRestMost)
	}

	// a plain overload, no NO_MORE_RETRY, rests the minute as before
	forgetRests(t)
	h2 := http.Header{}
	h2.Set("Retry-After", "301")
	plain := []byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`)
	if r := s.restAfter(c, 529, h2, plain); time.Until(r.Until) > overloadRestMost+time.Second {
		t.Fatalf("a plain overload rests %v, want at most %v", time.Until(r.Until).Round(time.Second), overloadRestMost)
	}

	// and a 6s blip is still a 6s rest
	forgetRests(t)
	h3 := http.Header{}
	h3.Set("Retry-After", "6")
	r3 := s.restAfter(c, 529, h3, over)
	if d := time.Until(r3.Until); d > 7*time.Second || d < 4*time.Second {
		t.Fatalf("a 6s window rests %v", d.Round(time.Second))
	}
}
