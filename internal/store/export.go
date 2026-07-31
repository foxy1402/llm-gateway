package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const exportHeader = "-- LLM Gateway Export"

// ExportSQL renders the entire user-editable state as a plain-text SQL dump.
// The output is FK-safe for import: DELETEs run in dependency order, then INSERTs.
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

	var b strings.Builder
	b.WriteString(exportHeader + "\n")
	b.WriteString("-- Generated: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	b.WriteString("-- Version: 1\n\n")
	b.WriteString("BEGIN TRANSACTION;\n\n")

	// Delete in FK-safe order: child first.
	b.WriteString("DELETE FROM combo_members;\n")
	b.WriteString("DELETE FROM combos;\n")
	b.WriteString("DELETE FROM providers;\n")
	b.WriteString("DELETE FROM settings;\n\n")

	if len(provs) > 0 {
		b.WriteString("INSERT INTO providers (id, display, base_url, auth_key, model, weight, tags, enabled, responses_native) VALUES\n")
		for i, p := range provs {
			tags := strings.Join(p.Tags, ",")
			fmt.Fprintf(&b, "  (%s, %s, %s, %s, %s, %d, %s, %d, %d)",
				q(p.ID), q(p.Display), q(p.BaseURL), q(p.AuthKey), q(p.Model),
				p.Weight, q(tags), boolToInt(p.Enabled), boolToInt(p.ResponsesNative))
			if i == len(provs)-1 {
				b.WriteString(";\n\n")
			} else {
				b.WriteString(",\n")
			}
		}
	}
	if len(combos) > 0 {
		b.WriteString("INSERT INTO combos (id, display_name, rotation, enabled) VALUES\n")
		for i, c := range combos {
			fmt.Fprintf(&b, "  (%s, %s, %s, %d)", q(c.ID), q(c.DisplayName), q(string(c.Rotation)), boolToInt(c.Enabled))
			if i == len(combos)-1 {
				b.WriteString(";\n\n")
			} else {
				b.WriteString(",\n")
			}
		}
		// Members.
		first := true
		for _, c := range combos {
			for pos, pid := range c.Members {
				if first {
					b.WriteString("INSERT INTO combo_members (combo_id, provider_id, position) VALUES\n")
					first = false
				} else {
					b.WriteString(",\n")
				}
				fmt.Fprintf(&b, "  (%s, %s, %d)", q(c.ID), q(pid), pos)
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
			keys = append(keys, k)
		}
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range stmts {
		// Skip the wrapping BEGIN/COMMIT from the export; we manage our own tx.
		upper := strings.ToUpper(strings.TrimSpace(st))
		if upper == "BEGIN TRANSACTION" || upper == "COMMIT" {
			continue
		}
		if _, err := tx.Exec(st); err != nil {
			return fmt.Errorf("execute statement %q: %w", trunc(st, 60), err)
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
