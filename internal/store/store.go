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
	return &Store{db: db}, nil
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
	return out, rows.Err()
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

func (s *Store) listComboMembers(comboID string) ([]string, error) {
	rows, err := s.db.Query("SELECT provider_id FROM combo_members WHERE combo_id = ? ORDER BY position", comboID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
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
	for i, pid := range c.Members {
		if _, err := tx.Exec("INSERT INTO combo_members (combo_id, provider_id, position) VALUES (?, ?, ?)", c.ID, pid, i); err != nil {
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
		(ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp, e.ModelIn, e.ProviderUsed, e.Endpoint, e.Status, e.LatencyMs, e.PromptTokens, e.CompletionTokens, e.Error)
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
	q := "SELECT id, ts, model_in, provider_used, endpoint, status, latency_ms, prompt_tokens, completion_tokens, COALESCE(error,'') FROM request_log"
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
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.ModelIn, &e.ProviderUsed, &e.Endpoint, &e.Status, &e.LatencyMs, &prompt, &completion, &e.Error); err != nil {
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
