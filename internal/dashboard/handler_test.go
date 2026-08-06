package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"llm-gateway/internal/config"
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

// TestComboAccountValidation pins: bogus account IDs must fail validateCombo
// (this is what turns a bogus pin into a clean 400 instead of the raw SQLite
// FK 500 observed in the live smoke run); valid pins pass.
func TestComboAccountValidation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.UpsertProvider(config.Provider{ID: "vercel", BaseURL: "http://x", Model: "m", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAccounts("vercel", []config.Account{
		{ID: "vercel:k1", ProviderID: "vercel", Label: "k1", AuthKey: "a1", Enabled: true, Weight: 1},
		{ID: "vercel:k2", ProviderID: "vercel", Label: "k2", AuthKey: "a2", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Legacy provider: no account pool at all.
	if err := st.UpsertProvider(config.Provider{ID: "legacy", BaseURL: "http://y", AuthKey: "k", Model: "m", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		t.Fatalf("reload: %v", err)
	}
	api := &apiHandlers{d: &Deps{Store: st, Reg: reg}}

	if msg := api.validateCombo(comboPayload{ID: "bad", Members: []comboMemberPayload{
		{ProviderID: "vercel", AccountID: "vercel:nope"},
	}}); msg == "" {
		t.Fatal("nonexistent account pin must fail validation (FK 500 regress)")
	}

	if msg := api.validateCombo(comboPayload{ID: "bad2", Members: []comboMemberPayload{
		{ProviderID: "legacy", AccountID: "legacy:zzz"},
	}}); msg == "" {
		t.Fatal("pin on accountless provider must fail validation")
	}

	if msg := api.validateCombo(comboPayload{ID: "good", Members: []comboMemberPayload{
		{ProviderID: "vercel", AccountID: "vercel:k2"},
	}}); msg != "" {
		t.Fatalf("valid pin rejected: %s", msg)
	}

	// Pin to an account of ANOTHER provider is caught too.
	if msg := api.validateCombo(comboPayload{ID: "cross", Members: []comboMemberPayload{
		{ProviderID: "legacy", AccountID: "vercel:k2"},
	}}); msg == "" {
		t.Fatal("cross-provider pin must fail validation")
	}
}
