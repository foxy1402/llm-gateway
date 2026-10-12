package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const exportHeader = "-- LLM Gateway Export"

// ExportSQL renders the entire user-editable state as a plain-text SQL dump.
// The output is FK-safe for import: DELETEs run in dependency order, then INSERTs.
//
// Every functional column is covered. The one exception is created_at on
// providers/provider_accounts/combos/proxies: nothing in the gateway reads it, it
// is not carried on the config structs, and it defaults to unixepoch() on insert
// — so a restored backup shows the restore date rather than the original. Note
// the dump contains provider auth keys, account keys and proxy credentials in
// plaintext (a backup would be useless without them): treat the file as
// credential material.
func (s *Store) ExportSQL() (string, error) {
	provs, err := s.ListProviders()
	if err != nil {
		return "", err
	}
	combos, err := s.ListCombos()
	if err != nil {
		return "", err
	}
	settings, err := s.AllSettings()
	if err != nil {
		return "", err
	}
	proxies, err := s.ListProxies()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(exportHeader + "\n")
	b.WriteString("-- Generated: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	b.WriteString("-- Version: 1\n\n")
	b.WriteString("BEGIN TRANSACTION;\n\n")

	// Delete in FK-safe order: child first.
	b.WriteString("DELETE FROM combo_members;\n")
	b.WriteString("DELETE FROM combos;\n")
	b.WriteString("DELETE FROM provider_models;\n")
	b.WriteString("DELETE FROM provider_accounts;\n")
	b.WriteString("DELETE FROM providers;\n")
	b.WriteString("DELETE FROM proxies;\n")
	b.WriteString("DELETE FROM settings;\n\n")

	if len(provs) > 0 {
		b.WriteString("INSERT INTO providers (id, display, base_url, auth_key, model, weight, tags, enabled, responses_native, token_param_mode, proxy_rotate) VALUES\n")
		for i, p := range provs {
			tags := strings.Join(p.Tags, ",")
			fmt.Fprintf(&b, "  (%s, %s, %s, %s, %s, %d, %s, %d, %d, %s, %d)",
				q(p.ID), q(p.Display), q(p.BaseURL), q(p.AuthKey), q(p.Model),
				p.Weight, q(tags), boolToInt(p.Enabled), boolToInt(p.ResponsesNative), q(p.TokenParamMode), boolToInt(p.ProxyRotate))
			if i == len(provs)-1 {
				b.WriteString(";\n\n")
			} else {
				b.WriteString(",\n")
			}
		}

		// Accounts & fetched model pool.
		first := true
		for _, p := range provs {
			for pos, a := range p.Accounts {
				if first {
					b.WriteString("INSERT INTO provider_accounts (id, provider_id, label, auth_key, model, enabled, position, weight, token_param_mode) VALUES\n")
					first = false
				} else {
					b.WriteString(",\n")
				}
				fmt.Fprintf(&b, "  (%s, %s, %s, %s, %s, %d, %d, %d, %s)",
					q(a.ID), q(p.ID), q(a.Label), q(a.AuthKey), q(a.Model),
					boolToInt(a.Enabled), pos, max(a.Weight, 1), q(a.TokenParamMode))
			}
		}
		if !first {
			b.WriteString(";\n\n")
		}
		first = true
		for _, p := range provs {
			for pos, m := range p.Models {
				if m == "" {
					continue
				}
				if first {
					b.WriteString("INSERT INTO provider_models (provider_id, model_id, position) VALUES\n")
					first = false
				} else {
					b.WriteString(",\n")
				}
				fmt.Fprintf(&b, "  (%s, %s, %d)", q(p.ID), q(m), pos)
			}
		}
		if !first {
			b.WriteString(";\n\n")
		}
	}
	if len(proxies) > 0 {
		b.WriteString("INSERT INTO proxies (id, label, url, enabled, position) VALUES\n")
		for i, px := range proxies {
			fmt.Fprintf(&b, "  (%s, %s, %s, %d, %d)", q(px.ID), q(px.Label), q(px.URL), boolToInt(px.Enabled), px.Position)
			if i == len(proxies)-1 {
				b.WriteString(";\n\n")
			} else {
				b.WriteString(",\n")
			}
		}
	}
	if len(combos) > 0 {
		b.WriteString("INSERT INTO combos (id, display_name, rotation, enabled, proxy_rotate) VALUES\n")
		for i, c := range combos {
			fmt.Fprintf(&b, "  (%s, %s, %s, %d, %d)", q(c.ID), q(c.DisplayName), q(string(c.Rotation)), boolToInt(c.Enabled), boolToInt(c.ProxyRotate))
			if i == len(combos)-1 {
				b.WriteString(";\n\n")
			} else {
				b.WriteString(",\n")
			}
		}
		// Members with per-member account + model pins. Empty account pin exports as
		// NULL (the in-DB representation of "rotate across keys").
		first := true
		for _, c := range combos {
			for pos, m := range c.Members {
				if first {
					b.WriteString("INSERT INTO combo_members (combo_id, provider_id, account_id, model, token_param_mode, position) VALUES\n")
					first = false
				} else {
					b.WriteString(",\n")
				}
				acct := "NULL"
				if m.AccountID != "" {
					acct = q(m.AccountID)
				}
				fmt.Fprintf(&b, "  (%s, %s, %s, %s, %s, %d)", q(c.ID), q(m.ProviderID), acct, q(m.Model), q(m.TokenParamMode), pos)
			}
		}
		if !first {
			b.WriteString(";\n\n")
		}
	}
	if len(settings) > 0 {
		// Stable order.
		keys := make([]string, 0, len(settings))
		for k := range settings {
			// Reserved runtime state (failban:state) is deliberately NOT exported:
			// it is host-specific brute-force bookkeeping, not configuration, and
			// including it let a restore silently reset bans.
			if IsReservedSetting(k) {
				continue
			}
			keys = append(keys, k)
		}
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		if len(keys) > 0 {
			b.WriteString("INSERT INTO settings (key, value) VALUES\n")
			for i, k := range keys {
				fmt.Fprintf(&b, "  (%s, %s)", q(k), q(settings[k]))
				if i == len(keys)-1 {
					b.WriteString(";\n\n")
				} else {
					b.WriteString(",\n")
				}
			}
		}
	}

	b.WriteString("COMMIT;\n")
	return b.String(), nil
}

// q SQL-escapes a string literal.
func q(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ImportError is returned for malformed import files.
type ImportError struct{ Reason string }

func (e *ImportError) Error() string { return "invalid import: " + e.Reason }

// IsValidation reports whether err is a user-fixable validation problem (400).
func IsValidation(err error) bool {
	var ie *ImportError
	return errors.As(err, &ie)
}

// ImportSQL applies an export file inside a single transaction, replacing all
// providers, combos, combo_members and settings. The file must start with the
// gateway export header.
func (s *Store) ImportSQL(sqlText string) error {
	if !strings.HasPrefix(strings.TrimSpace(sqlText), exportHeader) {
		return &ImportError{Reason: "missing gateway export header"}
	}
	stmts := splitStatements(sqlText)
	if len(stmts) == 0 {
		return &ImportError{Reason: "no statements found"}
	}
	// A truncated export — a download cut short, a partial copy/paste — is still
	// syntactically valid input: the file opens with the header and the seven
	// unconditional DELETE FROM statements, so importing one wiped every provider,
	// key, combo and setting and then reported SUCCESS. Require the INSERTs that
	// are supposed to restore what those DELETEs remove, and require that every
	// table being cleared is also being repopulated.
	deleted := map[string]bool{}
	inserted := map[string]bool{}
	for _, st := range stmts {
		fields := strings.Fields(st)
		if len(fields) < 3 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "DELETE":
			if strings.EqualFold(fields[1], "FROM") {
				deleted[strings.ToLower(fields[2])] = true
			}
		case "INSERT":
			if strings.EqualFold(fields[1], "INTO") {
				inserted[strings.ToLower(strings.SplitN(fields[2], "(", 2)[0])] = true
			}
		}
	}
	if len(inserted) == 0 {
		return &ImportError{Reason: "export contains no INSERT statements — the file looks truncated; refusing to clear the existing configuration"}
	}
	// providers is the anchor table: an export that clears it without restoring it
	// cannot be a complete backup, whatever else the file contains.
	if deleted["providers"] && !inserted["providers"] {
		return &ImportError{Reason: "export clears providers but restores none — the file looks truncated; refusing to import"}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, st := range stmts {
		// Skip the wrapping BEGIN/COMMIT from the export; we manage our own tx.
		upper := strings.ToUpper(strings.TrimSpace(st))
		if upper == "BEGIN TRANSACTION" || upper == "COMMIT" {
			continue
		}
		// The import runs with the caller's authority but the file contents are
		// untrusted text: an export that smuggles in DROP/ALTER/ATTACH/PRAGMA or
		// touches request_log must not execute. Only the exact statements
		// ExportSQL itself emits are allowed.
		if err := validateImportStatement(st); err != nil {
			return err
		}
		// Settings are subject to the same reservation as the dashboard write
		// allowlist: an import must not carry reserved runtime keys
		// (failban:state) that would silently reset brute-force protections on the
		// next restart. Reserved rows are SKIPPED rather than failing the whole
		// import, so an older export that predates this filter still restores
		// everything else.
		st, skipped := stripReservedSettings(st)
		if skipped && st == "" {
			continue
		}
		if _, err := tx.Exec(st); err != nil {
			// st can be an INSERT row carrying provider/account auth keys verbatim.
			// Surface only the statement KIND and index server-side; the caller
			// (the uploading admin) gets a generic message, and the raw statement is
			// never echoed into the HTTP response body.
			slog.Warn("import statement failed", "kind", strings.Fields(st)[0], "err", err)
			return fmt.Errorf("execute import statement %d (%s): %w", i, strings.ToLower(strings.Fields(st)[0]), err)
		}
	}
	return tx.Commit()
}

// stripReservedSettings removes reserved-key rows from an
// "INSERT INTO settings (key, value) VALUES (...), (...)" statement. Returns the
// (possibly rewritten) statement and whether anything was dropped. A statement
// that is not a settings INSERT, or whose shape can't be parsed, is returned
// unchanged with skipped=false — the import validator has already constrained
// the shape, and failing to parse simply means the row passes through
// (reserved keys are additionally never written by this version's export).
func stripReservedSettings(stmt string) (string, bool) {
	fields := strings.Fields(stmt)
	if len(fields) < 6 || !strings.EqualFold(fields[0], "INSERT") ||
		!strings.EqualFold(fields[1], "INTO") || !strings.EqualFold(fields[2], "settings") {
		return stmt, false
	}
	open := strings.Index(stmt, "(")
	closeParen := strings.Index(stmt, ")")
	if open < 0 || closeParen < open {
		return stmt, false
	}
	cols := strings.Split(stmt[open+1:closeParen], ",")
	keyIdx := -1
	for i, c := range cols {
		if strings.EqualFold(strings.TrimSpace(c), "key") {
			keyIdx = i
			break
		}
	}
	if keyIdx < 0 {
		return stmt, false
	}
	valuesAt := strings.Index(strings.ToUpper(stmt), "VALUES")
	if valuesAt < 0 {
		return stmt, false
	}
	head := stmt[:valuesAt] + "VALUES"
	tuples, ok := splitValueTuples(stmt[valuesAt+len("VALUES"):])
	if !ok {
		return stmt, false
	}
	kept := tuples[:0]
	dropped := false
	for _, t := range tuples {
		vals, ok := splitTupleValues(t)
		if !ok || keyIdx >= len(vals) {
			return stmt, false // unparseable → leave the whole statement alone
		}
		if IsReservedSetting(unquoteSQL(vals[keyIdx])) {
			dropped = true
			continue
		}
		kept = append(kept, t)
	}
	if !dropped {
		return stmt, false
	}
	if len(kept) == 0 {
		return "", true
	}
	return head + "\n  " + strings.Join(kept, ",\n  "), true
}

// splitValueTuples splits " (a,b), (c,d);" into ["(a,b)", "(c,d)"], respecting
// single-quoted strings. Returns ok=false on any shape surprise.
func splitValueTuples(s string) ([]string, bool) {
	var out []string
	depth := 0
	inString := false
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++ // escaped ''
					continue
				}
				inString = false
			}
			continue
		}
		switch c {
		case '\'':
			inString = true
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, strings.TrimSpace(s[start:i+1]))
				start = -1
			} else if depth < 0 {
				return nil, false
			}
		}
	}
	if depth != 0 || inString || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// splitTupleValues splits one "(a, b, c)" tuple into its top-level comma-separated
// values, respecting quoted strings and nested parens.
func splitTupleValues(t string) ([]string, bool) {
	t = strings.TrimSpace(t)
	if len(t) < 2 || t[0] != '(' || t[len(t)-1] != ')' {
		return nil, false
	}
	inner := t[1 : len(t)-1]
	var out []string
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inString {
			if c == '\'' {
				if i+1 < len(inner) && inner[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
			continue
		}
		switch c {
		case '\'':
			inString = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(inner[start:]))
	return out, true
}

// unquoteSQL strips surrounding single quotes and unescapes doubled single
// quotes from a SQL string literal; a non-literal (NULL, number) is returned
// as-is.
func unquoteSQL(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// importAllowedTables are the tables an export file may write. Notably absent:
// request_log (audit data, never user-editable) and anything schema-level.
var importAllowedTables = map[string]bool{
	"providers":         true,
	"provider_accounts": true,
	"provider_models":   true,
	"combos":            true,
	"combo_members":     true,
	"proxies":           true,
	"settings":          true,
}

// validateImportStatement accepts only the two statement shapes ExportSQL emits:
// "DELETE FROM <table>" (whole-table replace, no WHERE) and
// "INSERT INTO <table> (...) VALUES ...". Everything else — UPDATE, DROP,
// ATTACH, PRAGMA, writes to request_log, … — is rejected.
func validateImportStatement(st string) error {
	fields := strings.Fields(st)
	if len(fields) == 0 {
		return &ImportError{Reason: "empty statement"}
	}
	switch strings.ToUpper(fields[0]) {
	case "DELETE":
		if len(fields) != 3 || !strings.EqualFold(fields[1], "FROM") || !importAllowedTables[strings.ToLower(fields[2])] {
			return &ImportError{Reason: "only whole-table DELETE FROM <allowed table> is permitted, got: " + trunc(st, 60)}
		}
	case "INSERT":
		if len(fields) < 4 || !strings.EqualFold(fields[1], "INTO") || !importAllowedTables[strings.ToLower(fields[2])] {
			return &ImportError{Reason: "only INSERT INTO <allowed table> is permitted, got: " + trunc(st, 60)}
		}
	default:
		return &ImportError{Reason: "disallowed statement kind: " + trunc(st, 60)}
	}
	return nil
}

// FinalizeImport synchronizes derived tables after an import: for any provider row
// lacking provider_accounts (a v1 legacy dump), synthesize a default account from
// auth_key; likewise seed provider_models from provider.model if the fetched pool
// came in empty. This keeps imports self-healing without needing the provider row
// to carry account/model info itself.
func (s *Store) FinalizeImport(ctx context.Context) error {
	provRows, err := s.db.QueryContext(ctx, `SELECT id, auth_key, model FROM providers`)
	if err != nil {
		return err
	}
	type row struct{ id, key, model string }
	var rows []row
	for provRows.Next() {
		var r row
		if err := provRows.Scan(&r.id, &r.key, &r.model); err != nil {
			provRows.Close()
			return err
		}
		rows = append(rows, r)
	}
	provRows.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_accounts WHERE provider_id = ?`, r.id).Scan(&n); err != nil {
			return err
		}
		if n == 0 && r.key != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_accounts (id, provider_id, label, auth_key, enabled, position, weight)
				 VALUES (?, ?, ?, ?, 1, 0, 1)`,
				r.id+":default", r.id, "default", r.key); err != nil {
				return fmt.Errorf("synthesize account for %q: %w", r.id, err)
			}
		}
		var m int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_models WHERE provider_id = ?`, r.id).Scan(&m); err != nil {
			return err
		}
		if m == 0 && r.model != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO provider_models (provider_id, model_id, position) VALUES (?, ?, 0)`,
				r.id, r.model); err != nil {
				return fmt.Errorf("synthesize model for %q: %w", r.id, err)
			}
		}
	}
	return tx.Commit()
}

// splitStatements splits a SQL script on semicolons, respecting single-quoted strings.
func splitStatements(sqlText string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	runes := []rune(sqlText)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if inString {
			cur.WriteRune(c)
			if c == '\'' {
				// Handle escaped ''.
				if i+1 < len(runes) && runes[i+1] == '\'' {
					cur.WriteRune(runes[i+1])
					i++
				} else {
					inString = false
				}
			}
			continue
		}
		// Line-comment: skip to newline.
		if c == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		switch c {
		case '\'':
			inString = true
			cur.WriteRune(c)
		case ';':
			stmt := strings.TrimSpace(cur.String())
			if stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	// Trailing statement without semicolon.
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
