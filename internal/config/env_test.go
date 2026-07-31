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
