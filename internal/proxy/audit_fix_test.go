package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

// P2-1: a stall/read-error after headers are committed must emit an SSE error
// frame so the client can distinguish truncation from completion (and a strict
// Responses client isn't left waiting for a terminal event that never comes).
func TestStreamStallEmitsErrorFrame(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the stream open past the (tiny) stall timeout
	}))
	defer upstream.Close()
	defer close(release)

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, time.Second)
	px.SetStreamStallTimeout(150 * time.Millisecond)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	out := rec.Body.String()
	if !strings.Contains(out, `"upstream_error"`) {
		t.Fatalf("stalled stream must emit an error frame, got:\n%s", out)
	}
	if !strings.Contains(out, "upstream stalled") {
		t.Fatalf("error frame should name the stall, got:\n%s", out)
	}
}

// P2-2: a non-streaming body larger than the cap must NOT be forwarded as a
// truncated-but-valid 200; with a single account it surfaces as a gateway error.
func TestOversizedBodyNotForwardedAs200(t *testing.T) {
	big := strings.Repeat("x", (10<<20)+2048)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"`))
		_, _ = w.Write([]byte(big))
		_, _ = w.Write([]byte(`"}}]}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 5*time.Second)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code == 200 {
		t.Fatalf("an oversized body must not be forwarded as 200; body len=%d", rec.Body.Len())
	}
	if rec.Body.Len() > 1<<20 {
		t.Fatalf("the truncated upstream body leaked to the client: %d bytes", rec.Body.Len())
	}
}

// P2-3: an upstream 401 (revoked key) must rotate to a sibling account that
// still works, rather than failing the request terminally.
func TestUpstream401RotatesToSiblingKey(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer dead" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			// Account rotation starts at index 1, so put the dead key SECOND and the
			// live key FIRST: the first attempt then provably hits the 401 and must
			// rotate onto the live sibling.
			{ID: "p:live", ProviderID: "p", Label: "live", AuthKey: "live", Enabled: true, Weight: 1},
			{ID: "p:dead", ProviderID: "p", Label: "dead", AuthKey: "dead", Enabled: true, Weight: 1},
		},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}
	px.SetAttemptBackoff(0) // keep the test fast

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != 200 {
		t.Fatalf("401 on one key must rotate to the healthy sibling, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[len(seen)-1] != "Bearer live" {
		t.Fatalf("expected the request to end on the live key, calls: %v", seen)
	}
}

// P2-11: the upstream's Retry-After must reach the client when a 429 exhausts
// every account.
func TestExhausted429ForwardsRetryAfter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-RateLimit-Reset", "99")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "solo", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	reg := registry.New()
	reg.Reload(st)
	px := New(reg, st, 2*time.Second)
	px.SetAttemptBackoff(0)
	defer px.WaitLogs()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"solo","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 passthrough, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After = %q, want 42 (upstream hint dropped)", got)
	}
	if got := rec.Header().Get("X-RateLimit-Reset"); got != "99" {
		t.Fatalf("X-RateLimit-Reset = %q, want 99", got)
	}
	if !strings.Contains(rec.Body.String(), "slow down") {
		t.Fatalf("upstream error body should be preserved, got %q", rec.Body.String())
	}
}

// P2-5: the request budget must bound an all-dead rotation instead of letting it
// run for maxAttempts × per-attempt timeout.
func TestRequestBudgetStopsRotation(t *testing.T) {
	// Black-hole upstream: accepts, waits for the request context OR an explicit
	// release. The release matters for teardown: httptest.Server.Close() blocks
	// until in-flight handlers return, and a handler parked on r.Context().Done()
	// alone would hang forever if the client keeps the connection open.
	release := make(chan struct{})
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer dead.Close()
	defer close(release)

	provs := []config.Provider{
		{ID: "a", BaseURL: dead.URL, Model: "m", Weight: 1, Enabled: true},
		{ID: "b", BaseURL: dead.URL, Model: "m", Weight: 1, Enabled: true},
		{ID: "c", BaseURL: dead.URL, Model: "m", Weight: 1, Enabled: true},
	}
	combos := []config.Combo{{ID: "combo", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}, {ProviderID: "c"}}}}
	px, _, _ := newTestStack(t, dead, provs, combos)
	px.SetAttemptBackoff(0)
	// Long per-attempt timeout, tiny total budget: without the budget the request
	// would take >= 2×timeout per attempt.
	px.timeout = 30 * time.Second
	px.SetRequestBudget(300 * time.Millisecond)

	start := time.Now()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"combo","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointChatCompletions)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("request budget did not bound rotation: took %v", elapsed)
	}
	if rec.Code == 200 {
		t.Fatalf("dead upstreams should not yield 200")
	}
}
