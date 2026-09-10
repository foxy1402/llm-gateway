package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
)

func storeWithConfig(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveProviderWithAccounts(config.Provider{
		ID: "p1", Display: "P1", BaseURL: "https://api.example.com/v1",
		AuthKey: "sk-real-key", Model: "m", Weight: 1, Enabled: true,
	}, []config.Account{{ID: "p1:k1", Label: "k1", AuthKey: "sk-acct", Enabled: true, Weight: 1}}); err != nil {
		t.Fatalf("save provider: %v", err)
	}
	if err := st.UpsertCombo(config.Combo{
		ID: "c1", DisplayName: "C1", Rotation: config.RoundRobin, Enabled: true,
		Members: []config.ComboMember{{ProviderID: "p1"}},
	}); err != nil {
		t.Fatalf("save combo: %v", err)
	}
	if err := st.SetSetting("health.cooldown", "60"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	return st
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// A truncated export file (download cut short, partial paste) is still valid
// input: it opens with the header and the unconditional DELETEs. Importing one
// used to wipe every provider/key/combo/setting and report success.
func TestImportRejectsTruncatedExport(t *testing.T) {
	st := storeWithConfig(t)
	full, err := st.ExportSQL()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	cut := strings.Index(full, "INSERT INTO")
	if cut < 0 {
		t.Fatal("export has no INSERT statements to cut at")
	}
	truncated := full[:cut]

	if err := st.ImportSQL(truncated); err == nil {
		t.Fatal("truncated export was accepted")
	} else if !IsValidation(err) {
		t.Fatalf("want a validation error, got %v", err)
	}
	if n := countRows(t, st, "providers"); n != 1 {
		t.Fatalf("providers wiped by rejected import: %d", n)
	}
	if n := countRows(t, st, "provider_accounts"); n != 1 {
		t.Fatalf("accounts wiped by rejected import: %d", n)
	}
	if n := countRows(t, st, "combos"); n != 1 {
		t.Fatalf("combos wiped by rejected import: %d", n)
	}
	if n := countRows(t, st, "settings"); n == 0 {
		t.Fatal("settings wiped by rejected import")
	}
}

// Header + a single DELETE is the minimal destructive file.
func TestImportRejectsDeleteOnlyFile(t *testing.T) {
	st := storeWithConfig(t)
	if err := st.ImportSQL(exportHeader + "\nDELETE FROM providers;\n"); err == nil {
		t.Fatal("delete-only file was accepted")
	}
	if n := countRows(t, st, "providers"); n != 1 {
		t.Fatalf("providers wiped: %d", n)
	}
}

// The guard must not reject a genuine export.
func TestImportAcceptsFullExportRoundTrip(t *testing.T) {
	st := storeWithConfig(t)
	full, err := st.ExportSQL()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := st.ImportSQL(full); err != nil {
		t.Fatalf("full export rejected: %v", err)
	}
	provs, err := st.ListProviders()
	if err != nil {
		t.Fatalf("list after import: %v", err)
	}
	if len(provs) != 1 || provs[0].AuthKey != "sk-real-key" {
		t.Fatalf("round-trip lost data: %+v", provs)
	}
	if len(provs[0].Accounts) != 1 || provs[0].Accounts[0].AuthKey != "sk-acct" {
		t.Fatalf("round-trip lost accounts: %+v", provs[0].Accounts)
	}
}

// Offset without an explicit limit produced invalid SQL (SQLite rejects a bare
// OFFSET), so any caller that paginated without setting Limit got a syntax error.
func TestQueryLogsOffsetWithoutLimit(t *testing.T) {
	st := storeWithConfig(t)
	for i := 0; i < 5; i++ {
		st.LogRequest(config.LogEntry{ModelIn: "m", ProviderUsed: "p1", Endpoint: "chat.completions", Status: 200})
	}
	rows, err := st.QueryLogs(config.LogFilter{Offset: 2})
	if err != nil {
		t.Fatalf("offset without limit: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows after skipping 2, got %d", len(rows))
	}
}

// SetSettings must be all-or-nothing: the previous per-key loop left an
// unpredictable subset committed when a later key failed.
func TestSetSettingsIsAtomic(t *testing.T) {
	st := storeWithConfig(t)
	if err := st.SetSettings(map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("set settings: %v", err)
	}
	all, err := st.AllSettings()
	if err != nil {
		t.Fatalf("all settings: %v", err)
	}
	if all["a"] != "1" || all["b"] != "2" {
		t.Fatalf("settings not written: %+v", all)
	}
}

// Pruning must delete everything past the cutoff even when the backlog exceeds
// one batch, and must not touch newer rows.
func TestPruneLogsChunksPastBatchSize(t *testing.T) {
	st := storeWithConfig(t)
	old := time.Now().AddDate(0, 0, -10).Unix()
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < pruneBatchSize+50; i++ {
		if _, err := tx.Exec(
			`INSERT INTO request_log (ts, model_in, provider_used, endpoint, status, latency_ms) VALUES (?,?,?,?,?,?)`,
			old, "m", "p1", "chat.completions", 200, 1); err != nil {
			t.Fatalf("insert old row: %v", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO request_log (ts, model_in, provider_used, endpoint, status, latency_ms) VALUES (?,?,?,?,?,?)`,
		time.Now().Unix(), "m", "p1", "chat.completions", 200, 1); err != nil {
		t.Fatalf("insert fresh row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := st.PruneLogs(7)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != int64(pruneBatchSize+50) {
		t.Fatalf("want %d pruned, got %d", pruneBatchSize+50, n)
	}
	if remaining := countRows(t, st, "request_log"); remaining != 1 {
		t.Fatalf("want the fresh row kept, got %d rows", remaining)
	}
}
