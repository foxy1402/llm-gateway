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

// TestComboMemberAccountPersists: a member pinned to a specific account key
// round-trips through storage; deleting that key clears the pin (NULL), leaving
// the member to fall back to pool rotation instead of hard-failing.
func TestComboMemberAccountPersists(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: "u", Enabled: true})
	accounts := []config.Account{
		{ID: "a:k1", ProviderID: "a", Label: "one", AuthKey: "key1", Enabled: true, Weight: 1},
		{ID: "a:k2", ProviderID: "a", Label: "two", AuthKey: "key2", Enabled: true, Weight: 1},
	}
	if err := st.ReplaceAccounts("a", accounts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{
			{ProviderID: "a", AccountID: "a:k2", Model: "gpt-oss-120b"},
			{ProviderID: "a", Model: "kimi-k3"}, // no pin → rotates across keys
		}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCombo("c")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("members: %+v", got.Members)
	}
	if got.Members[0].AccountID != "a:k2" || got.Members[0].Model != "gpt-oss-120b" {
		t.Fatalf("pinned member: %+v", got.Members[0])
	}
	if got.Members[1].AccountID != "" {
		t.Fatalf("unpinned member should read back empty, got %q", got.Members[1].AccountID)
	}
	// Removing the pinned key must NULL out the pin instead of breaking the combo.
	if err := st.ReplaceAccounts("a", accounts[:1]); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetCombo("c")
	if got2.Members[0].AccountID != "" {
		t.Fatalf("pin should clear after account deletion, got %q", got2.Members[0].AccountID)
	}
}

// TestReplaceAccountsPreservesPins: editing the account pool (disable one key,
// rename another, change weights) must NOT clear combo pins to keys that survive
// the edit — only genuinely removed keys may lose their pin. Regression for the
// smoke failure where a mere "disable key" PUT detached every pin on the provider.
func TestReplaceAccountsPreservesPins(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "a", BaseURL: "u", Enabled: true})
	accounts := []config.Account{
		{ID: "a:k1", ProviderID: "a", Label: "one", AuthKey: "key1", Enabled: true, Weight: 1},
		{ID: "a:k2", ProviderID: "a", Label: "two", AuthKey: "key2", Enabled: true, Weight: 1},
	}
	if err := st.ReplaceAccounts("a", accounts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCombo(config.Combo{ID: "c", Rotation: config.Priority, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "a", AccountID: "a:k2"}}}); err != nil {
		t.Fatal(err)
	}

	// Edit in place: same IDs, flipped attributes.
	edited := []config.Account{
		{ID: "a:k1", ProviderID: "a", Label: "one", AuthKey: "key1", Enabled: false, Weight: 3},
		{ID: "a:k2", ProviderID: "a", Label: "two-renamed", AuthKey: "key2", Enabled: true, Weight: 2},
	}
	if err := st.ReplaceAccounts("a", edited); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCombo("c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Members[0].AccountID != "a:k2" {
		t.Fatalf("pin must survive an in-place pool edit, got %q", got.Members[0].AccountID)
	}
	p, _ := st.GetProvider("a")
	if p.Accounts[0].Enabled || p.Accounts[0].Weight != 3 || p.Accounts[1].Label != "two-renamed" {
		t.Fatalf("in-place edit not applied: %+v", p.Accounts)
	}

	// Swap one key for a brand-new one: k1's row goes away, k2's pin must remain.
	swapped := []config.Account{
		{ID: "a:k9", ProviderID: "a", Label: "new", AuthKey: "key9", Enabled: true, Weight: 1},
		edited[1],
	}
	if err := st.ReplaceAccounts("a", swapped); err != nil {
		t.Fatal(err)
	}
	got2, _ := st.GetCombo("c")
	if got2.Members[0].AccountID != "a:k2" {
		t.Fatalf("pin must survive removal of a sibling key, got %q", got2.Members[0].AccountID)
	}
}

func TestAccountCRUD(t *testing.T) {
	st := open(t)
	st.UpsertProvider(config.Provider{ID: "p", BaseURL: "u", AuthKey: "k", Model: "m", Enabled: true})
	accounts := []config.Account{
		{ID: "p:a1", ProviderID: "p", Label: "primary", AuthKey: "key1", Model: "moonshotai/kimi-k3", Enabled: true, Weight: 2},
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
	if p.Accounts[0].AuthKey != "key1" || p.Accounts[0].Weight != 2 || p.Accounts[0].Model != "moonshotai/kimi-k3" {
		t.Fatalf("account[0]: %+v", p.Accounts[0])
	}
	if p.Accounts[1].Model != "" {
		t.Fatalf("account[1] model should default empty, got %q", p.Accounts[1].Model)
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
