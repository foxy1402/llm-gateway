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
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Members: []config.ComboMember{{ProviderID: "a"}, {ProviderID: "b"}}, Enabled: true}); err != nil {
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

func TestComboMemberModelPersists(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: "u", AuthKey: "k", Model: "m", Enabled: true})
	st.UpsertProvider(config.Provider{ID: "b", BaseURL: "u", AuthKey: "k", Model: "m", Enabled: true})
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{
			{ProviderID: "a", Model: "gpt-4o"},
			{ProviderID: "b", Model: "llama-3"},
			{ProviderID: "b", Model: "mixtral"},
		}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCombo("c")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("combo not found")
	}
	if len(got.Members) != 3 {
		t.Fatalf("members: %+v", got.Members)
	}
	if got.Members[0].Model != "gpt-4o" || got.Members[1].Model != "llama-3" || got.Members[2].Model != "mixtral" {
		t.Fatalf("member models: %+v", got.Members)
	}
}

func TestAccountCRUD(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "p", BaseURL: "u", AuthKey: "k", Model: "m", Enabled: true})
	accounts := []config.Account{
		{ID: "p:a1", ProviderID: "p", Label: "primary", AuthKey: "key1", Enabled: true, Weight: 2},
		{ID: "p:a2", ProviderID: "p", Label: "backup", AuthKey: "key2", Enabled: true, Weight: 1},
	}
	if err := st.ReplaceAccounts("p", accounts); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProvider("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts) != 2 {
		t.Fatalf("accounts: %+v", p.Accounts)
	}
	if p.Accounts[0].AuthKey != "key1" || p.Accounts[0].Weight != 2 {
		t.Fatalf("account[0]: %+v", p.Accounts[0])
	}
	// Remove one.
	if err := st.ReplaceAccounts("p", accounts[:1]); err != nil {
		t.Fatal(err)
	}
	p2, _ := st.GetProvider("p")
	if len(p2.Accounts) != 1 || p2.Accounts[0].AuthKey != "key1" {
		t.Fatalf("after delete: %+v", p2.Accounts)
	}
}

func TestProviderModelPoolPersists(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "p", BaseURL: "u", AuthKey: "k", Model: "m", Enabled: true})
	if err := st.ReplaceModels("p", []string{"m1", "m2", "m3"}); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetProvider("p")
	if len(p.Models) != 3 || p.Models[0] != "m1" || p.Models[2] != "m3" {
		t.Fatalf("models: %+v", p.Models)
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
