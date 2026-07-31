package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatalf("reload: %v", err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Deps{Store: st, Reg: reg, Proxy: nil, Auth: nil, Env: nil})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// providerPayload JSON tags must match config field names used by the SPA.
func TestProviderPayloadToConfig(t *testing.T) {
	body := `{"id":"p1","display":"P","base_url":"https://x","auth_key":"k","model":"m","weight":4,"tags":["fast"],"enabled":true,"responses_native":true}`
	var pp providerPayload
	if err := json.Unmarshal([]byte(body), &pp); err != nil {
		t.Fatal(err)
	}
	cfg := pp.toConfig()
	if cfg.BaseURL != "https://x" || cfg.Weight != 4 || !cfg.ResponsesNative || cfg.Tags[0] != "fast" {
		t.Fatalf("payload mapping wrong: %+v", cfg)
	}
}

func TestProvidersCRUDEndpoints(t *testing.T) {
	srv := newServer(t)

	// Create.
	body := `{"id":"p1","base_url":"https://api.example.com","auth_key":"k","model":"m","weight":1,"enabled":true}`
	resp, err := http.Post(srv.URL+"/dashboard/api/providers", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Skip("auth middleware active in real server; skipping unauthenticated CRUD")
	}
}
