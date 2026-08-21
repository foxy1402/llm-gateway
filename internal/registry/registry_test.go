package registry

import (
	"context"
	"path/filepath"
	"testing"

	"llm-gateway/internal/store"
)

// TestReloadDefaultRetryableCodesInclude402 guards Reload's fallback (used when
// no "health.error_codes" setting has been saved yet) staying in lockstep with
// HealthTracker's own default — a fresh install and an install that never
// touched the setting must behave identically.
func TestReloadDefaultRetryableCodesInclude402(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{402, 429, 500, 502, 503, 504} {
		if !r.Health().IsRetryable(code) {
			t.Fatalf("Reload with no stored setting should keep %d retryable by default", code)
		}
	}
}
