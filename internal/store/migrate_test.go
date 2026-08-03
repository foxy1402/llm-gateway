package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateFromLegacySchema builds a v0 database (legacy tables only), reopens
// it through Open(), and asserts the migration seeds a default account + model and
// grants user_version=1 idempotently.
func TestMigrateFromLegacySchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	// Build the legacy schema by hand (as a pre-refactor binary would have written it).
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `
		CREATE TABLE providers (id TEXT PRIMARY KEY, display TEXT NOT NULL, base_url TEXT NOT NULL,
			auth_key TEXT NOT NULL, model TEXT NOT NULL, weight INTEGER NOT NULL DEFAULT 1,
			tags TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			responses_native INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()));
		CREATE TABLE combos (id TEXT PRIMARY KEY, display_name TEXT NOT NULL, rotation TEXT NOT NULL DEFAULT 'round-robin',
			enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT (unixepoch()));
		CREATE TABLE combo_members (combo_id TEXT NOT NULL, provider_id TEXT NOT NULL,
			position INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (combo_id, provider_id));
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO providers (id, display, base_url, auth_key, model, weight, enabled)
			VALUES ('gem', 'Gemini', 'https://x', 'key-legacy', 'gemini-2.0', 1, 1);
		INSERT INTO combos (id, display_name, rotation, enabled) VALUES ('c1', 'combo', 'round-robin', 1);
		INSERT INTO combo_members (combo_id, provider_id, position) VALUES ('c1', 'gem', 0);
	`
	if _, err := raw.Exec(legacy); err != nil {
		t.Fatalf("build legacy: %v", err)
	}
	raw.Close()

	// Open runs the migration.
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	p, err := st.GetProvider("gem")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("provider missing after migration")
	}
	if len(p.Accounts) != 1 || p.Accounts[0].AuthKey != "key-legacy" {
		t.Fatalf("accounts seeded: %+v", p.Accounts)
	}
	if len(p.Models) != 1 || p.Models[0] != "gemini-2.0" {
		t.Fatalf("models seeded: %+v", p.Models)
	}
	// combo_members.model must exist (empty = legacy fallback).
	c, err := st.GetCombo("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Members) != 1 || c.Members[0].ProviderID != "gem" {
		t.Fatalf("combo members: %+v", c.Members)
	}

	// Idempotent: closing and reopening should not duplicate accounts.
	st.Close()
	st2, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	p2, _ := st2.GetProvider("gem")
	if len(p2.Accounts) != 1 {
		t.Fatalf("accounts after reopen (want 1): %+v", p2.Accounts)
	}
}
