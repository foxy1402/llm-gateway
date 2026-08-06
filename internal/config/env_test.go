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
