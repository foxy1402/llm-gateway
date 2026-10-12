package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

// P2-9: an import must not carry reserved runtime settings (failban:state) — a
// tampered backup could otherwise reset brute-force ban state on restart. The
// reserved row is skipped while everything else restores.
func TestImportSkipsReservedSettings(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveProviderWithAccounts(config.Provider{
		ID: "p", Display: "P", BaseURL: "https://api.example.com/v1",
		AuthKey: "k", Model: "m", Weight: 1, Enabled: true,
	}, []config.Account{{ID: "p:a", Label: "a", AuthKey: "sk", Enabled: true, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	// Pre-existing real ban state must survive the import.
	if err := st.SetSetting("failban:state", `{"1.2.3.4":{"until":9999999999}}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("health.cooldown", "45"); err != nil {
		t.Fatal(err)
	}

	inject := exportHeader + "\n\nBEGIN TRANSACTION;\n" +
		"DELETE FROM combo_members;\nDELETE FROM combos;\nDELETE FROM provider_models;\n" +
		"DELETE FROM provider_accounts;\nDELETE FROM providers;\nDELETE FROM proxies;\nDELETE FROM settings;\n" +
		"INSERT INTO providers (id, display, base_url, auth_key, model, weight, tags, enabled, responses_native, token_param_mode, proxy_rotate) VALUES ('p','P','https://api.example.com/v1','k','m',1,'',1,0,'',0);\n" +
		"INSERT INTO provider_accounts (id, provider_id, label, auth_key, model, enabled, position, weight, token_param_mode) VALUES ('p:a','p','a','sk','',1,0,1,'');\n" +
		"INSERT INTO settings (key, value) VALUES ('health.cooldown','45'),('failban:state','{\"evil\":true}');\n" +
		"COMMIT;\n"

	if err := st.ImportSQL(inject); err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if v, _ := st.GetSetting("failban:state"); strings.Contains(v, "evil") {
		t.Fatalf("reserved setting was overwritten by import: %q", v)
	}
	if v, _ := st.GetSetting("health.cooldown"); v != "45" {
		t.Fatalf("non-reserved setting not restored: %q", v)
	}
}

// P2-9: ExportSQL must not emit reserved runtime keys in the first place.
func TestExportExcludesReservedSettings(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting("failban:state", `{"1.2.3.4":{}}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("health.cooldown", "30"); err != nil {
		t.Fatal(err)
	}
	dump, err := st.ExportSQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, "failban:state") {
		t.Fatalf("export leaked reserved setting:\n%s", dump)
	}
	if !strings.Contains(dump, "health.cooldown") {
		t.Fatal("export dropped a real setting")
	}
}

// P2-10: a client-supplied account ID belonging to a DIFFERENT provider must be
// rejected rather than silently re-parented (which overwrote the other
// provider's credentials).
func TestReplaceAccountsRejectsCrossProviderID(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(context.Background(), filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"a", "b"} {
		if err := st.SaveProviderWithAccounts(config.Provider{
			ID: id, Display: id, BaseURL: "https://api.example.com/v1",
			AuthKey: "k", Model: "m", Weight: 1, Enabled: true,
		}, []config.Account{{ID: id + ":k1", Label: "k1", AuthKey: "secret-" + id, Enabled: true, Weight: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	// Try to overwrite provider B's account from provider A's replace call.
	err = st.ReplaceAccounts("a", []config.Account{
		{ID: "b:k1", ProviderID: "a", Label: "hijack", AuthKey: "attacker", Enabled: true, Weight: 1},
	})
	if err == nil {
		t.Fatal("cross-provider account ID was accepted")
	}
	// B's key must be untouched.
	b, err := st.GetProvider("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Accounts) != 1 || b.Accounts[0].AuthKey != "secret-b" {
		t.Fatalf("provider B credentials were clobbered: %+v", b.Accounts)
	}
}

// P3-11: a DB path containing DSN-breaking characters must still open (the path
// is escaped rather than interpolated raw).
func TestOpenWithSpecialCharPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird&name#1.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open with special-char path: %v", err)
	}
	defer st.Close()
	if err := st.SetSetting("k", "v"); err != nil {
		t.Fatalf("write after special-char open: %v", err)
	}
	if v, _ := st.GetSetting("k"); v != "v" {
		t.Fatalf("round trip failed: %q", v)
	}
}
