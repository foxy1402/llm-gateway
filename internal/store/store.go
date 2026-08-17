package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
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
	// Migrations: add detail columns to existing databases (idempotent — safe to
	// re-run; SQLite errors on duplicate column are ignored).
	for _, col := range []string{"upstream_url TEXT DEFAULT ''", "request_payload TEXT DEFAULT ''", "response_snippet TEXT DEFAULT ''"} {
		colName := strings.Fields(col)[0]
		db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE request_log ADD COLUMN %s", col))
		_ = colName // silence unused lint; the DDL above uses colName via col split
	}
	s := &Store{db: db}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying handle for tests.
func (s *Store) DB() *sql.DB { return s.db }

// --- Providers ---

func scanProvider(row interface{ Scan(...any) error }) (config.Provider, error) {
	var p config.Provider
	var tags string
	var enabled, native int
	err := row.Scan(&p.ID, &p.Display, &p.BaseURL, &p.AuthKey, &p.Model, &p.Weight, &tags, &enabled, &native)
	if err != nil {
		return config.Provider{}, err
	}
	p.Enabled = enabled != 0
	p.ResponsesNative = native != 0
	if tags != "" {
		p.Tags = strings.Split(tags, ",")
	} else {
		p.Tags = nil
	}
	return p, nil
}

const providerCols = "id, display, base_url, auth_key, model, weight, tags, enabled, responses_native"

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
	tags := strings.Join(p.Tags, ",")
	_, err := s.db.Exec(`INSERT INTO providers
		(id, display, base_url, auth_key, model, weight, tags, enabled, responses_native)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display=excluded.display, base_url=excluded.base_url, auth_key=excluded.auth_key,
			model=excluded.model, weight=excluded.weight, tags=excluded.tags,
			enabled=excluded.enabled, responses_native=excluded.responses_native`,
		p.ID, p.Display, p.BaseURL, p.AuthKey, p.Model, p.Weight, tags, boolToInt(p.Enabled), boolToInt(p.ResponsesNative))
	return err
}

func (s *Store) DeleteProvider(id string) error {
	_, err := s.db.Exec("DELETE FROM providers WHERE id = ?", id)
	return err
}

// --- Provider accounts & models ---

const accountCols = "id, provider_id, label, auth_key, model, enabled, position, weight"

func scanAccount(row interface{ Scan(...any) error }) (config.Account, error) {
	var a config.Account
	var enabled int
	err := row.Scan(&a.ID, &a.ProviderID, &a.Label, &a.AuthKey, &a.Model, &enabled, &a.Position, &a.Weight)
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
			(id, provider_id, label, auth_key, model, enabled, position, weight)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label = excluded.label, auth_key = excluded.auth_key, model = excluded.model,
				enabled = excluded.enabled, position = excluded.position, weight = excluded.weight`,
			a.ID, providerID, a.Label, a.AuthKey, a.Model, boolToInt(a.Enabled), i, max(a.Weight, 1)); err != nil {
			return fmt.Errorf("upsert account: %w", err)
		}
	}
	return tx.Commit()
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
	rows, err := s.db.Query("SELECT id, display_name, rotation, enabled FROM combos ORDER BY id")
	if err != nil {
		return nil, err
	}
	// Collect all rows and close the cursor BEFORE issuing nested member queries —
	// with a single pooled connection, a nested query while rows is open deadlocks.
	out := []config.Combo{}
	for rows.Next() {
		var c config.Combo
		var enabled int
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.Rotation, &enabled); err != nil {
			rows.Close()
			return nil, err
		}
		c.Enabled = enabled != 0
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
	row := s.db.QueryRow("SELECT id, display_name, rotation, enabled FROM combos WHERE id = ?", id)
	var c config.Combo
	var enabled int
	err := row.Scan(&c.ID, &c.DisplayName, &c.Rotation, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Enabled = enabled != 0
	members, err := s.listComboMembers(c.ID)
	if err != nil {
		return nil, err
	}
	c.Members = members
	return &c, nil
}

func (s *Store) listComboMembers(comboID string) ([]config.ComboMember, error) {
	rows, err := s.db.Query("SELECT provider_id, COALESCE(account_id, ''), model FROM combo_members WHERE combo_id = ? ORDER BY position", comboID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.ComboMember{}
	for rows.Next() {
		var m config.ComboMember
		if err := rows.Scan(&m.ProviderID, &m.AccountID, &m.Model); err != nil {
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
	_, err = tx.Exec(`INSERT INTO combos (id, display_name, rotation, enabled) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name, rotation=excluded.rotation, enabled=excluded.enabled`,
		c.ID, c.DisplayName, string(c.Rotation), boolToInt(c.Enabled))
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
		if _, err := tx.Exec("INSERT INTO combo_members (combo_id, provider_id, account_id, model, position) VALUES (?, ?, ?, ?, ?)",
			c.ID, m.ProviderID, accountID, m.Model, i); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteCombo(id string) error {
	_, err := s.db.Exec("DELETE FROM combos WHERE id = ?", id)
	return err
}

// --- Request log ---

func (s *Store) LogRequest(e config.LogEntry) error {
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO request_log
		(ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, error, upstream_url, request_payload, response_snippet)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp, e.ModelIn, e.ProviderUsed, e.Endpoint, e.Status, e.LatencyMs, e.PromptTokens, e.CompletionTokens, e.Error,
		e.UpstreamURL, e.RequestPayload, e.ResponseSnippet)
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
	q := "SELECT id, ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, COALESCE(error,''), COALESCE(upstream_url,''), COALESCE(request_payload,''), COALESCE(response_snippet,'') FROM request_log"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ts DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
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
		var prompt, completion sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ModelIn, &e.ProviderUsed, &e.Endpoint, &e.Status, &e.LatencyMs, &prompt, &completion, &e.Error, &e.UpstreamURL, &e.RequestPayload, &e.ResponseSnippet); err != nil {
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
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetLog returns a single log entry by ID, including full detail columns.
func (s *Store) GetLog(id int64) (*config.LogEntry, error) {
	var e config.LogEntry
	var prompt, completion sql.NullInt64
	err := s.db.QueryRow(
		"SELECT id, ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, COALESCE(error,''), COALESCE(upstream_url,''), COALESCE(request_payload,''), COALESCE(response_snippet,'') FROM request_log WHERE id = ?",
		id,
	).Scan(&e.ID, &e.Timestamp, &e.ModelIn, &e.ProviderUsed, &e.Endpoint, &e.Status, &e.LatencyMs, &prompt, &completion, &e.Error, &e.UpstreamURL, &e.RequestPayload, &e.ResponseSnippet)
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

func (s *Store) PruneLogs(olderThanDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -olderThanDays).Unix()
	res, err := s.db.Exec("DELETE FROM request_log WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
