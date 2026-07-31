package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Env struct {
	APIKey           string
	DashboardPassword string
	DashboardSecret  string
	Listen           string
	LogLevel         string
	DBPath           string
	RequestTimeout   time.Duration
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
	return e, nil
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
