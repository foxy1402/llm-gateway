package config

import (
	"testing"
	"time"
)

// P2-5: a non-positive REQUEST_TIMEOUT is a footgun (every non-streaming request
// would fail instantly and providers would get cooled), so it must be rejected
// at load rather than silently accepted.
func TestRequestTimeoutMustBePositive(t *testing.T) {
	setRequiredEnv(t)
	for _, bad := range []string{"0", "-1s", "0s"} {
		t.Setenv("REQUEST_TIMEOUT", bad)
		if _, err := LoadEnv(); err == nil {
			t.Fatalf("REQUEST_TIMEOUT=%q should be rejected", bad)
		}
	}
	t.Setenv("REQUEST_TIMEOUT", "30s")
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.RequestTimeout != 30*time.Second {
		t.Fatalf("RequestTimeout = %v, want 30s", e.RequestTimeout)
	}
}

// REQUEST_BUDGET / STREAM_STALL_TIMEOUT / ATTEMPT_BACKOFF_BASE parse into the Env
// and are ignored (fall back to proxy defaults) when invalid or non-positive.
func TestTuningEnvParsing(t *testing.T) {
	setRequiredEnv(t)

	t.Setenv("REQUEST_BUDGET", "5m")
	t.Setenv("STREAM_STALL_TIMEOUT", "3m")
	t.Setenv("ATTEMPT_BACKOFF_BASE", "250ms")
	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.RequestBudget != 5*time.Minute {
		t.Fatalf("RequestBudget = %v", e.RequestBudget)
	}
	if e.StreamStallTimeout != 3*time.Minute {
		t.Fatalf("StreamStallTimeout = %v", e.StreamStallTimeout)
	}
	if !e.AttemptBackoffSet || e.AttemptBackoffBase != 250*time.Millisecond {
		t.Fatalf("AttemptBackoffBase = %v set=%v", e.AttemptBackoffBase, e.AttemptBackoffSet)
	}

	// Explicit 0 disables backoff but is still "set".
	t.Setenv("ATTEMPT_BACKOFF_BASE", "0")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !e.AttemptBackoffSet || e.AttemptBackoffBase != 0 {
		t.Fatalf("explicit 0 should set backoff to 0: %v set=%v", e.AttemptBackoffBase, e.AttemptBackoffSet)
	}
}

// P2-13: DASHBOARD_SECURE_COOKIE is a standalone flag defaulting to TRUSTED_PROXY.
func TestDashboardSecureCookieFlag(t *testing.T) {
	setRequiredEnv(t)

	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.DashboardSecureCookie {
		t.Fatal("default should not mark cookies Secure")
	}

	t.Setenv("TRUSTED_PROXY", "1")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !e.DashboardSecureCookie {
		t.Fatal("TRUSTED_PROXY=1 should default the cookie flag on")
	}

	t.Setenv("DASHBOARD_SECURE_COOKIE", "0")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.DashboardSecureCookie {
		t.Fatal("DASHBOARD_SECURE_COOKIE=0 should override the TRUSTED_PROXY default")
	}
}
