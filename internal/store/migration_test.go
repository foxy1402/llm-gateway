package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// seedLegacyDB creates a database with the pre-account-pinning schema: no
// provider_accounts.model, no combo_members.account_id, and the old
// (combo_id, provider_id) primary key.
func seedLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	legacy := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE providers (
			id TEXT PRIMARY KEY, display TEXT NOT NULL, base_url TEXT NOT NULL,
			auth_key TEXT NOT NULL, model TEXT NOT NULL, weight INTEGER NOT NULL DEFAULT 1,
			tags TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			responses_native INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()))`,
		`CREATE TABLE provider_accounts (
			id TEXT PRIMARY KEY,
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			label TEXT NOT NULL DEFAULT '', auth_key TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1, position INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()))`,
		`CREATE TABLE provider_models (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model_id TEXT NOT NULL, position INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (provider_id, model_id))`,
		`CREATE TABLE combos (
			id TEXT PRIMARY KEY, display_name TEXT NOT NULL,
			rotation TEXT NOT NULL DEFAULT 'round-robin', enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()))`,
		`CREATE TABLE combo_members (
			combo_id TEXT NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model TEXT NOT NULL DEFAULT '', position INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (combo_id, provider_id))`,
		`CREATE TABLE request_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL DEFAULT (unixepoch()),
			model_in TEXT NOT NULL, provider_used TEXT NOT NULL, endpoint TEXT NOT NULL,
			status INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
			prompt_tokens INTEGER, completion_tokens INTEGER, error TEXT)`,
		`INSERT INTO providers (id, display, base_url, auth_key, model) VALUES
			('lightning','Lightning','https://api.lightning.ai/v1','sk-legacy-a','openai/gpt-5'),
			('vercel','Vercel','https://ai-gateway.vercel.sh/v1','sk-legacy-b','gpt-oss')`,
		`INSERT INTO provider_accounts (id, provider_id, label, auth_key, position) VALUES
			('lightning:k1','lightning','key one','sk-acct-1',0),
			('lightning:k2','lightning','key two','sk-acct-2',1)`,
		`INSERT INTO combos (id, display_name, rotation) VALUES ('coding','Coding','priority')`,
		`INSERT INTO combo_members (combo_id, provider_id, model, position) VALUES
			('coding','lightning','openai/gpt-5',0),
			('coding','vercel','',1)`,
		`INSERT INTO settings (key, value) VALUES ('health.cooldown','60')`,
	}
	for _, stmt := range legacy {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed legacy schema: %v\n%s", err, stmt)
		}
	}
}

// An older database must open AND be fully readable: before the shape
// migrations, Open succeeded and then every provider/combo read failed with
// "no such column", so the process crash-looped on intact data and ExportSQL
// could not even take a backup first.
func TestOpenMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer st.Close()

	provs, err := st.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders after migration: %v", err)
	}
	if len(provs) != 2 {
		t.Fatalf("want 2 providers, got %d", len(provs))
	}
	var lightning *struct {
		accounts int
		key      string
	}
	for _, p := range provs {
		if p.ID == "lightning" {
			lightning = &struct {
				accounts int
				key      string
			}{len(p.Accounts), p.AuthKey}
		}
	}
	if lightning == nil {
		t.Fatal("lightning provider lost in migration")
	}
	if lightning.accounts != 2 {
		t.Fatalf("want 2 accounts preserved, got %d", lightning.accounts)
	}
	if lightning.key != "sk-legacy-a" {
		t.Fatalf("provider auth key altered: %q", lightning.key)
	}

	combos, err := st.ListCombos()
	if err != nil {
		t.Fatalf("ListCombos after migration: %v", err)
	}
	if len(combos) != 1 || len(combos[0].Members) != 2 {
		t.Fatalf("want 1 combo with 2 members, got %+v", combos)
	}
	// Positions must be renumbered from 0 without collisions under the new PK.
	if combos[0].Members[0].ProviderID != "lightning" || combos[0].Members[1].ProviderID != "vercel" {
		t.Fatalf("member order/content changed: %+v", combos[0].Members)
	}
	if combos[0].Members[0].Model != "openai/gpt-5" {
		t.Fatalf("member model pin lost: %+v", combos[0].Members[0])
	}

	dump, err := st.ExportSQL()
	if err != nil {
		t.Fatalf("ExportSQL after migration: %v", err)
	}
	if dump == "" {
		t.Fatal("export empty after migration")
	}

	pk, err := primaryKeyColumns(context.Background(), st.DB(), "combo_members")
	if err != nil {
		t.Fatalf("read pk: %v", err)
	}
	if len(pk) != 2 || pk[0] != "combo_id" || pk[1] != "position" {
		t.Fatalf("combo_members pk not migrated: %v", pk)
	}

	// Foreign keys must be back ON for pooled connections after the rebuild.
	if _, err := st.DB().Exec(
		`INSERT INTO combo_members (combo_id, provider_id, position) VALUES ('coding','ghost',99)`); err == nil {
		t.Fatal("foreign keys left disabled after migration: orphan member accepted")
	}
}

// Opening twice must be a no-op the second time (migrations are idempotent).
func TestOpenLegacySchemaTwiceIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	for i := 0; i < 2; i++ {
		st, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		combos, err := st.ListCombos()
		if err != nil {
			t.Fatalf("ListCombos on open %d: %v", i, err)
		}
		if len(combos) != 1 || len(combos[0].Members) != 2 {
			t.Fatalf("open %d: combo damaged: %+v", i, combos)
		}
		st.Close()
	}
}
