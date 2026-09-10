package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"llm-gateway/internal/config"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	// Warn loudly when bootstrapping a brand-new database: on a PaaS with a
	// missing/unmounted volume the gateway would otherwise start clean and
	// silently serve "no providers" 404s, looking like a code bug rather than an
	// ops problem. (Nonexistent parent dir also implies a fresh file.)
	fresh := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fresh = true
	}
	// Ensure the parent directory exists before SQLite tries to open the file —
	// headless/distroless containers mount volumes that may be empty.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil && !os.IsPermission(err) {
			// Non-fatal: a read-only mount would fail on open anyway with a clearer error.
			return nil, fmt.Errorf("create db dir %q: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// WAL allows many concurrent readers alongside the single writer. Multiple
	// connections let dashboard reads (logs, charts, lists) proceed during
	// LogRequest writes; busy_timeout absorbs any write-write contention.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("run schema: %w", err)
	}
	if fresh {
		slog.Warn("created a NEW empty database — no providers/combos from a previous deploy were found; if you expected state, check that your volume is mounted at DB_PATH", "path", path)
	}
	// Migrations: add detail columns to existing databases (idempotent — safe to
	// re-run; the ONLY tolerated error is SQLite's "duplicate column name"). A
	// fresh DB already has these via schema.sql above; this only matters for a
	// database created before the column existed.
	//
	// Ordering matters: combo_members/provider_accounts columns that the
	// combo_members table rebuild below selects must exist first, so the
	// ADD COLUMN pass runs before rebuildComboMembers.
	columnMigrations := []struct {
		table   string
		columns []string
	}{
		{"request_log", []string{"upstream_url TEXT DEFAULT ''", "request_payload TEXT DEFAULT ''", "response_snippet TEXT DEFAULT ''", "cached_tokens INTEGER", "heal_note TEXT DEFAULT ''"}},
		{"providers", []string{"token_param_mode TEXT NOT NULL DEFAULT ''", "proxy_rotate INTEGER NOT NULL DEFAULT 0"}},
		// provider_accounts.model (per-key model pin) and combo_members.account_id
		// (pin a member to one specific key) were added to schema.sql without a
		// matching ALTER, so a database created before them opened "successfully"
		// and then failed EVERY provider/combo read with "no such column" — the
		// process crash-looped on a volume whose data was perfectly intact, and
		// ExportSQL failed too, so backing up before repair was impossible.
		// account_id must stay nullable with a NULL default: SQLite only permits
		// ADD COLUMN with a REFERENCES clause when the default is NULL.
		{"provider_accounts", []string{"token_param_mode TEXT NOT NULL DEFAULT ''", "model TEXT NOT NULL DEFAULT ''"}},
		{"combo_members", []string{
			"token_param_mode TEXT NOT NULL DEFAULT ''",
			"model TEXT NOT NULL DEFAULT ''",
			"position INTEGER NOT NULL DEFAULT 0",
			"account_id TEXT REFERENCES provider_accounts(id) ON DELETE SET NULL",
		}},
		{"combos", []string{"proxy_rotate INTEGER NOT NULL DEFAULT 0"}},
	}
	for _, mig := range columnMigrations {
		for _, col := range mig.columns {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", mig.table, col)); err != nil {
				if strings.Contains(err.Error(), "duplicate column") {
					continue
				}
				// A failed migration must surface: swallowing it leaves the schema one
				// column short and every later insert/select against it fails mysteriously.
				db.Close()
				return nil, fmt.Errorf("migrate %s add %q: %w", mig.table, strings.Fields(col)[0], err)
			}
		}
	}
	if err := rebuildComboMembers(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate combo_members primary key: %w", err)
	}
	// idx_log_provider (provider_used alone) is superseded by the composite
	// (provider_used, ts DESC) index in schema.sql, which serves the same filter
	// AND the ORDER BY. Dropping it saves write amplification on every log insert.
	if _, err := db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_log_provider"); err != nil {
		slog.Warn("could not drop superseded log index", "err", err)
	}
	s := &Store{db: db}
	return s, nil
}

// rebuildComboMembers migrates combo_members to the current PRIMARY KEY
// (combo_id, position). Earlier schemas used (combo_id, provider_id) and then
// (combo_id, provider_id, model), neither of which can represent the same
// provider twice in one combo — and CREATE TABLE IF NOT EXISTS silently no-ops
// on an existing table, so the old PK survived every upgrade. A no-op when the
// PK already matches, which is the case for any database created after the
// account-pinning release.
func rebuildComboMembers(ctx context.Context, db *sql.DB) error {
	pk, err := primaryKeyColumns(ctx, db, "combo_members")
	if err != nil {
		return err
	}
	if len(pk) == 2 && pk[0] == "combo_id" && pk[1] == "position" {
		return nil
	}
	slog.Warn("migrating combo_members to the current primary key; this is a one-time upgrade of an older database", "old_primary_key", pk)

	// PRAGMA foreign_keys is a no-op inside a transaction and is per-connection,
	// so the whole rebuild has to run on ONE pinned connection with FKs disabled
	// around it — otherwise dropping the old table would cascade-delete rows.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	// Re-enable on the way out: this connection returns to the pool and would
	// otherwise serve later writes with FK enforcement silently disabled.
	defer func() {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			slog.Error("failed to re-enable foreign keys after migration", "err", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE combo_members_new (
			combo_id         TEXT NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
			provider_id      TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			account_id       TEXT REFERENCES provider_accounts(id) ON DELETE SET NULL,
			model            TEXT NOT NULL DEFAULT '',
			token_param_mode TEXT NOT NULL DEFAULT '',
			position         INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (combo_id, position)
		)`,
		// Positions are renumbered per combo: the old PK allowed duplicate
		// positions (they were not part of it), which would collide under the new one.
		`INSERT INTO combo_members_new (combo_id, provider_id, account_id, model, token_param_mode, position)
		 SELECT combo_id, provider_id, account_id, COALESCE(model, ''), COALESCE(token_param_mode, ''),
		        ROW_NUMBER() OVER (PARTITION BY combo_id ORDER BY position, provider_id) - 1
		 FROM combo_members`,
		`DROP TABLE combo_members`,
		`ALTER TABLE combo_members_new RENAME TO combo_members`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(stmt)[0], err)
		}
	}
	// Catch a rebuild that would leave dangling references (e.g. a member whose
	// provider was deleted while FKs were off in some earlier version) BEFORE
	// committing, rather than discovering it on the next write.
	var orphan string
	err = tx.QueryRowContext(ctx, `SELECT combo_id || '/' || provider_id FROM combo_members m
		WHERE NOT EXISTS (SELECT 1 FROM combos c WHERE c.id = m.combo_id)
		   OR NOT EXISTS (SELECT 1 FROM providers p WHERE p.id = m.provider_id) LIMIT 1`).Scan(&orphan)
	switch {
	case err == nil:
		return fmt.Errorf("refusing to commit: combo member %q references a missing combo/provider", orphan)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return tx.Commit()
}

// primaryKeyColumns returns a table's PRIMARY KEY columns in key order.
func primaryKeyColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying handle for tests.
func (s *Store) DB() *sql.DB { return s.db }

// --- Providers ---

func scanProvider(row interface{ Scan(...any) error }) (config.Provider, error) {
	var p config.Provider
	var tags string
	var enabled, native, proxyRotate int
	err := row.Scan(&p.ID, &p.Display, &p.BaseURL, &p.AuthKey, &p.Model, &p.Weight, &tags, &enabled, &native, &p.TokenParamMode, &proxyRotate)
	if err != nil {
		return config.Provider{}, err
	}
	p.Enabled = enabled != 0
	p.ResponsesNative = native != 0
	p.ProxyRotate = proxyRotate != 0
	if tags != "" {
		p.Tags = strings.Split(tags, ",")
	} else {
		p.Tags = nil
	}
	return p, nil
}

const providerCols = "id, display, base_url, auth_key, model, weight, tags, enabled, responses_native, token_param_mode, proxy_rotate"

func (s *Store) ListProviders() ([]config.Provider, error) {
	rows, err := s.db.Query("SELECT " + providerCols + " FROM providers ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Hydrate accounts + fetched model pool per provider in two batched queries
	// rather than N*N round trips.
	accounts, err := s.allProviderAccounts()
	if err != nil {
		return nil, err
	}
	models, err := s.allProviderModels()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Accounts = accounts[out[i].ID]
		out[i].Models = models[out[i].ID]
	}
	return out, nil
}

func (s *Store) GetProvider(id string) (*config.Provider, error) {
	row := s.db.QueryRow("SELECT "+providerCols+" FROM providers WHERE id = ?", id)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	accts, err := s.providerAccounts(id)
	if err != nil {
		return nil, err
	}
	models, err := s.providerModels(id)
	if err != nil {
		return nil, err
	}
	p.Accounts = accts
	p.Models = models
	return &p, nil
}

func (s *Store) UpsertProvider(p config.Provider) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertProviderTx(tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertProviderTx(tx *sql.Tx, p config.Provider) error {
	tags := strings.Join(p.Tags, ",")
	_, err := tx.Exec(`INSERT INTO providers
		(id, display, base_url, auth_key, model, weight, tags, enabled, responses_native, token_param_mode, proxy_rotate)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display=excluded.display, base_url=excluded.base_url, auth_key=excluded.auth_key,
			model=excluded.model, weight=excluded.weight, tags=excluded.tags,
			enabled=excluded.enabled, responses_native=excluded.responses_native,
			token_param_mode=excluded.token_param_mode, proxy_rotate=excluded.proxy_rotate`,
		p.ID, p.Display, p.BaseURL, p.AuthKey, p.Model, p.Weight, tags, boolToInt(p.Enabled), boolToInt(p.ResponsesNative), p.TokenParamMode, boolToInt(p.ProxyRotate))
	return err
}

// SaveProviderWithAccounts upserts the provider row and swaps its account pool
// in ONE transaction. Splitting the two (as the old create/update handlers did)
// meant a failure between them left a provider row with no accounts — an
// endpoint that 404s every request until someone notices.
func (s *Store) SaveProviderWithAccounts(p config.Provider, accounts []config.Account) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertProviderTx(tx, p); err != nil {
		return err
	}
	if err := replaceAccountsTx(tx, p.ID, accounts); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteProvider(id string) error {
	_, err := s.db.Exec("DELETE FROM providers WHERE id = ?", id)
	return err
}

// --- Provider accounts & models ---

const accountCols = "id, provider_id, label, auth_key, model, enabled, position, weight, token_param_mode"

func scanAccount(row interface{ Scan(...any) error }) (config.Account, error) {
	var a config.Account
	var enabled int
	err := row.Scan(&a.ID, &a.ProviderID, &a.Label, &a.AuthKey, &a.Model, &enabled, &a.Position, &a.Weight, &a.TokenParamMode)
	if err != nil {
		return config.Account{}, err
	}
	a.Enabled = enabled != 0
	return a, nil
}

func (s *Store) providerAccounts(providerID string) ([]config.Account, error) {
	rows, err := s.db.Query("SELECT "+accountCols+" FROM provider_accounts WHERE provider_id = ? ORDER BY position, id", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) allProviderAccounts() (map[string][]config.Account, error) {
	rows, err := s.db.Query("SELECT " + accountCols + " FROM provider_accounts ORDER BY provider_id, position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]config.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out[a.ProviderID] = append(out[a.ProviderID], a)
	}
	return out, rows.Err()
}

func (s *Store) providerModels(providerID string) ([]string, error) {
	rows, err := s.db.Query("SELECT model_id FROM provider_models WHERE provider_id = ? ORDER BY position, model_id", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) allProviderModels() (map[string][]string, error) {
	rows, err := s.db.Query("SELECT provider_id, model_id FROM provider_models ORDER BY provider_id, position, model_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var pid, m string
		if err := rows.Scan(&pid, &m); err != nil {
			return nil, err
		}
		out[pid] = append(out[pid], m)
	}
	return out, rows.Err()
}

// ReplaceAccounts swaps the account pool for a provider atomically. The swap is
// diff-based rather than delete-all: rows whose ID survives the edit are updated
// in place, so combo_members.account_id pins (ON DELETE SET NULL) are cleared
// only when a key is genuinely removed — a rename/disable/weight tweak keeps
// every pin intact.
func (s *Store) ReplaceAccounts(providerID string, accounts []config.Account) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceAccountsTx(tx, providerID, accounts); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceAccountsTx(tx *sql.Tx, providerID string, accounts []config.Account) error {
	keep := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		keep[a.ID] = true
	}
	rows, err := tx.Query("SELECT id FROM provider_accounts WHERE provider_id = ?", providerID)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan account: %w", err)
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.Exec("DELETE FROM provider_accounts WHERE id = ?", id); err != nil {
			return fmt.Errorf("delete account %q: %w", id, err)
		}
	}
	for i, a := range accounts {
		if _, err := tx.Exec(`INSERT INTO provider_accounts
			(id, provider_id, label, auth_key, model, enabled, position, weight, token_param_mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label = excluded.label, auth_key = excluded.auth_key, model = excluded.model,
				enabled = excluded.enabled, position = excluded.position, weight = excluded.weight,
				token_param_mode = excluded.token_param_mode`,
			a.ID, providerID, a.Label, a.AuthKey, a.Model, boolToInt(a.Enabled), i, max(a.Weight, 1), a.TokenParamMode); err != nil {
			return fmt.Errorf("upsert account: %w", err)
		}
	}
	return nil
}

// ReplaceModels swaps the fetched model pool for a provider atomically.
func (s *Store) ReplaceModels(providerID string, models []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM provider_models WHERE provider_id = ?", providerID); err != nil {
		return fmt.Errorf("clear models: %w", err)
	}
	for i, m := range models {
		if m == "" {
			continue
		}
		if _, err := tx.Exec("INSERT INTO provider_models (provider_id, model_id, position) VALUES (?, ?, ?)", providerID, m, i); err != nil {
			return fmt.Errorf("insert model: %w", err)
		}
	}
	return tx.Commit()
}

// --- Combos ---

func (s *Store) ListCombos() ([]config.Combo, error) {
	rows, err := s.db.Query("SELECT id, display_name, rotation, enabled, proxy_rotate FROM combos ORDER BY id")
	if err != nil {
		return nil, err
	}
	// Collect all rows and close the cursor BEFORE issuing nested member queries —
	// with a single pooled connection, a nested query while rows is open deadlocks.
	out := []config.Combo{}
	for rows.Next() {
		var c config.Combo
		var enabled, proxyRotate int
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.Rotation, &enabled, &proxyRotate); err != nil {
			rows.Close()
			return nil, err
		}
		c.Enabled = enabled != 0
		c.ProxyRotate = proxyRotate != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		members, err := s.listComboMembers(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Members = members
	}
	return out, nil
}

func (s *Store) GetCombo(id string) (*config.Combo, error) {
	row := s.db.QueryRow("SELECT id, display_name, rotation, enabled, proxy_rotate FROM combos WHERE id = ?", id)
	var c config.Combo
	var enabled, proxyRotate int
	err := row.Scan(&c.ID, &c.DisplayName, &c.Rotation, &enabled, &proxyRotate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Enabled = enabled != 0
	c.ProxyRotate = proxyRotate != 0
	members, err := s.listComboMembers(c.ID)
	if err != nil {
		return nil, err
	}
	c.Members = members
	return &c, nil
}

func (s *Store) listComboMembers(comboID string) ([]config.ComboMember, error) {
	rows, err := s.db.Query("SELECT provider_id, COALESCE(account_id, ''), model, token_param_mode FROM combo_members WHERE combo_id = ? ORDER BY position", comboID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.ComboMember{}
	for rows.Next() {
		var m config.ComboMember
		if err := rows.Scan(&m.ProviderID, &m.AccountID, &m.Model, &m.TokenParamMode); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpsertCombo(c config.Combo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO combos (id, display_name, rotation, enabled, proxy_rotate) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name, rotation=excluded.rotation,
			enabled=excluded.enabled, proxy_rotate=excluded.proxy_rotate`,
		c.ID, c.DisplayName, string(c.Rotation), boolToInt(c.Enabled), boolToInt(c.ProxyRotate))
	if err != nil {
		return fmt.Errorf("upsert combo: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM combo_members WHERE combo_id = ?", c.ID); err != nil {
		return fmt.Errorf("clear members: %w", err)
	}
	for i, m := range c.Members {
		// Empty pin must be written as NULL: '' is not a NULL for the FK to
		// provider_accounts(id) and would be rejected (no account has an empty ID).
		var accountID any
		if m.AccountID != "" {
			accountID = m.AccountID
		}
		if _, err := tx.Exec("INSERT INTO combo_members (combo_id, provider_id, account_id, model, token_param_mode, position) VALUES (?, ?, ?, ?, ?, ?)",
			c.ID, m.ProviderID, accountID, m.Model, m.TokenParamMode, i); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteCombo(id string) error {
	_, err := s.db.Exec("DELETE FROM combos WHERE id = ?", id)
	return err
}

// --- Proxy pool ---

// ListProxies returns the whole pool (enabled and disabled) in rotation order.
func (s *Store) ListProxies() ([]config.ProxyEntry, error) {
	rows, err := s.db.Query("SELECT id, label, url, enabled, position FROM proxies ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.ProxyEntry{}
	for rows.Next() {
		var p config.ProxyEntry
		var enabled int
		if err := rows.Scan(&p.ID, &p.Label, &p.URL, &enabled, &p.Position); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpsertProxy(p config.ProxyEntry) error {
	_, err := s.db.Exec(`INSERT INTO proxies (id, label, url, enabled, position) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET label=excluded.label, url=excluded.url,
			enabled=excluded.enabled, position=excluded.position`,
		p.ID, p.Label, p.URL, boolToInt(p.Enabled), p.Position)
	return err
}

func (s *Store) DeleteProxy(id string) error {
	_, err := s.db.Exec("DELETE FROM proxies WHERE id = ?", id)
	return err
}

// --- Request log ---

func (s *Store) LogRequest(e config.LogEntry) error {
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO request_log
		(ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, cached_tokens, error, upstream_url, request_payload, response_snippet, heal_note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp, e.ModelIn, e.ProviderUsed, e.Endpoint, e.Status, e.LatencyMs, e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.Error,
		e.UpstreamURL, e.RequestPayload, e.ResponseSnippet, e.HealNote)
	return err
}

func (s *Store) QueryLogs(f config.LogFilter) ([]config.LogEntry, error) {
	var where []string
	var args []any
	if f.ProviderID != "" {
		where = append(where, "provider_used = ?")
		args = append(args, f.ProviderID)
	}
	if f.Endpoint != "" {
		where = append(where, "endpoint = ?")
		args = append(args, f.Endpoint)
	}
	if f.ErrorsOnly {
		where = append(where, "(status >= 400 OR error != '' AND error IS NOT NULL)")
	}
	if f.Since > 0 {
		where = append(where, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, "ts < ?")
		args = append(args, f.Until)
	}
	q := "SELECT id, ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, cached_tokens, COALESCE(error,''), COALESCE(upstream_url,''), COALESCE(request_payload,''), COALESCE(response_snippet,''), COALESCE(heal_note,'') FROM request_log"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// Secondary id key keeps pagination deterministic when several rows share a
	// timestamp (clock-second resolution): without it OFFSET windows can repeat
	// rows.
	q += " ORDER BY ts DESC, id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	} else if f.Offset > 0 {
		// SQLite rejects OFFSET without LIMIT; -1 means "no limit".
		q += " LIMIT -1"
	}
	if f.Offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", f.Offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.LogEntry{}
	for rows.Next() {
		var e config.LogEntry
		var prompt, completion, cached sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ModelIn, &e.ProviderUsed, &e.Endpoint, &e.Status, &e.LatencyMs, &prompt, &completion, &cached, &e.Error, &e.UpstreamURL, &e.RequestPayload, &e.ResponseSnippet, &e.HealNote); err != nil {
			return nil, err
		}
		if prompt.Valid {
			v := int(prompt.Int64)
			e.PromptTokens = &v
		}
		if completion.Valid {
			v := int(completion.Int64)
			e.CompletionTokens = &v
		}
		if cached.Valid {
			v := int(cached.Int64)
			e.CachedTokens = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetLog returns a single log entry by ID, including full detail columns.
func (s *Store) GetLog(id int64) (*config.LogEntry, error) {
	var e config.LogEntry
	var prompt, completion, cached sql.NullInt64
	err := s.db.QueryRow(
		"SELECT id, ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, cached_tokens, COALESCE(error,''), COALESCE(upstream_url,''), COALESCE(request_payload,''), COALESCE(response_snippet,''), COALESCE(heal_note,'') FROM request_log WHERE id = ?",
		id,
	).Scan(&e.ID, &e.Timestamp, &e.ModelIn, &e.ProviderUsed, &e.Endpoint, &e.Status, &e.LatencyMs, &prompt, &completion, &cached, &e.Error, &e.UpstreamURL, &e.RequestPayload, &e.ResponseSnippet, &e.HealNote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if prompt.Valid {
		v := int(prompt.Int64)
		e.PromptTokens = &v
	}
	if completion.Valid {
		v := int(completion.Int64)
		e.CompletionTokens = &v
	}
	if cached.Valid {
		v := int(cached.Int64)
		e.CachedTokens = &v
	}
	return &e, nil
}

// ClearLogs deletes all request_log entries. Returns the number of deleted rows.
func (s *Store) ClearLogs() (int64, error) {
	res, err := s.db.Exec("DELETE FROM request_log")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountLogsToday returns the number of log entries since local midnight.
func (s *Store) CountLogsToday() (int64, error) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM request_log WHERE ts >= ?", midnight.Unix()).Scan(&n)
	return n, err
}

// CountLogs returns total matching rows for pagination.
func (s *Store) CountLogs(f config.LogFilter) (int64, error) {
	var where []string
	var args []any
	if f.ProviderID != "" {
		where = append(where, "provider_used = ?")
		args = append(args, f.ProviderID)
	}
	if f.Endpoint != "" {
		where = append(where, "endpoint = ?")
		args = append(args, f.Endpoint)
	}
	if f.ErrorsOnly {
		where = append(where, "(status >= 400 OR error != '' AND error IS NOT NULL)")
	}
	if f.Since > 0 {
		where = append(where, "ts >= ?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, "ts < ?")
		args = append(args, f.Until)
	}
	q := "SELECT COUNT(*) FROM request_log"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int64
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// RequestsPerHour returns grouped counts per hour for the last `hours` hours.
// bucket: unix hour; provider: provider id; count: rows in that bucket.
type HourlyCount struct {
	Bucket   int64  `json:"bucket"`
	Provider string `json:"provider"`
	Count    int64  `json:"count"`
}

func (s *Store) RequestsPerHour(hours int) ([]HourlyCount, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.db.Query(`
		SELECT (ts / 3600) * 3600 AS bucket, provider_used, COUNT(*)
		FROM request_log WHERE ts >= ? GROUP BY bucket, provider_used ORDER BY bucket`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HourlyCount{}
	for rows.Next() {
		var h HourlyCount
		if err := rows.Scan(&h.Bucket, &h.Provider, &h.Count); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// pruneBatchSize bounds one PruneLogs DELETE. An unbounded delete of a large
// backlog (e.g. the first run after lowering log.retention_days) held the write
// lock for over a second per 150k rows, stalling every concurrent log write and
// dashboard save; at multi-GB scale it can approach busy_timeout and start
// returning SQLITE_BUSY.
const pruneBatchSize = 5000

func (s *Store) PruneLogs(olderThanDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -olderThanDays).Unix()
	var total int64
	for {
		res, err := s.db.Exec(
			"DELETE FROM request_log WHERE id IN (SELECT id FROM request_log WHERE ts < ? LIMIT ?)",
			cutoff, pruneBatchSize)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatchSize {
			break
		}
		// Yield the write lock between batches so log writes and dashboard saves
		// interleave instead of queueing behind a long prune.
		time.Sleep(10 * time.Millisecond)
	}
	if total > 0 {
		// Deleted pages only return to SQLite's freelist, and passive WAL
		// checkpointing is skipped whenever a reader holds a snapshot (an open
		// dashboard inflates the WAL ~70x under load), so without this "pruning
		// worked" and "the volume is still full" stayed true simultaneously.
		if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			slog.Warn("wal checkpoint after prune failed", "err", err)
		}
		// Refresh planner statistics so the composite log indexes keep being chosen
		// as the table's shape changes.
		if _, err := s.db.Exec("ANALYZE request_log"); err != nil {
			slog.Warn("analyze after prune failed", "err", err)
		}
	}
	return total, nil
}

// --- Settings ---

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SetSettings writes several settings atomically. A per-key loop of SetSetting
// left earlier keys committed when a later one failed, and since Go map
// iteration order is random the surviving subset differed every attempt — while
// the caller reported "save failed" and the user believed nothing was written.
func (s *Store) SetSettings(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range kv {
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("set setting %q: %w", k, err)
		}
	}
	return tx.Commit()
}

func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
