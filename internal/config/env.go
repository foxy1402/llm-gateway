package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Env struct {
	APIKey            string
	DashboardPassword string
	DashboardSecret   string
	Listen            string
	LogLevel          string
	DBPath            string
	RequestTimeout    time.Duration
	ModelAliases      map[string]string
}

func LoadEnv() (*Env, error) {
	e := &Env{
		APIKey:            os.Getenv("GATEWAY_API_KEY"),
		DashboardPassword: os.Getenv("DASHBOARD_PASSWORD"),
		DashboardSecret:   os.Getenv("DASHBOARD_SECRET"),
		Listen:            normalizeListen(getEnv("GATEWAY_LISTEN", ":8080")),
		LogLevel:          getEnv("GATEWAY_LOG_LEVEL", "info"),
		DBPath:            getEnv("DB_PATH", "./gateway.db"),
	}
	if e.APIKey == "" {
		return nil, fmt.Errorf("GATEWAY_API_KEY is required")
	}
	if e.DashboardPassword == "" {
		return nil, fmt.Errorf("DASHBOARD_PASSWORD is required")
	}
	if e.DashboardSecret == "" {
		return nil, fmt.Errorf("DASHBOARD_SECRET is required (min 32 chars)")
	}
	if len(e.DashboardSecret) < 32 {
		return nil, fmt.Errorf("DASHBOARD_SECRET must be at least 32 characters")
	}
	if t := os.Getenv("REQUEST_TIMEOUT"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, fmt.Errorf("REQUEST_TIMEOUT invalid: %w", err)
		}
		e.RequestTimeout = d
	} else {
		e.RequestTimeout = 60 * time.Second
	}
	// MODEL_ALIASES: comma-separated incoming=target pairs. When a client sends a
	// model name that matches neither a provider nor a combo (common with agents
	// that default the model to their provider's name, e.g. "vercel"), the left-hand
	// name routes to the combo/provider on the right. Exact names always win —
	// aliases are a fallback only.
	if raw := os.Getenv("MODEL_ALIASES"); raw != "" {
		m, err := parseAliases(raw)
		if err != nil {
			return nil, err
		}
		e.ModelAliases = m
	}
	return e, nil
}

// parseAliases parses "a=x, b=y" into map[a]x, map[b]y, validating every pair.
func parseAliases(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		lhs, rhs, found := strings.Cut(pair, "=")
		lhs, rhs = strings.TrimSpace(lhs), strings.TrimSpace(rhs)
		if !found || lhs == "" || rhs == "" {
			return nil, fmt.Errorf("MODEL_ALIASES entry %q must be incoming=target", strings.TrimSpace(pair))
		}
		out[lhs] = rhs
	}
	return out, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// normalizeListen coerces the various forms users put in GATEWAY_LISTEN into a
// valid net.Listen "host:port" address. Some PaaS hosts reject a leading ":" or
// expect a bare port number. Accepted inputs:
//
//	"8080"          → ":8080"      (bare port)
//	":8080"         → ":8080"      (already valid)
//	"0.0.0.0:8080"  → "0.0.0.0:8080"
//	"localhost:9000"→ "localhost:9000"
//	"[::]:8080"     → unchanged    (IPv6 with port)
func normalizeListen(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ":8080"
	}
	// Bare port number (no colon anywhere).
	if !strings.Contains(s, ":") {
		return ":" + s
	}
	// Anything containing a colon is passed through — covers IPv6, host:port, :port.
	return s
}

// SlogLevel maps GATEWAY_LOG_LEVEL to a slog level.
func (e *Env) SlogLevel() int {
	switch e.LogLevel {
	case "debug":
		return -4 // slog.LevelDebug
	case "warn":
		return 4 // slog.LevelWarn
	case "error":
		return 8 // slog.LevelError
	default:
		return 0 // slog.LevelInfo
	}
}
