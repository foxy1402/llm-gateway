package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// TestAlternatingKeysAcrossRequests: one provider with two accounts; successive
// requests should cycle keys round-robin, proven by the Authorization header the
// upstream sees.
func TestAlternatingKeysAcrossRequests(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"x","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "p:a1", ProviderID: "p", Label: "one", AuthKey: "key-1", Enabled: true, Weight: 1},
			{ID: "p:a2", ProviderID: "p", Label: "two", AuthKey: "key-2", Enabled: true, Weight: 1},
		},
	}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	// Reload so the registry picks up the account pool.
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"p","messages":[],"stream":false}`
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 {
		t.Fatalf("expected 4 upstream calls, got %d", len(seen))
	}
	first, second := seen[0], seen[1]
	if first == second {
		t.Fatalf("expected alternating keys, got same twice: %v", seen)
	}
	if seen[2] != first || seen[3] != second {
		t.Fatalf("keys not round-robin: %v", seen)
	}
}

// TestMemberModelHonored: a combo member pins model "member-model" — the upstream
// request body must carry that exact model, not the provider default.
func TestMemberModelHonored(t *testing.T) {
	var gotModel string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		gotModel = b.Model
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"` + b.Model + `","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{
		{ID: "p", BaseURL: upstream.URL, AuthKey: "k", Model: "provider-default", Weight: 1, Enabled: true},
	}
	combos := []config.Combo{{
		ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p", Model: "member-model"}},
	}}
	px, _, _ := newTestStack(t, upstream, provs, combos)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, "chat.completions")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotModel != "member-model" {
		t.Fatalf("upstream got model %q, want member-model", gotModel)
	}
}

// TestAccountCooldownFallsToSibling: key-1 starts returning 429; the SAME request
// must immediately retry key-2 and succeed, while key-1 enters cooldown.
func TestAccountCooldownFallsToSibling(t *testing.T) {
	var mu sync.Mutex
	burned := map[string]bool{"key-1": true}
	seen := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		badf := burned[strings.TrimPrefix(auth, "Bearer ")]
		mu.Unlock()
		if badf {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"model":"x","choices":[]}`))
	}))
	defer upstream.Close()

	provs := []config.Provider{{
		ID: "p", BaseURL: upstream.URL, Model: "m", Weight: 1, Enabled: true,
		Accounts: []config.Account{
			{ID: "p:a1", ProviderID: "p", Label: "one", AuthKey: "key-1", Enabled: true, Weight: 1},
			{ID: "p:a2", ProviderID: "p", Label: "two", AuthKey: "key-2", Enabled: true, Weight: 1},
		},
	}}
	combos := []config.Combo{{
		ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p"}},
	}}
	px, st, reg := newTestStack(t, upstream, provs, combos)
	if err := st.ReplaceAccounts("p", provs[0].Accounts); err != nil {
		t.Fatal(err)
	}
	reg.Health().Configure(1, []int{429}) // 1s cooldown
	if err := reg.Reload(st); err != nil {
		t.Fatal(err)
	}

	// Two requests: each starts a fresh rotation, so each will hit key-1 → 429 →
	// key-2 → 200. Both must ultimately succeed.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"c","messages":[],"stream":false}`))
		rec := httptest.NewRecorder()
		px.ServeHTTP(rec, req, "chat.completions")
		if rec.Code != 200 {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	sawKey1, sawKey2 := false, false
	for _, s := range seen {
		if s == "Bearer key-1" {
			sawKey1 = true
		}
		if s == "Bearer key-2" {
			sawKey2 = true
		}
	}
	if !sawKey1 || !sawKey2 {
		t.Fatalf("expected both keys exercised (fallback), got %v", seen)
	}
	// key-1 should now be cooled.
	if reg.Health().IsAccountAvailable("p", "p:a1") {
		t.Fatal("key-1 should be in cooldown after repeated 429s")
	}
	if !reg.Health().IsAccountAvailable("p", "p:a2") {
		t.Fatal("key-2 should stay available")
	}
	time.Sleep(0) // allow cooldown goroutines to settle
}
