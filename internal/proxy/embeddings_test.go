package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/registry"
)

func TestEmbeddingsPassthrough(t *testing.T) {
	var sawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	px, _, _ := newTestStack(t, upstream, []config.Provider{
		{ID: "emb", BaseURL: upstream.URL, AuthKey: "k", Model: "text-emb", Weight: 1, Enabled: true},
	}, nil)

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"emb","input":"hello"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointEmbeddings)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sawPath, "embeddings") {
		t.Fatalf("upstream path: %s", sawPath)
	}
}

// Embeddings must address a provider id directly, not a combo id.
func TestEmbeddingsRejectsCombo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	px, _, _ := newTestStack(t, upstream, []config.Provider{
		{ID: "a", BaseURL: upstream.URL, AuthKey: "k", Model: "m", Weight: 1, Enabled: true},
	}, []config.Combo{
		{ID: "combo", Rotation: config.RoundRobin, Members: []config.ComboMember{{ProviderID: "a"}}, Enabled: true},
	})

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"combo","input":"x"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointEmbeddings)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for combo embeddings, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEmbeddingsDirectProviderSelfHealsAcross10Keys answers a real support
// question: a direct provider (no combo — the dashboard only shows a rotation
// policy picker for combos) with a large multi-key pool used for /v1/embeddings
// must self-heal exactly like chat.completions does. The account-rotation/retry
// loop in ServeHTTP is endpoint-agnostic, so this proves it end-to-end for
// embeddings specifically rather than assuming chat.completions coverage
// generalizes. 9 of 10 keys are dead (mixing 429/402/500); only the 10th key
// is live, so a single request can only succeed by rotating through the pool.
func TestEmbeddingsDirectProviderSelfHealsAcross10Keys(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	deadCodes := map[string]int{
		"Bearer k1": 429, "Bearer k2": 402, "Bearer k3": 500, "Bearer k4": 429, "Bearer k5": 402,
		"Bearer k6": 429, "Bearer k7": 402, "Bearer k8": 500, "Bearer k9": 429,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if code, dead := deadCodes[auth]; dead {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	accounts := make([]config.Account, 0, 10)
	for i := 1; i <= 10; i++ {
		accounts = append(accounts, config.Account{
			ID: fmt.Sprintf("mistral-embed-2312:k%d", i), ProviderID: "mistral-embed-2312",
			Label: fmt.Sprintf("k%d", i), AuthKey: fmt.Sprintf("k%d", i), Enabled: true, Weight: 1,
		})
	}
	provs := []config.Provider{{ID: "mistral-embed-2312", BaseURL: upstream.URL, Model: "text-emb", Weight: 1, Enabled: true, Accounts: accounts}}
	px, st, _ := newTestStack(t, upstream, provs, nil)
	if err := st.ReplaceAccounts("mistral-embed-2312", accounts); err != nil {
		t.Fatal(err)
	}
	if err := px.registry.Reload(st); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"mistral-embed-2312","input":"hello"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointEmbeddings)
	if rec.Code != 200 {
		t.Fatalf("direct provider should self-heal past 9 dead keys onto k10 for embeddings, got status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[len(seen)-1] != "Bearer k10" {
		t.Fatalf("last call should be the only live key, got %v", seen)
	}
	if len(seen) < 2 {
		t.Fatalf("expected genuine rotation across multiple keys, only 1 call made: %v", seen)
	}
}
