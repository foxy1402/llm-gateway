// Tests for the "one combo, one unpinned member, many keys" topology - the
// shape a user gets when a combo names just a provider with "any key
// (rotate)". It has no sibling member to absorb a fault, so any bug that
// retires the member turns straight into a 502 instead of a slower-but-
// successful request.

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// EXACTLY the user's topology: ONE combo, ONE member, no account pin
// ("any key (rotate)"), provider holds 4 keys. The first three are dead;
// the fourth must serve. If the single member gets burned on the first
// failure, this 502s.
func TestSingleUnpinnedMemberRotatesAllKeys(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth != "Bearer k4" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "agg", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("agg", []config.Account{
		{ID: "agg:k1", ProviderID: "agg", Label: "k1", AuthKey: "k1", Enabled: true, Weight: 1},
		{ID: "agg:k2", ProviderID: "agg", Label: "k2", AuthKey: "k2", Enabled: true, Weight: 1},
		{ID: "agg:k3", ProviderID: "agg", Label: "k3", AuthKey: "k3", Enabled: true, Weight: 1},
		{ID: "agg:k4", ProviderID: "agg", Label: "k4", AuthKey: "k4", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// ONE member, UNPINNED.
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "agg"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("single unpinned member must rotate through ALL keys, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[len(seen)-1] != "Bearer k4" {
		t.Fatalf("the live key must be the one that served, calls: %v", seen)
	}
	t.Logf("keys tried in order: %v (rotation stops as soon as a live key answers)", seen)
}

// Same topology, but a key in the pool has NO model configured (the
// blank-model path). It must be skipped and a sibling key must still serve.
func TestSingleUnpinnedMemberSurvivesModellessKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	// Provider has NO default model, so a key without its own model resolves to "".
	provs := []config.Provider{{ID: "agg", BaseURL: upstream.URL, Model: "", Weight: 1, Enabled: true}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	// Account order matters: the round-robin counter starts at 1, so the SECOND
	// entry is picked first. The model-less key must be that first pick, otherwise
	// the test never reaches the blank-model path it exists to cover.
	if err := st.ReplaceAccounts("agg", []config.Account{
		{ID: "agg:good", ProviderID: "agg", Label: "good", AuthKey: "good", Enabled: true, Weight: 1, Model: "real-model"},
		{ID: "agg:bad", ProviderID: "agg", Label: "bad", AuthKey: "bad", Enabled: true, Weight: 1, Model: ""},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "agg"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("a model-less key must not kill the whole member, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "Bearer good" {
		t.Fatalf("expected exactly the good key to serve, got %v", seen)
	}
}

// All keys dead: the single unpinned member must try EVERY key before giving up,
// and the client must see the real upstream status (429), not an opaque 502.
func TestSingleUnpinnedMemberExhaustsAllKeys(t *testing.T) {
	var mu sync.Mutex
	tried := map[string]bool{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tried[r.Header.Get("Authorization")] = true
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "agg", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	accts := []config.Account{}
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		accts = append(accts, config.Account{
			ID: "agg:" + k, ProviderID: "agg", Label: k, AuthKey: k, Enabled: true, Weight: 1,
		})
	}
	if err := st.ReplaceAccounts("agg", accts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "agg"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")

	mu.Lock()
	n := len(tried)
	mu.Unlock()
	if n != 4 {
		t.Errorf("only %d of 4 keys were attempted before giving up: %v", n, tried)
	}
	// The caller should learn the real reason (429 = every key over quota), not 502.
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (the real upstream reason) — got an opaque failure instead: %s", rec.Code, rec.Body.String())
	}
}

// The pool draining to cooldown must be RECOVERABLE: once the cooldown expires
// the same single-member combo serves again with no config change and no restart.
func TestSingleUnpinnedMemberRecoversAfterCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"m","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{ID: "agg", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true}}
	px, st, reg := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("agg", []config.Account{
		{ID: "agg:k1", ProviderID: "agg", Label: "k1", AuthKey: "k1", Enabled: true, Weight: 1},
		{ID: "agg:k2", ProviderID: "agg", Label: "k2", AuthKey: "k2", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "agg"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	// 1s cooldown so the window can actually elapse inside the test (Configure
	// ignores 0 — it only accepts a positive value).
	reg.Health().Configure(1, []int{429})

	// Drain the pool into cooldown.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	px.ServeHTTP(httptest.NewRecorder(), req, "chat.completions")
	if reg.Health().IsAccountAvailable("agg", "agg:k1") || reg.Health().IsAccountAvailable("agg", "agg:k2") {
		t.Fatal("expected both keys cooling down after 429s")
	}

	// Let the cooldown expire, with the upstream now healthy again.
	time.Sleep(1100 * time.Millisecond)
	fail.Store(false)
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("combo must self-heal once cooldowns expire, got %d: %s", rec.Code, rec.Body.String())
	}
}
