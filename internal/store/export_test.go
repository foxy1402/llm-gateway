package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

func TestExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(context.Background(), filepath.Join(dir, "a.db"))
	defer st.Close()
	st.UpsertProvider(config.Provider{ID: "p", BaseURL: "https://x", AuthKey: "k", Model: "m", Weight: 3, Enabled: true, TokenParamMode: "max_completion_tokens"})
	st.ReplaceAccounts("p", []config.Account{{ID: "p:a1", ProviderID: "p", Label: "a1", AuthKey: "k1", Enabled: true, Weight: 1, TokenParamMode: "max_tokens"}})
	st.UpsertCombo(config.Combo{ID: "c", Rotation: config.RoundRobin, Members: []config.ComboMember{{ProviderID: "p", AccountID: "p:a1", Model: "m", TokenParamMode: "max_completion_tokens"}}, Enabled: true})
	st.SetSetting("some.key", "someval")

	sqlDump, err := st.ExportSQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlDump, "p") {
		t.Fatal("export missing provider")
	}

	st2, _ := Open(context.Background(), filepath.Join(dir, "b.db"))
	defer st2.Close()
	if err := st2.ImportSQL(sqlDump); err != nil {
		t.Fatal(err)
	}
	p, err := st2.GetProvider("p")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Weight != 3 {
		t.Fatalf("imported provider: %+v", p)
	}
	// token_param_mode pins must survive the export/import round trip at all
	// three levels — they were previously missing from ExportSQL's INSERT
	// column lists and silently reset to "" on every restore.
	if p.TokenParamMode != "max_completion_tokens" {
		t.Fatalf("provider token_param_mode lost on export/import: %+v", p)
	}
	if len(p.Accounts) != 1 || p.Accounts[0].TokenParamMode != "max_tokens" {
		t.Fatalf("account token_param_mode lost on export/import: %+v", p.Accounts)
	}
	c, err := st2.GetCombo("c")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Members) != 1 || c.Members[0].TokenParamMode != "max_completion_tokens" {
		t.Fatalf("combo member token_param_mode lost on export/import: %+v", c.Members)
	}
	v, _ := st2.GetSetting("some.key")
	if v != "someval" {
		t.Fatalf("imported setting: %q", v)
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(context.Background(), filepath.Join(dir, "g.db"))
	defer st.Close()
	if err := st.ImportSQL("this is not sql at all"); err == nil {
		t.Fatal("expected error importing garbage")
	}
}

// An export file must not smuggle statements beyond the two shapes ExportSQL
// itself emits — the import endpoint runs them with full DB authority.
func TestImportRejectsDisallowedStatements(t *testing.T) {
	cases := []string{
		"DROP TABLE providers;",
		"UPDATE providers SET auth_key='x';",
		"DELETE FROM request_log;",
		"INSERT INTO request_log (ts) VALUES (1);",
		"PRAGMA foreign_keys=OFF;",
		"ATTACH DATABASE '/tmp/evil.db' AS e;",
		"DELETE FROM providers WHERE id='x';",
	}
	for _, stmt := range cases {
		payload := exportHeader + "\n\nBEGIN TRANSACTION;\n" + stmt + "\nCOMMIT;\n"
		st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		err = st.ImportSQL(payload)
		st.Close()
		if err == nil {
			t.Errorf("statement should be rejected: %s", stmt)
			continue
		}
		if !IsValidation(err) {
			t.Errorf("rejection should be a validation error (400), got: %v", err)
		}
	}
}
