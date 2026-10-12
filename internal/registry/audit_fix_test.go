package registry

import (
	"context"
	"path/filepath"
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/store"
)

// P3-8: an unbounded operator weight must not turn every routing decision into
// an allocation of sum(weights) slots. The cap bounds the pick list while still
// returning a valid, weighted account.
func TestNextAccountCapsHugeWeight(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveProviderWithAccounts(config.Provider{
		ID: "p", Display: "P", BaseURL: "https://api.example.com/v1",
		AuthKey: "k", Model: "m", Weight: 1, Enabled: true,
	}, []config.Account{
		{ID: "p:a", Label: "a", AuthKey: "k1", Enabled: true, Weight: 1_000_000_000},
		{ID: "p:b", Label: "b", AuthKey: "k2", Enabled: true, Weight: 1},
	}); err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.Reload(st); err != nil {
		t.Fatal(err)
	}
	p := r.GetProvider("p")
	if p == nil {
		t.Fatal("provider missing")
	}
	// Must not allocate ~1e9 slots per call; run enough iterations to catch a
	// pathological allocation via timeout/OOM if the cap regresses.
	for i := 0; i < 200; i++ {
		if _, ok := r.NextAccount(p, r.Health(), nil); !ok {
			t.Fatal("expected an eligible account")
		}
	}
}

// P2-3/P3-4: the default retryable set must include the account-scoped auth and
// transient codes (401/403/408/425/509), not just 402/429/5xx.
func TestDefaultRetryableCodesIncludeAuthAndTransient(t *testing.T) {
	h := NewHealthTracker()
	for _, code := range []int{401, 403, 408, 425, 509} {
		if !h.IsRetryable(code) {
			t.Fatalf("default retryable codes should include %d", code)
		}
	}
	// Must not over-rotate on codes that are genuinely terminal per-request.
	if h.IsRetryable(400) || h.IsRetryable(404) || h.IsRetryable(422) {
		t.Fatal("400/404/422 must not be retryable by default")
	}
}

// P3-3: repeated failures must back off (cooldown grows), not re-probe at a flat
// interval forever.
func TestCooldownBacksOffOnRepeatFailures(t *testing.T) {
	h := NewHealthTracker()
	h.Configure(60, []int{500})
	h.RecordFailure("p", 500)
	first := h.Snapshot()[0].CooldownRemainingMs
	h.RecordFailure("p", 500)
	second := h.Snapshot()[0].CooldownRemainingMs
	if second <= first {
		t.Fatalf("cooldown did not grow: first=%dms second=%dms", first, second)
	}
	if second > (30*60*1000)+(30*60*1000/4) {
		t.Fatalf("cooldown exceeded the cap (%dms)", second)
	}
}

// P2-6: a soft failure (a mere timeout) must not take a provider out for the
// full cooldown, and must not escalate the failure counter.
func TestSoftFailureBooksShortPenalty(t *testing.T) {
	h := NewHealthTracker()
	h.Configure(600, []int{500})
	h.RecordSoftFailure("p")
	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snap))
	}
	if snap[0].Failures != 0 {
		t.Fatalf("soft failure must not increment the failure counter, got %d", snap[0].Failures)
	}
	if snap[0].CooldownRemainingMs > 10*1000+100 {
		t.Fatalf("soft penalty too long: %dms (should be <= ~10s)", snap[0].CooldownRemainingMs)
	}
	if snap[0].CooldownRemainingMs == 0 {
		t.Fatal("soft failure should still book a short cooldown")
	}
}

// Improvements: cooldownSeconds == 0 disables cooldown entirely (an explicit
// operator choice), while a negative value leaves it unchanged.
func TestConfigureZeroDisablesCooldown(t *testing.T) {
	h := NewHealthTracker()
	h.Configure(0, nil)
	h.RecordFailure("p", 500)
	if !h.IsAvailable("p") {
		t.Fatal("cooldown=0 should disable cooldown, provider still unavailable")
	}

	h2 := NewHealthTracker()
	h2.Configure(60, nil)
	h2.Configure(-1, nil) // negative = leave unchanged
	h2.RecordFailure("p", 500)
	if h2.IsAvailable("p") {
		t.Fatal("a negative value must not disable the configured cooldown")
	}
}
