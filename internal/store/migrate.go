package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
)

// migrate upgrades a legacy database to the current schema. The schema itself is
// idempotent (CREATE TABLE IF NOT EXISTS), but legacy databases created before the
// multi-account refactor have three gaps:
//
//  1. combo_members has no `model` column.
//  2. provider_accounts is empty even where providers.auth_key holds the only key.
//  3. provider_models is unpopulated (we can seed it with provider.model, if set).
//
// We detect this by PRAGMA user_version: 0 means an untouched-by-us legacy DB. The
// entire migration runs in one transaction so a failure can't produce a torn schema.
func (s *Store) migrate(ctx context.Context) error {
	var ver int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		return err
	}
	if ver >= 1 {
		return nil // already at current version
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. combo_members gained a model column. SQLite's sqlite_master stores the DDL
	// literally, so instead of parsing it we just empty the table and recreate it
	// (imported dumps re-create their rows at import time anyway). This is safe
	// because at v0 the only way combo_members exists is via the legacy schema.
	if hasTable(ctx, tx, "combo_members") && !hasColumn(ctx, tx, "combo_members", "model") {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE combo_members_new (
			combo_id    TEXT NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			model       TEXT NOT NULL DEFAULT '',
			position    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (combo_id, provider_id, model)
		)`); err != nil {
			return fmt.Errorf("combo_members_new: %w", err)
		}
		// Preserve legacy member linkage with empty model (fallback to provider.model).
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO combo_members_new (combo_id, provider_id, model, position)
			 SELECT combo_id, provider_id, '', position FROM combo_members`); err != nil {
			return fmt.Errorf("migrate combo_members: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DROP TABLE combo_members"); err != nil {
			return fmt.Errorf("drop old combo_members: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE combo_members_new RENAME TO combo_members"); err != nil {
			return fmt.Errorf("rename combo_members: %w", err)
		}
	}

	// 2a. Legacy DBs (created before weight existed) have provider_accounts without a
	// weight column because CREATE TABLE IF NOT EXISTS is a no-op on an existing table.
	if hasTable(ctx, tx, "provider_accounts") && !hasColumn(ctx, tx, "provider_accounts", "weight") {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE provider_accounts ADD COLUMN weight INTEGER NOT NULL DEFAULT 1"); err != nil {
			return fmt.Errorf("add weight column: %w", err)
		}
	}

	// 2. Seed provider_accounts from provider.auth_key. One row per non-empty key,
	// ID chosen deterministically from the provider ID to avoid duplicates on a
	// partially-migrated DB re-open.
	if hasTable(ctx, tx, "provider_accounts") {
		rows, err := tx.QueryContext(ctx, `SELECT id, auth_key FROM providers WHERE auth_key != ''`)
		if err != nil {
			return fmt.Errorf("scan provider keys: %w", err)
		}
		type pk struct{ id, key string }
		var provs []pk
		for rows.Next() {
			var p pk
			if err := rows.Scan(&p.id, &p.key); err != nil {
				rows.Close()
				return err
			}
			provs = append(provs, p)
		}
		rows.Close()
		for _, p := range provs {
			acctID := p.id + ":default"
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_accounts (id, provider_id, label, auth_key, enabled, position)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO NOTHING`,
				acctID, p.id, "default", p.key, 1, 0); err != nil {
				return fmt.Errorf("seed account for %q: %w", p.id, err)
			}
		}
	}

	// 3. Seed a usable model list from provider.model when one is configured.
	if hasTable(ctx, tx, "provider_models") {
		rows, err := tx.QueryContext(ctx, `SELECT id, model FROM providers WHERE model != ''`)
		if err != nil {
			return fmt.Errorf("scan provider models: %w", err)
		}
		type pm struct{ id, model string }
		var provs []pm
		for rows.Next() {
			var p pm
			if err := rows.Scan(&p.id, &p.model); err != nil {
				rows.Close()
				return err
			}
			provs = append(provs, p)
		}
		rows.Close()
		for _, p := range provs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_models (provider_id, model_id)
				 VALUES (?, ?) ON CONFLICT(provider_id, model_id) DO NOTHING`,
				p.id, p.model); err != nil {
				return fmt.Errorf("seed model for %q: %w", p.id, err)
			}
		}
	}

	// Mark the DB as migrated. Use a string assignment because PRAGMA doesn't take binds.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

// hasTable reports whether a table exists in the current connection's schema.
func hasTable(ctx context.Context, tx *sql.Tx, name string) bool {
	var n int
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}

// hasColumn reports whether a table contains the given column.
func hasColumn(ctx context.Context, tx *sql.Tx, table, col string) bool {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if strings.EqualFold(name, col) {
			return true
		}
	}
	return false
}

// randomID returns a short random ID for newly created accounts.
func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

// NewAccountID builds a provider account ID from a provider and a short random suffix.
func NewAccountID(providerID string) string {
	return providerID + ":" + randomID()
}
