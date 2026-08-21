package config

import "testing"

func TestNormalizeListen(t *testing.T) {
	cases := map[string]string{
		"8080":           ":8080",
		":8080":          ":8080",
		"0.0.0.0:8080":   "0.0.0.0:8080",
		"localhost:9000": "localhost:9000",
		"[::]:8080":      "[::]:8080",
		"":               ":8080",
		"  3000  ":       ":3000",
	}
	for in, want := range cases {
		if got := normalizeListen(in); got != want {
			t.Errorf("normalizeListen(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMaxRequestBodyMB: default must comfortably cover base64 vision/OCR
// payloads (the old hardcoded 4MiB proxy limit rejected those); an explicit
// override must apply, and a nonsense/zero override must fall back to the
// default rather than disabling the guard (0 would make every request fail).
func TestMaxRequestBodyMB(t *testing.T) {
	setRequiredEnv(t)

	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxRequestBodyMB != 25 {
		t.Fatalf("default MaxRequestBodyMB = %d, want 25", e.MaxRequestBodyMB)
	}

	t.Setenv("MAX_REQUEST_BODY_MB", "50")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxRequestBodyMB != 50 {
		t.Fatalf("override MaxRequestBodyMB = %d, want 50", e.MaxRequestBodyMB)
	}

	t.Setenv("MAX_REQUEST_BODY_MB", "0")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxRequestBodyMB != 25 {
		t.Fatalf("MAX_REQUEST_BODY_MB=0 must fall back to default, got %d", e.MaxRequestBodyMB)
	}
}

// TestMaxAccountAttemptsPerProvider: default must be generous enough to try a
// realistic key pool in full; an explicit override applies, and a zero/invalid
// value falls back to the default rather than disabling self-heal rotation.
func TestMaxAccountAttemptsPerProvider(t *testing.T) {
	setRequiredEnv(t)

	e, err := LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxAccountAttemptsPerProvider != 10 {
		t.Fatalf("default MaxAccountAttemptsPerProvider = %d, want 10", e.MaxAccountAttemptsPerProvider)
	}

	t.Setenv("MAX_ACCOUNT_ATTEMPTS_PER_PROVIDER", "20")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxAccountAttemptsPerProvider != 20 {
		t.Fatalf("override MaxAccountAttemptsPerProvider = %d, want 20", e.MaxAccountAttemptsPerProvider)
	}

	t.Setenv("MAX_ACCOUNT_ATTEMPTS_PER_PROVIDER", "0")
	e, err = LoadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.MaxAccountAttemptsPerProvider != 10 {
		t.Fatalf("MAX_ACCOUNT_ATTEMPTS_PER_PROVIDER=0 must fall back to default, got %d", e.MaxAccountAttemptsPerProvider)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_API_KEY", "gw-test-key")
	t.Setenv("DASHBOARD_PASSWORD", "test-password")
	t.Setenv("DASHBOARD_SECRET", "01234567890123456789012345678901")
}

func TestParseAliases(t *testing.T) {
	m, err := parseAliases("vercel=vip-combo , ironclaw=vercel")
	if err != nil {
		t.Fatal(err)
	}
	if m["vercel"] != "vip-combo" || m["ironclaw"] != "vercel" {
		t.Fatalf("aliases: %v", m)
	}
	for _, bad := range []string{"vercel", "vercel=", "=combo", "vercel=combo,,", "  "} {
		if _, err := parseAliases(bad); err == nil {
			t.Fatalf("invalid entry %q must fail", bad)
		}
	}
}
