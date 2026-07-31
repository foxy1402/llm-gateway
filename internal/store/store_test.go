package store

import (
	"context"
	"path/filepath"
	"testing"

	"llm-gateway/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestProviderCRUD(t *testing.T) {
	st := open(t)
	if err := st.UpsertProvider(config.Provider{ID: "p1", BaseURL: "https://x", AuthKey: "k", Model: "m", Weight: 2, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProvider("p1")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Weight != 2 {
		t.Fatalf("provider: %+v", p)
	}
	p.Enabled = false
	if err := st.UpsertProvider(*p); err != nil {
		t.Fatal(err)
	}
	p2, _ := st.GetProvider("p1")
	if p2.Enabled {
		t.Fatal("update did not persist")
	}
}

func TestComboRoundTrip(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: "u", AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	st.UpsertProvider(config.Provider{ID: "b", BaseURL: "u", AuthKey: "k", Model: "m", Weight: 1, Enabled: true})
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Members: []string{"a", "b"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	combos, err := st.ListCombos()
	if err != nil {
		t.Fatal(err)
	}
	if len(combos) != 1 || len(combos[0].Members) != 2 {
		t.Fatalf("combos: %+v", combos)
	}
}

func TestLogAndQuery(t *testing.T) {
	st := open(t)
	entry := config.LogEntry{Timestamp: 1, ModelIn: "m", ProviderUsed: "p", Endpoint: "chat.completions", Status: 200, LatencyMs: 5}
	if err := st.LogRequest(entry); err != nil {
		t.Fatal(err)
	}
	logs, err := st.QueryLogs(config.LogFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ProviderUsed != "p" {
		t.Fatalf("logs: %+v", logs)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	st := open(t)
	if err := st.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetSetting("k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v" {
		t.Fatalf("setting: %q", v)
	}
}
