package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
		{ID: "combo", Rotation: config.RoundRobin, Members: []string{"a"}, Enabled: true},
	})

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"combo","input":"x"}`))
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req, registry.EndpointEmbeddings)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for combo embeddings, got %d: %s", rec.Code, rec.Body.String())
	}
}
